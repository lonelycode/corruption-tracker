// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package storetest gives tracker tests a real Postgres to run against
// without any host application: each Open creates a private schema on the
// test database, applies the tracker's migration track to it, and drops it
// when the test ends. Tests never touch pre-existing objects, so packages
// can run in parallel against one test database.
//
// The test database comes from DATABASE_URL_TEST:
//   - unset: the test is skipped (no-DB workflow stays green);
//   - set but unreachable, rejected or unmarked: the test FAILS — setting it
//     is an explicit request for DB coverage, so it must not skip silently.
//
// The database must be marked as a throwaway test database: its comment
// must end in "test-db" (e.g. COMMENT ON DATABASE x IS 'tracker-test-db').
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/migrations"
	"github.com/lonelycode/corruption-tracker/store"
)

// MarkerSuffix is what the test database's comment must end with.
const MarkerSuffix = "test-db"

// SeededTables hold reference data the baseline migration seeds (the
// taxonomies). TruncateAll leaves them alone.
var SeededTables = []string{
	"fraud_typologies", "fraud_agencies", "fraud_regions",
	"fraud_profession_categories", "fraud_sectors",
}

// Open returns a Store over a fresh, fully migrated private schema.
func Open(t testing.TB) *store.Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set; skipping tracker DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = admin.Ping(ctx)
	}
	if err != nil {
		t.Fatalf("storetest: DATABASE_URL_TEST is set but unusable: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := assertMarked(ctx, admin); err != nil {
		t.Fatal(err)
	}
	// pg_trgm must live in public, visible from every test schema; created
	// inside one test schema it would vanish with it under other tests.
	if _, err := admin.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public`); err != nil {
		// Parallel packages can race the CREATE; losing the race is fine.
		var ok bool
		if qerr := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`).Scan(&ok); qerr != nil || !ok {
			t.Fatalf("storetest: ensure pg_trgm: %v", err)
		}
	}

	schema := "storetest_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("storetest: create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Logf("storetest: drop schema %s: %v", schema, err)
		}
	})

	scoped, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Migrate(scoped); err != nil {
		t.Fatalf("storetest: migrate tracker schema: %v", err)
	}
	pool, err := pgxpool.New(ctx, scoped)
	if err != nil {
		t.Fatalf("storetest: connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// TruncateAll empties every tracker table in the pool's current schema
// except the seeded reference tables and the migration version table. For
// hosts whose own test harness shares a schema with the tracker. It works
// on a frozen or handed-off instance too: the truncate bypasses the freeze
// write guard (tracker_instance goes with it, so the next test starts in
// normal mode).
func TruncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := TruncateAllTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TruncateAllTx is TruncateAll inside the caller's transaction, so a host
// harness can empty its own tables and the tracker's in one go. It sets the
// guard bypass (corruption_tracker.allow_frozen_write) for that
// transaction, which also covers the host's own TRUNCATE ... CASCADE if it
// reaches tracker tables.
func TruncateAllTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET LOCAL corruption_tracker.allow_frozen_write = 'on'`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT tablename FROM pg_tables
		 WHERE schemaname = current_schema()
		   AND (tablename LIKE 'fraud\_%' OR tablename LIKE 'tracker\_%')
		   AND tablename <> $1
		   AND NOT (tablename = ANY($2))
		 ORDER BY tablename`, migrations.Table, SeededTables)
	if err != nil {
		return fmt.Errorf("list tracker tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, pgx.Identifier{n}.Sanitize())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `TRUNCATE `+strings.Join(names, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("truncate tracker tables: %w", err)
	}
	return nil
}

func assertMarked(ctx context.Context, pool *pgxpool.Pool) error {
	var comment *string
	if err := pool.QueryRow(ctx, `
		SELECT shobj_description(d.oid, 'pg_database')
		  FROM pg_database d WHERE d.datname = current_database()`).Scan(&comment); err != nil {
		return fmt.Errorf("storetest: read DB marker: %w", err)
	}
	if comment == nil || !strings.HasSuffix(*comment, MarkerSuffix) {
		seen := "<no comment>"
		if comment != nil {
			seen = *comment
		}
		return fmt.Errorf("storetest: the DATABASE_URL_TEST database is not marked as a throwaway test DB "+
			"(comment %q must end in %q).\n\nFix: COMMENT ON DATABASE <dbname> IS 'tracker-%s';\n"+
			"Never mark a dev or production database.", seen, MarkerSuffix, MarkerSuffix)
	}
	return nil
}

func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("storetest: parse DATABASE_URL_TEST: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func randomSuffix(t testing.TB) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
