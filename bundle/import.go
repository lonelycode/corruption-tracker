// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/migrations"
)

// ImportOptions configure one import (design
// §10.6).
type ImportOptions struct {
	// DSN is the target database. It is needed when the target must be
	// migrated (to the bundle's schema version, then to the latest) and for
	// the --replace pg_dump; pool must point at the same database.
	DSN string
	// Replace allows importing over existing data. Before anything changes
	// it writes a pg_dump of the target and a bundle of its current dataset
	// into BackupDir.
	Replace   bool
	BackupDir string
	// PGDump is the pg_dump binary (default: "pg_dump" on PATH).
	PGDump string
	// Settings stores the bundle's settings.json (and secrets.enc) on the
	// target. Required when the bundle carries settings.
	Settings SettingsTarget
	// Passphrase opens secrets.enc; without it the import proceeds without
	// secrets and the report says so. A wrong one refuses the import.
	Passphrase string
	Log        *slog.Logger
	Now        func() time.Time
}

// SettingsTarget is the target instance's settings store as the importer
// needs it (settings.Service implements it). If it also has
// Reload(ctx) error, ImportFile calls that after commit.
type SettingsTarget interface {
	// SettingKind reports whether name is a setting the target knows and
	// whether it is secret.
	SettingKind(name string) (known, secret bool)
	// SealSecret encrypts a secret setting under the target's instance key.
	SealSecret(name, plaintext string) ([]byte, error)
}

// TableReport is one line of the verification report.
type TableReport struct {
	Name   string
	Rows   int64
	SHA256 string
	OK     bool
}

// ImportReport is what an import loaded and proved.
type ImportReport struct {
	ExportID       string
	SourceInstance string
	SchemaVersion  uint // the bundle's; the target is migrated to the latest afterwards
	Tables         []TableReport
	Backups        []string // files written before a --replace
	// Settings imported into tracker_settings, and the bundle's settings
	// this version doesn't know (skipped).
	SettingsImported int
	SecretsImported  int
	SettingsSkipped  []string
	// SecretsSkipped are the secrets.enc keys this version doesn't know as
	// secrets (also listed in SettingsSkipped): secrets were present, but
	// none of these could be restored.
	SecretsSkipped []string
	// SecretsNotImported explains why secrets.enc was not imported ("" when
	// it was, or the bundle had none).
	SecretsNotImported string
}

// settingRow is one tracker_settings row to write.
type settingRow struct {
	key, value string
	ciphertext []byte // set for secrets
}

// replaceableOnEmpty are tables a fresh instance fills by itself (taxonomy
// seeds, the scheduler's first runs, connector toggles). They don't make a
// target "non-empty"; the bundle's rows replace them.
var replaceableOnEmpty = []string{
	"fraud_typologies", "fraud_agencies", "fraud_regions", "fraud_profession_categories", "fraud_sectors",
	"tracker_job_runs", "tracker_connector_state",
}

const loadBatch = 1000

// ImportFile restores a bundle into the tracker database pool points at.
//
// Nothing is written until the whole bundle has verified (manifest, entry
// set, checksums, row counts), the target has been checked (empty, or
// --replace with backups taken; schema version not newer than the bundle's)
// and the table shapes match the manifest. The load then runs in one
// transaction, in the bundle's FK-safe order with IDs preserved, resets the
// sequences, and re-exports every loaded table inside the transaction: any
// byte that differs from the bundle rolls everything back. Afterwards the
// schema is migrated to the latest version and the instance is marked as
// imported, with ingestion paused.
func ImportFile(ctx context.Context, pool *pgxpool.Pool, path string, opts ImportOptions) (ImportReport, error) {
	var rep ImportReport
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	// 1. The bundle, offline.
	m, err := VerifyFile(path)
	if err != nil {
		return rep, fmt.Errorf("bundle rejected, nothing imported: %w", err)
	}
	rep.ExportID, rep.SourceInstance, rep.SchemaVersion = m.ExportID, m.Source.InstanceID, m.SchemaVersion
	stage, err := os.MkdirTemp("", "corruption-tracker-import-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(stage)
	if err := extract(path, stage, m); err != nil {
		return rep, fmt.Errorf("bundle rejected, nothing imported: %w", err)
	}
	settingRows, err := prepareSettings(stage, m, opts, &rep)
	if err != nil {
		return rep, err
	}

	// 2. The target.
	var hostTrack bool
	// Only the tracker's own schema counts: a test schema on a shared
	// database may see a host's tables further down the search_path.
	if err := pool.QueryRow(ctx, `SELECT to_regclass(quote_ident(current_schema()) || '.schema_migrations') IS NOT NULL`).Scan(&hostTrack); err != nil {
		return rep, err
	}
	if hostTrack {
		return rep, errors.New("refusing to import into a database with a host application's schema (schema_migrations exists); " +
			"import into a standalone tracker database")
	}
	latest, err := migrations.Latest()
	if err != nil {
		return rep, err
	}
	if m.SchemaVersion > latest {
		return rep, fmt.Errorf("bundle schema version %d is newer than this binary's (%d); upgrade corruption-tracker first",
			m.SchemaVersion, latest)
	}
	current, err := trackerVersion(ctx, pool)
	if err != nil {
		return rep, err
	}
	if (current != m.SchemaVersion || m.SchemaVersion != latest) && opts.DSN == "" {
		return rep, errors.New("this import needs schema migrations; pass the target DSN")
	}
	nonEmpty, err := nonEmptyTables(ctx, pool, m)
	if err != nil {
		return rep, err
	}
	if len(nonEmpty) > 0 && !opts.Replace {
		return rep, fmt.Errorf("target is not empty (%s); refusing to import without --replace", strings.Join(nonEmpty, ", "))
	}
	if opts.Replace {
		if current > m.SchemaVersion {
			return rep, fmt.Errorf("--replace needs the target at or below the bundle's schema version %d (target is at %d)",
				m.SchemaVersion, current)
		}
		if rep.Backups, err = backup(ctx, pool, opts); err != nil {
			return rep, fmt.Errorf("pre-import backup failed, nothing changed: %w", err)
		}
	}

	// 3. Schema at exactly the bundle's version, and matching its shapes.
	if current != m.SchemaVersion {
		if err := migrations.MigrateTo(opts.DSN, m.SchemaVersion); err != nil {
			return rep, err
		}
	}
	if err := checkShapes(ctx, pool, m); err != nil {
		return rep, err
	}

	// 4. Load, verify by re-export, commit.
	if rep.Tables, err = load(ctx, pool, stage, m, opts.Replace, settingRows); err != nil {
		return rep, err
	}
	if r, ok := opts.Settings.(interface{ Reload(context.Context) error }); ok && len(settingRows) > 0 {
		if err := r.Reload(ctx); err != nil {
			opts.Log.Warn("import: reload settings", "err", err)
		}
	}

	// 5. Upgrade the imported data and mark the instance.
	if m.SchemaVersion != latest {
		if err := migrations.Migrate(opts.DSN); err != nil {
			return rep, fmt.Errorf("data imported at schema %d, but migrating to %d failed: %w", m.SchemaVersion, latest, err)
		}
	}
	if err := markImported(ctx, pool, m); err != nil {
		return rep, err
	}
	opts.Log.Info("tracker import done", "export_id", m.ExportID, "schema_version", m.SchemaVersion,
		"tables", len(rep.Tables), "replace", opts.Replace)
	return rep, nil
}

// extract writes the bundle's settings and table files into stage,
// re-checking every checksum (the file may have changed since Verify).
func extract(path, stage string, m Manifest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	want := map[string]string{SettingsFile: m.Settings.SHA256}
	for _, t := range m.Tables {
		want[t.File] = t.SHA256
	}
	if m.Secrets != nil {
		want[m.Secrets.File] = m.Secrets.SHA256
	}
	if err := os.Mkdir(filepath.Join(stage, "tables"), 0o700); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		sum, ok := want[hdr.Name]
		if !ok {
			continue // manifest.json
		}
		out, err := os.OpenFile(filepath.Join(stage, hdr.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(out, h), tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != sum {
			return fmt.Errorf("%s changed since verification", hdr.Name)
		}
		delete(want, hdr.Name)
	}
	for name := range want {
		return fmt.Errorf("%s missing", name)
	}
	return nil
}

func trackerVersion(ctx context.Context, pool *pgxpool.Pool) (uint, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, migrations.Table).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v uint
	var dirty bool
	err := pool.QueryRow(ctx, `SELECT version, dirty FROM `+pgx.Identifier{migrations.Table}.Sanitize()).Scan(&v, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if dirty {
		return v, fmt.Errorf("target tracker schema version %d is dirty; fix it before importing", v)
	}
	return v, nil
}

// nonEmptyTables lists the bundle's dataset tables that hold rows on the
// target, ignoring the ones a fresh instance fills by itself.
func nonEmptyTables(ctx context.Context, pool *pgxpool.Pool, m Manifest) ([]string, error) {
	var out []string
	names := append(slices.Clone(TableSet), AccessLogTable)
	for _, name := range names {
		if slices.Contains(replaceableOnEmpty, name) {
			continue
		}
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if name == AccessLogTable && !m.Options.IncludeAccessLog {
			continue // the target's own access log stays
		}
		var any bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{name}.Sanitize()+`)`).Scan(&any); err != nil {
			return nil, err
		}
		if any {
			out = append(out, name)
		}
	}
	return out, nil
}

// backup writes a pg_dump of the target database and a bundle of its
// current dataset into opts.BackupDir.
func backup(ctx context.Context, pool *pgxpool.Pool, opts ImportOptions) ([]string, error) {
	if opts.BackupDir == "" {
		return nil, errors.New("--replace needs a backup directory")
	}
	if opts.DSN == "" {
		return nil, errors.New("--replace needs the target DSN for pg_dump")
	}
	if err := os.MkdirAll(opts.BackupDir, 0o700); err != nil {
		return nil, err
	}
	stamp := opts.Now().UTC().Format("20060102T150405Z")
	bin := opts.PGDump
	if bin == "" {
		bin = "pg_dump"
	}
	dump := filepath.Join(opts.BackupDir, "pre-import-"+stamp+".dump")
	cmd := exec.CommandContext(ctx, bin, "--format=custom", "--file="+dump, "--dbname="+opts.DSN)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dump) // never leave a partial dump that looks like a backup
		return nil, fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	files := []string{dump}

	var id string
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT id::text FROM tracker_instance LIMIT 1), '')`).Scan(&id); err != nil {
		id = "" // no tracker_instance at this schema version
	}
	if id == "" {
		id = NewID()
	}
	path, _, err := Export(ctx, pool, opts.BackupDir, ExportOptions{
		Source: Source{InstanceID: id, Kind: KindStandalone}, AppVersion: "pre-import backup",
		IncludeAccessLog: true, Now: opts.Now, Log: opts.Log,
	})
	if err != nil {
		return files, fmt.Errorf("backup bundle: %w", err)
	}
	return append(files, path), nil
}

// checkShapes compares every bundle table's columns with the target's.
func checkShapes(ctx context.Context, pool *pgxpool.Pool, m Manifest) error {
	for _, t := range m.Tables {
		rows, err := pool.Query(ctx, `
			SELECT attname, format_type(atttypid, atttypmod) FROM pg_attribute
			 WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped ORDER BY attnum`, t.Name)
		if err != nil {
			return err
		}
		var got []Column
		for rows.Next() {
			var c Column
			if err := rows.Scan(&c.Name, &c.Type); err != nil {
				rows.Close()
				return err
			}
			got = append(got, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !slices.Equal(got, t.Columns) {
			return fmt.Errorf("table %s on the target doesn't match the bundle (target %v, bundle %v)", t.Name, got, t.Columns)
		}
	}
	return nil
}

// load inserts every table in one transaction and proves the result by
// re-exporting it before commit.
func load(ctx context.Context, pool *pgxpool.Pool, stage string, m Manifest, replace bool, settingRows []settingRow) ([]TableReport, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The freeze guard lets an import write.
	if _, err := tx.Exec(ctx, `SET LOCAL corruption_tracker.allow_frozen_write = 'on'`); err != nil {
		return nil, err
	}

	var names []string
	for _, t := range m.Tables {
		names = append(names, pgx.Identifier{t.Name}.Sanitize())
	}
	if replace {
		// TRUNCATE skips the row-level append-only guards; CASCADE takes the
		// accounts and sessions that hang off tracker_actors with it (they
		// are in the pg_dump backup).
		if _, err := tx.Exec(ctx, `TRUNCATE `+strings.Join(names, ", ")+` CASCADE`); err != nil {
			return nil, fmt.Errorf("clear target: %w", err)
		}
	} else {
		// Only the self-filled tables hold rows; children before parents.
		for i := len(m.Tables) - 1; i >= 0; i-- {
			if slices.Contains(replaceableOnEmpty, m.Tables[i].Name) {
				if _, err := tx.Exec(ctx, `DELETE FROM `+names[i]); err != nil {
					return nil, fmt.Errorf("clear %s: %w", m.Tables[i].Name, err)
				}
			}
		}
	}

	for i, t := range m.Tables {
		if err := loadTable(ctx, tx, filepath.Join(stage, t.File), names[i]); err != nil {
			return nil, fmt.Errorf("load %s: %w", t.Name, err)
		}
		if t.MaxID != nil {
			if _, err := tx.Exec(ctx, `
				SELECT setval(pg_get_serial_sequence($1, a.attname), $2)
				  FROM pg_attribute a
				 WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped
				   AND pg_get_serial_sequence($1, a.attname) IS NOT NULL`, t.Name, *t.MaxID); err != nil {
				return nil, fmt.Errorf("reset %s sequence: %w", t.Name, err)
			}
		}
	}

	// The bundle's settings win; settings only the target has stay.
	for _, sr := range settingRows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tracker_settings (key, value, secret, ciphertext, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, NULL, now())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, secret = EXCLUDED.secret,
			       ciphertext = EXCLUDED.ciphertext, updated_by = NULL, updated_at = now()`,
			sr.key, sr.value, sr.ciphertext != nil, sr.ciphertext); err != nil {
			return nil, fmt.Errorf("import setting %s: %w", sr.key, err)
		}
	}

	// Re-export inside the transaction with the exporter's own settings and
	// query: identical bytes prove the load was lossless.
	for _, s := range []string{
		`SET LOCAL TimeZone = 'UTC'`, `SET LOCAL DateStyle = 'ISO, YMD'`,
		`SET LOCAL IntervalStyle = 'postgres'`, `SET LOCAL extra_float_digits = 1`,
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			return nil, err
		}
	}
	check, err := os.MkdirTemp("", "corruption-tracker-import-check-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(check)
	if err := os.Mkdir(filepath.Join(check, "tables"), 0o700); err != nil {
		return nil, err
	}
	var report []TableReport
	var mismatch []string
	for _, t := range m.Tables {
		e, err := exportTable(ctx, tx, check, t.Name)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", t.Name, err)
		}
		ok := e.SHA256 == t.SHA256 && e.Rows == t.Rows
		report = append(report, TableReport{Name: t.Name, Rows: e.Rows, SHA256: e.SHA256, OK: ok})
		if !ok {
			mismatch = append(mismatch, t.Name)
		}
	}
	if len(mismatch) > 0 {
		return report, fmt.Errorf("imported data does not re-export identically (%s); rolled back", strings.Join(mismatch, ", "))
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	return report, nil
}

func loadTable(ctx context.Context, tx pgx.Tx, file, table string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	q := `INSERT INTO ` + table + ` OVERRIDING SYSTEM VALUE SELECT * FROM json_populate_recordset(NULL::` + table + `, $1::json)`
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20) // snapshot payloads can be large
	var batch []json.RawMessage
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		arr, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		batch = batch[:0]
		_, err = tx.Exec(ctx, q, string(arr))
		return err
	}
	for sc.Scan() {
		batch = append(batch, json.RawMessage(slices.Clone(sc.Bytes())))
		if len(batch) == loadBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// markImported records the bundle on tracker_instance and pauses
// ingestion, so nothing runs until an operator has checked the import and
// the handoff checklist (§11.5).
func markImported(ctx context.Context, pool *pgxpool.Pool, m Manifest) error {
	from := map[string]any{"instance_id": m.Source.InstanceID, "export_id": m.ExportID, "freeze_id": nil}
	if m.Handoff != nil {
		from["freeze_id"] = m.Handoff.FreezeID
		from["frozen_at"] = m.Handoff.FrozenAt // connectors catch up from here on resume (§11.4)
	}
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tracker_instance DEFAULT VALUES ON CONFLICT DO NOTHING`); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tracker_instance SET imported_from = $1::jsonb, ingestion_paused = true, updated_at = now()`,
		string(raw)); err != nil {
		return fmt.Errorf("mark instance imported: %w", err)
	}
	return nil
}

// prepareSettings decides, before anything is written, which of the
// bundle's settings and secrets go into tracker_settings, decrypting and
// re-sealing secrets under the target's key.
func prepareSettings(stage string, m Manifest, opts ImportOptions, rep *ImportReport) ([]settingRow, error) {
	raw, err := os.ReadFile(filepath.Join(stage, SettingsFile))
	if err != nil {
		return nil, err
	}
	var plain map[string]string
	if err := json.Unmarshal(raw, &plain); err != nil {
		return nil, fmt.Errorf("bundle rejected, nothing imported: settings.json: %w", err)
	}
	if (len(plain) > 0 || m.Secrets != nil) && opts.Settings == nil {
		return nil, errors.New("the bundle carries settings, but no settings store was given to import them into")
	}
	var rows []settingRow
	for _, k := range sortedKeys(plain) {
		known, secret := opts.Settings.SettingKind(k)
		switch {
		case !known:
			rep.SettingsSkipped = append(rep.SettingsSkipped, k)
			opts.Log.Warn("import: skipping a setting this version doesn't know", "setting", k)
		case secret:
			return nil, fmt.Errorf("bundle rejected, nothing imported: settings.json carries %s, which is a secret setting, in plaintext", k)
		default:
			rows = append(rows, settingRow{key: k, value: plain[k]})
		}
	}
	rep.SettingsImported = len(rows)

	if m.Secrets == nil {
		return rows, nil
	}
	if opts.Passphrase == "" {
		rep.SecretsNotImported = "the bundle has secrets.enc but no passphrase was given (" + PassphraseEnv + "); re-enter the secret settings on the Settings page"
		opts.Log.Warn("import: " + rep.SecretsNotImported)
		return rows, nil
	}
	enc, err := os.ReadFile(filepath.Join(stage, m.Secrets.File))
	if err != nil {
		return nil, err
	}
	secrets, err := OpenSecrets(enc, opts.Passphrase)
	if err != nil {
		return nil, fmt.Errorf("nothing imported: %w", err)
	}
	for _, k := range sortedKeys(secrets) {
		known, secret := opts.Settings.SettingKind(k)
		if !known || !secret {
			rep.SettingsSkipped = append(rep.SettingsSkipped, k)
			rep.SecretsSkipped = append(rep.SecretsSkipped, k)
			opts.Log.Warn("import: skipping a bundled secret this version doesn't know as a secret", "setting", k)
			continue
		}
		ct, err := opts.Settings.SealSecret(k, secrets[k])
		if err != nil {
			return nil, fmt.Errorf("nothing imported: seal %s under this instance's key: %w", k, err)
		}
		rows = append(rows, settingRow{key: k, ciphertext: ct})
		rep.SecretsImported++
	}
	return rows, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
