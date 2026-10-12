// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package migrations owns the tracker's schema: an embedded golang-migrate
// track recorded in its own table (tracker_schema_migrations), separate from
// any host application's track. See design §8.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var sqlFS embed.FS

// Table is the golang-migrate version table of the tracker track.
const Table = "tracker_schema_migrations"

// BaselineVersion is the squashed baseline (0001) that Adopt stamps.
const BaselineVersion = 1

// advisoryClass is the tracker's advisory-lock class id ("FTKR"); objid 2 is
// reserved for schema adoption (see design §11.2).
const (
	advisoryClass = 0x46544b52
	advisoryAdopt = 2
)

// baselineTables are the tables migration 0001 creates. Adopt refuses to
// stamp a database where any is missing.
var baselineTables = []string{
	"fraud_typologies", "fraud_agencies", "fraud_regions",
	"fraud_profession_categories", "fraud_sectors",
	"fraud_persons", "fraud_organisations", "fraud_cases",
	"fraud_case_typologies", "fraud_case_agencies", "fraud_case_participants",
	"fraud_case_organisations", "fraud_sources", "fraud_case_events",
	"fraud_claims", "fraud_claim_reviews", "fraud_case_match_candidates",
	"fraud_report_snapshots", "fraud_appeal_checks",
}

// Migrate applies every pending tracker migration to dsn (postgres:// or
// postgresql:// URL). Run Adopt first on a database whose fraud tables were
// created by a host application's own migrations.
func Migrate(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply tracker migrations: %w", err)
	}
	return nil
}

// MigrateTo moves dsn to exactly version (up or down). Used by the bundle
// importer, which loads data at the bundle's schema version before running
// the remaining migrations, and by migration tests.
func MigrateTo(dsn string, version uint) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate tracker schema to %d: %w", version, err)
	}
	return nil
}

// Version reports the tracker schema version of dsn (0, false when the
// track has never run).
func Version(dsn string) (uint, bool, error) {
	m, err := newMigrator(dsn)
	if err != nil {
		return 0, false, err
	}
	defer m.Close()
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if dirty {
		return v, true, fmt.Errorf("tracker schema version %d is dirty", v)
	}
	return v, true, nil
}

func newMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(sqlFS, "sql")
	if err != nil {
		return nil, fmt.Errorf("load tracker migrations: %w", err)
	}
	target, err := migrateURL(dsn)
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return nil, fmt.Errorf("init tracker migrator: %w", err)
	}
	return m, nil
}

// migrateURL rewrites a postgres URL for the pgx5 driver and points it at
// the tracker's version table.
func migrateURL(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return "", fmt.Errorf("database url must be postgres:// or postgresql://, got %q", u.Scheme)
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("x-migrations-table", Table)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// AdoptOptions lets the host add its own preconditions before an existing
// fraud schema is stamped as the tracker baseline (e.g. "the host's own
// migration track is at the version that last changed fraud tables").
type AdoptOptions struct {
	HostCheck func(ctx context.Context, tx pgx.Tx) error
}

// Adopt stamps the baseline as applied on a database whose fraud tables
// already exist (created by a host application's migrations), without
// executing it. It is a no-op when the tracker track already exists or the
// fraud tables don't (a fresh database: Migrate creates them). It never
// changes data, and refuses — rather than guesses — when the existing
// schema is not the one the baseline describes.
func Adopt(ctx context.Context, pool *pgxpool.Pool, opts AdoptOptions) (adopted bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, advisoryClass, advisoryAdopt); err != nil {
		return false, fmt.Errorf("adopt lock: %w", err)
	}
	var hasTrack, hasCases bool
	if err := tx.QueryRow(ctx, `
		SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL,
		       to_regclass(format('%I.fraud_cases', current_schema())) IS NOT NULL`,
		Table).Scan(&hasTrack, &hasCases); err != nil {
		return false, err
	}
	if hasTrack || !hasCases {
		return false, nil
	}

	if err := fingerprint(ctx, tx); err != nil {
		return false, fmt.Errorf("refusing to adopt existing fraud schema: %w", err)
	}
	if opts.HostCheck != nil {
		if err := opts.HostCheck(ctx, tx); err != nil {
			return false, fmt.Errorf("refusing to adopt existing fraud schema: %w", err)
		}
	}
	// Same layout golang-migrate's pgx driver creates, so Migrate picks up
	// from version 1.
	if _, err := tx.Exec(ctx, `CREATE TABLE `+pgx.Identifier{Table}.Sanitize()+
		` (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`); err != nil {
		return false, fmt.Errorf("create %s: %w", Table, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{Table}.Sanitize()+
		` (version, dirty) VALUES ($1, false)`, BaselineVersion); err != nil {
		return false, fmt.Errorf("stamp baseline: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// fingerprint checks the existing schema is the baseline's: every baseline
// table, the newest baseline column (fraud_cases.status_overridden, last
// fraud DDL before the track split), the lag view and the append-only
// review guard.
func fingerprint(ctx context.Context, tx pgx.Tx) error {
	var missing []string
	for _, t := range baselineTables {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL`, t).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			missing = append(missing, "table "+t)
		}
	}
	var hasCol, hasView, hasGuard bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_schema = current_schema()
		                  AND table_name = 'fraud_cases' AND column_name = 'status_overridden'),
		       to_regclass(format('%I.fraud_case_lags', current_schema())) IS NOT NULL,
		       EXISTS (SELECT 1 FROM pg_trigger
		                WHERE tgname = 'fraud_claim_reviews_no_update'
		                  AND tgrelid = to_regclass(format('%I.fraud_claim_reviews', current_schema())))`).
		Scan(&hasCol, &hasView, &hasGuard); err != nil {
		return err
	}
	if !hasCol {
		missing = append(missing, "column fraud_cases.status_overridden")
	}
	if !hasView {
		missing = append(missing, "view fraud_case_lags")
	}
	if !hasGuard {
		missing = append(missing, "trigger fraud_claim_reviews_no_update")
	}
	if len(missing) > 0 {
		return fmt.Errorf("not the tracker baseline schema; missing: %s", strings.Join(missing, ", "))
	}
	// Objects from later tracker migrations mean the schema is already past
	// the baseline and only the version table went missing: stamping v1
	// would make Migrate re-run those migrations.
	var pastBaseline bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass(format('%I.tracker_actors', current_schema())) IS NOT NULL`).Scan(&pastBaseline); err != nil {
		return err
	}
	if pastBaseline {
		return fmt.Errorf("schema is already past the tracker baseline (tracker_actors exists) but %s is missing; restore the version table instead of adopting", Table)
	}
	return nil
}
