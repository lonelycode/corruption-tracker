// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExportOptions configure one export.
type ExportOptions struct {
	Source     Source
	AppVersion string
	// IncludeAccessLog adds tracker_access_log (§10.4 order 11).
	IncludeAccessLog bool
	// Handoff, set when exporting a frozen instance for a handoff, is
	// recorded in the manifest (§10.3, §11.4 step 2).
	Handoff *Handoff
	// IncludeSecrets adds secrets.enc (§10.7): the secret settings,
	// decrypted with Secrets and re-sealed under Passphrase. Standalone
	// sources only (alitheia-embedded secrets live in the host environment).
	IncludeSecrets bool
	Passphrase     string
	Secrets        SecretOpener
	// Now and NewID default to the wall clock and a random UUID; tests pin
	// them.
	Now   func() time.Time
	NewID func() string
	Log   *slog.Logger

	// afterTable, when set (tests only), runs after each table file is
	// written, still inside the snapshot transaction.
	afterTable func(table string)
}

// FileName is the bundle's file name (§10.1):
// corruption-tracker-<instance-short>-<YYYYMMDDTHHMMSSZ>.tar.gz.
func FileName(m Manifest) string {
	short := strings.ReplaceAll(m.Source.InstanceID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	t, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		t = time.Time{}
	}
	return fmt.Sprintf("corruption-tracker-%s-%s.tar.gz", short, t.UTC().Format("20060102T150405Z"))
}

// Export writes a bundle of pool's tracker dataset into dir and returns its
// path. The file appears under its final name only once complete.
func Export(ctx context.Context, pool *pgxpool.Pool, dir string, opts ExportOptions) (string, Manifest, error) {
	tmp, err := os.CreateTemp(dir, ".corruption-tracker-export-*.partial")
	if err != nil {
		return "", Manifest{}, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // no-op after the rename
	}()
	m, err := Write(ctx, pool, tmp, opts)
	if err != nil {
		return "", m, err
	}
	if err := tmp.Sync(); err != nil {
		return "", m, err
	}
	if err := tmp.Close(); err != nil {
		return "", m, err
	}
	path := filepath.Join(dir, FileName(m))
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", m, err
	}
	return path, m, nil
}

// Write reads the dataset in one REPEATABLE READ, READ ONLY transaction and
// writes the bundle archive to w. No locks are taken: concurrent writers
// keep running, and their commits after the snapshot are simply absent.
func Write(ctx context.Context, pool *pgxpool.Pool, w io.Writer, opts ExportOptions) (Manifest, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = NewID
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Source.Kind != KindAlitheiaEmbedded && opts.Source.Kind != KindStandalone {
		return Manifest{}, fmt.Errorf("bundle: unknown source kind %q", opts.Source.Kind)
	}
	if opts.Source.InstanceID == "" {
		return Manifest{}, errors.New("bundle: source instance id is required")
	}
	if opts.IncludeSecrets {
		if opts.Source.Kind == KindAlitheiaEmbedded {
			return Manifest{}, errors.New("bundle: --include-secrets is not available for an alitheia-embedded source (its secrets live in the host environment, not the database)")
		}
		if opts.Secrets == nil {
			return Manifest{}, errors.New("bundle: --include-secrets needs the settings store to decrypt secrets")
		}
		if err := checkPassphrase(opts.Passphrase); err != nil {
			return Manifest{}, err
		}
	}

	stage, err := os.MkdirTemp("", "corruption-tracker-export-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stage)

	created := opts.Now().UTC().Truncate(time.Second)
	m := Manifest{
		Format:        Format,
		FormatVersion: FormatVersion,
		AppVersion:    opts.AppVersion,
		ExportID:      opts.NewID(),
		CreatedAt:     created.Format(time.RFC3339),
		Source:        opts.Source,
		Options:       Options{IncludeSecrets: opts.IncludeSecrets, IncludeAccessLog: opts.IncludeAccessLog},
		Tables:        []TableEntry{},
		OmittedTables: []OmittedTable{},
	}
	m.Handoff = opts.Handoff
	if err := readSnapshot(ctx, pool, stage, &m, opts); err != nil {
		return m, err
	}

	if m.Settings.File == "" { // schema without tracker_settings
		settingsSum, err := writeStaged(filepath.Join(stage, SettingsFile), []byte("{}\n"))
		if err != nil {
			return m, err
		}
		m.Settings = FileRef{File: SettingsFile, SHA256: settingsSum}
	}

	if err := writeArchive(w, stage, m, created); err != nil {
		return m, err
	}
	var rows int64
	for _, t := range m.Tables {
		rows += t.Rows
	}
	opts.Log.Info("tracker export written", "export_id", m.ExportID, "schema_version", m.SchemaVersion,
		"tables", len(m.Tables), "omitted_tables", len(m.OmittedTables), "rows", rows)
	return m, nil
}

// readSnapshot opens the snapshot transaction and writes every table file.
func readSnapshot(ctx context.Context, pool *pgxpool.Pool, stage string, m *Manifest, opts ExportOptions) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The first statement fixes the snapshot.
	if err := tx.QueryRow(ctx, `SELECT pg_current_snapshot()::text`).Scan(&m.Snapshot.PGSnapshot); err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	m.Snapshot.Isolation = "repeatable read"
	for _, s := range []string{
		`SET LOCAL TimeZone = 'UTC'`,
		`SET LOCAL DateStyle = 'ISO, YMD'`,
		`SET LOCAL IntervalStyle = 'postgres'`,
		`SET LOCAL extra_float_digits = 1`,
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	var serverVersion string
	if err := tx.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&serverVersion); err != nil {
		return err
	}
	if f := strings.Fields(serverVersion); len(f) > 0 {
		serverVersion = f[0]
	}
	m.Snapshot.PGServerVersion = serverVersion
	// Diagnostic only, and not permitted everywhere (e.g. on a standby):
	// try it under a savepoint so a refusal doesn't abort the snapshot.
	if _, err := tx.Exec(ctx, `SAVEPOINT wal_lsn`); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&m.Snapshot.WALLSN); err != nil {
		m.Snapshot.WALLSN = ""
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT wal_lsn`); err != nil {
			return err
		}
	}

	var dirty bool
	if err := tx.QueryRow(ctx, `SELECT version, dirty FROM tracker_schema_migrations`).Scan(&m.SchemaVersion, &dirty); err != nil {
		return fmt.Errorf("read tracker schema version: %w", err)
	}
	if dirty {
		return fmt.Errorf("tracker schema version %d is dirty; fix the migration before exporting", m.SchemaVersion)
	}
	var hasSettings bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('tracker_settings') IS NOT NULL`).Scan(&hasSettings); err != nil {
		return err
	}
	if hasSettings {
		if err := exportSettings(ctx, tx, stage, m, opts); err != nil {
			return err
		}
	} else if opts.IncludeSecrets {
		return errors.New("bundle: --include-secrets: this schema has no tracker_settings")
	}

	tables := append([]string(nil), TableSet...)
	if opts.IncludeAccessLog {
		tables = append(tables, AccessLogTable)
	}
	if err := os.Mkdir(filepath.Join(stage, "tables"), 0o755); err != nil {
		return err
	}
	for _, name := range tables {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass($1) AND relkind IN ('r', 'p'))`,
			name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			m.OmittedTables = append(m.OmittedTables, OmittedTable{
				Name: name, Reason: fmt.Sprintf("not in schema version %d", m.SchemaVersion)})
			continue
		}
		entry, err := exportTable(ctx, tx, stage, name)
		if err != nil {
			return fmt.Errorf("export %s: %w", name, err)
		}
		m.Tables = append(m.Tables, entry)
		if opts.afterTable != nil {
			opts.afterTable(name)
		}
	}
	if !opts.IncludeAccessLog {
		m.OmittedTables = append(m.OmittedTables, OmittedTable{
			Name: AccessLogTable, Reason: "option include_access_log=false"})
	}
	return nil
}

func exportTable(ctx context.Context, tx pgx.Tx, stage, name string) (TableEntry, error) {
	e := TableEntry{Name: name, File: tablesDir + name + ".ndjson"}
	rows, err := tx.Query(ctx, `
		SELECT attname, format_type(atttypid, atttypmod),
		       attidentity <> '' OR pg_get_serial_sequence(quote_ident($1), attname) IS NOT NULL
		  FROM pg_attribute
		 WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped
		 ORDER BY attnum`, name)
	if err != nil {
		return e, err
	}
	var serial string
	for rows.Next() {
		var c Column
		var isSerial bool
		if err := rows.Scan(&c.Name, &c.Type, &isSerial); err != nil {
			rows.Close()
			return e, err
		}
		e.Columns = append(e.Columns, c)
		if isSerial && serial == "" {
			serial = c.Name
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return e, err
	}
	if e.PK, err = orderKey(ctx, tx, name); err != nil {
		return e, err
	}

	cols := make([]string, len(e.Columns))
	for i, c := range e.Columns {
		cols[i] = pgx.Identifier{c.Name}.Sanitize()
	}
	order := make([]string, len(e.PK))
	for i, c := range e.PK {
		order[i] = pgx.Identifier{c}.Sanitize()
	}
	q := fmt.Sprintf(`SELECT row_to_json(t)::text FROM (SELECT %s FROM %s ORDER BY %s) t`,
		strings.Join(cols, ", "), pgx.Identifier{name}.Sanitize(), strings.Join(order, ", "))

	f, err := os.Create(filepath.Join(stage, e.File))
	if err != nil {
		return e, err
	}
	defer f.Close()
	h := sha256.New()
	out := io.MultiWriter(f, h)
	data, err := tx.Query(ctx, q)
	if err != nil {
		return e, err
	}
	for data.Next() {
		var line string
		if err := data.Scan(&line); err != nil {
			data.Close()
			return e, err
		}
		if strings.ContainsRune(line, '\n') {
			// row_to_json escapes newlines inside strings; a raw one would
			// break the line format.
			data.Close()
			return e, errors.New("row_to_json produced a multi-line row")
		}
		if _, err := io.WriteString(out, line+"\n"); err != nil {
			data.Close()
			return e, err
		}
		e.Rows++
	}
	data.Close()
	if err := data.Err(); err != nil {
		return e, err
	}
	if err := f.Close(); err != nil {
		return e, err
	}
	e.SHA256 = hex.EncodeToString(h.Sum(nil))

	if serial != "" {
		var max *int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT max(%s)::bigint FROM %s`,
			pgx.Identifier{serial}.Sanitize(), pgx.Identifier{name}.Sanitize())).Scan(&max); err != nil {
			return e, err
		}
		e.MaxID = max
	}
	return e, nil
}

// orderKey returns the primary key columns, or else those of the first
// non-partial unique index over NOT NULL columns. A table with neither
// can't be exported deterministically (§10.2).
func orderKey(ctx context.Context, tx pgx.Tx, table string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.indexrelid::bigint, i.indisprimary, a.attname, a.attnotnull
		  FROM pg_index i
		  CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		 WHERE i.indrelid = to_regclass($1) AND i.indisunique AND i.indpred IS NULL AND i.indexprs IS NULL
		 ORDER BY i.indisprimary DESC, i.indexrelid, k.ord`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type idx struct {
		cols    []string
		notNull bool
	}
	var order []int64
	byID := map[int64]*idx{}
	for rows.Next() {
		var id int64
		var primary, notNull bool
		var col string
		if err := rows.Scan(&id, &primary, &col, &notNull); err != nil {
			return nil, err
		}
		ix, ok := byID[id]
		if !ok {
			ix = &idx{notNull: true}
			byID[id] = ix
			order = append(order, id)
		}
		ix.cols = append(ix.cols, col)
		ix.notNull = ix.notNull && notNull
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range order {
		if ix := byID[id]; ix.notNull {
			return ix.cols, nil
		}
	}
	return nil, fmt.Errorf("table %s has no primary key or NOT NULL unique key; it can't be exported deterministically", table)
}

func writeStaged(path string, b []byte) (string, error) {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// writeArchive writes manifest.json, settings.json and the table files, in
// that order, with fixed modes, owners and mtimes (§10.1).
func writeArchive(w io.Writer, stage string, m Manifest, mtime time.Time) error {
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')

	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	add := func(name string, r io.Reader, size int64) error {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: size,
			ModTime: mtime, Format: tar.FormatUSTAR,
		}); err != nil {
			return fmt.Errorf("tar header %s: %w", name, err)
		}
		_, err := io.Copy(tw, r)
		return err
	}
	if err := add(ManifestFile, strings.NewReader(string(manifest)), int64(len(manifest))); err != nil {
		return err
	}
	files := []string{m.Settings.File}
	for _, t := range m.Tables {
		files = append(files, t.File)
	}
	if m.Secrets != nil {
		files = append(files, m.Secrets.File)
	}
	for _, name := range files {
		f, err := os.Open(filepath.Join(stage, name))
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err == nil {
			err = add(name, f, st.Size())
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// NewID returns a random (v4) UUID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// exportSettings writes settings.json (non-secret settings, keys sorted) and,
// with IncludeSecrets, secrets.enc, from the snapshot transaction.
func exportSettings(ctx context.Context, tx pgx.Tx, stage string, m *Manifest, opts ExportOptions) error {
	rows, err := tx.Query(ctx, `SELECT key, value, secret, ciphertext FROM tracker_settings ORDER BY key`)
	if err != nil {
		return fmt.Errorf("read tracker_settings: %w", err)
	}
	plain := map[string]string{}
	secrets := map[string]string{}
	for rows.Next() {
		var k, v string
		var secret bool
		var ct []byte
		if err := rows.Scan(&k, &v, &secret, &ct); err != nil {
			rows.Close()
			return err
		}
		if !secret {
			plain[k] = v
			continue
		}
		if !opts.IncludeSecrets {
			continue
		}
		pt, err := opts.Secrets.OpenSecret(k, ct)
		if err != nil {
			rows.Close()
			return fmt.Errorf("decrypt secret setting %s: %w", k, err)
		}
		secrets[k] = pt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(plain) // compact, keys sorted: an empty map stays exactly "{}\n"
	if err != nil {
		return err
	}
	sum, err := writeStaged(filepath.Join(stage, SettingsFile), append(b, '\n'))
	if err != nil {
		return err
	}
	m.Settings = FileRef{File: SettingsFile, SHA256: sum}
	if !opts.IncludeSecrets {
		return nil
	}
	enc, err := sealSecrets(secrets, opts.Passphrase)
	if err != nil {
		return err
	}
	ssum, err := writeStaged(filepath.Join(stage, SecretsFile), enc)
	if err != nil {
		return err
	}
	m.Secrets = &FileRef{File: SecretsFile, SHA256: ssum}
	return nil
}
