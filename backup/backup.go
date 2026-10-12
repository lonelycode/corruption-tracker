// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package backup is the standalone tracker's own backups:
// a nightly job, a backup before every schema migration and one on demand
// from /admin/backups, each holding
//
//   - database.dump: a pg_dump (custom format) of the whole database. It
//     restores everything, accounts and settings included, with `restore`.
//   - an export bundle (package bundle): the dataset in the portable,
//     verifiable format another tracker imports.
//
// Backups live on the data volume, under <data dir>/backups, one directory
// per backup with a backup.json manifest. A directory is written under a
// temporary name and renamed when complete, so a listed backup is never
// partial. Retention keeps the newest backup of each of the last 14 days
// and of each of the last 8 weeks, plus the last 3 pre-migration backups.
//
// Stored secrets (the LLM key, the mailbox password) are in the dump only
// as ciphertext. Restoring them needs the same instance key: the data
// volume's secret.key or TRACKER_SECRET. Without it the restored tracker
// asks for them again; nothing else is lost.
//
// alitheia doesn't use this package: its database has its own backups.
package backup

import (
	"bytes"
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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/version"
)

// Kinds of backup.
const (
	KindNightly    = "nightly"
	KindManual     = "manual"
	KindPreMigrate = "pre-migrate"
)

// Retention defaults.
const (
	KeepDaily      = 14
	KeepWeekly     = 8
	KeepPreMigrate = 3
)

// StaleAfter is when the newest backup counts as overdue: the nightly job
// has missed more than one run.
const StaleAfter = 36 * time.Hour

// DumpFile is the pg_dump in every backup; ManifestFile describes it.
const (
	DumpFile     = "database.dump"
	ManifestFile = "backup.json"
)

// DirName is the backups directory under the data volume.
const DirName = "backups"

// Options configure a Runner.
type Options struct {
	Pool *pgxpool.Pool
	// DSN is what pg_dump connects with (the pool's own connection string).
	DSN string
	// Dir holds the backups: <data dir>/backups.
	Dir string
	// PGDump and PGRestore default to the binaries on PATH (the image
	// ships PostgreSQL 16's).
	PGDump    string
	PGRestore string
	Now       func() time.Time
	Log       *slog.Logger
}

// Runner takes, lists and prunes backups.
type Runner struct{ o Options }

// New returns a Runner.
func New(o Options) *Runner {
	if o.PGDump == "" {
		o.PGDump = "pg_dump"
	}
	if o.PGRestore == "" {
		o.PGRestore = "pg_restore"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Runner{o: o}
}

// Dir is where the backups live.
func (r *Runner) Dir() string { return r.o.Dir }

// File is one file of a backup.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Backup is one completed backup (its backup.json).
type Backup struct {
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	CreatedAt     time.Time `json:"created_at"`
	SchemaVersion uint      `json:"schema_version"`
	AppVersion    string    `json:"app_version"`
	Note          string    `json:"note,omitempty"`
	Files         []File    `json:"files"`
}

// Size is the backup's total size in bytes.
func (b Backup) Size() int64 {
	var n int64
	for _, f := range b.Files {
		n += f.Size
	}
	return n
}

// Has reports whether file is one of the backup's files.
func (b Backup) Has(file string) bool {
	for _, f := range b.Files {
		if f.Name == file {
			return true
		}
	}
	return false
}

// Run takes a backup now and then applies retention. note is free text
// for the manifest (e.g. which migration a pre-migrate backup preceded).
func (r *Runner) Run(ctx context.Context, kind, note string) (Backup, error) {
	switch kind {
	case KindNightly, KindManual, KindPreMigrate:
	default:
		return Backup{}, fmt.Errorf("unknown backup kind %q", kind)
	}
	if r.o.Dir == "" {
		return Backup{}, errors.New("backup: no backup directory")
	}
	if err := os.MkdirAll(r.o.Dir, 0o700); err != nil {
		return Backup{}, fmt.Errorf("backup: %w", err)
	}
	now := r.o.Now().UTC()
	b := Backup{
		Name:       now.Format("20060102T150405Z") + "-" + kind,
		Kind:       kind,
		CreatedAt:  now,
		AppVersion: version.Version,
		Note:       note,
	}
	final := filepath.Join(r.o.Dir, b.Name)
	if _, err := os.Stat(final); err == nil {
		return Backup{}, fmt.Errorf("backup %s already exists", b.Name)
	}
	tmp, err := os.MkdirTemp(r.o.Dir, ".partial-"+b.Name+"-")
	if err != nil {
		return Backup{}, fmt.Errorf("backup: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()

	if err := r.o.Pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT version FROM tracker_schema_migrations LIMIT 1), 0)`).Scan(&b.SchemaVersion); err != nil {
		return Backup{}, fmt.Errorf("backup: read schema version: %w", err)
	}
	if err := r.dump(ctx, filepath.Join(tmp, DumpFile)); err != nil {
		return Backup{}, err
	}
	var id string
	if err := r.o.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT id::text FROM tracker_instance LIMIT 1), '')`).Scan(&id); err != nil || id == "" {
		id = bundle.NewID()
	}
	if _, _, err := bundle.Export(ctx, r.o.Pool, tmp, bundle.ExportOptions{
		Source:     bundle.Source{InstanceID: id, Kind: bundle.KindStandalone},
		AppVersion: version.Version, IncludeAccessLog: true, Now: r.o.Now, Log: r.o.Log,
	}); err != nil {
		return Backup{}, fmt.Errorf("backup: export bundle: %w", err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		return Backup{}, err
	}
	for _, e := range entries {
		f, err := describe(filepath.Join(tmp, e.Name()))
		if err != nil {
			return Backup{}, err
		}
		b.Files = append(b.Files, f)
	}
	mb, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return Backup{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, ManifestFile), append(mb, '\n'), 0o600); err != nil {
		return Backup{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return Backup{}, fmt.Errorf("backup: %w", err)
	}
	ok = true
	r.o.Log.Info("backup written", "name", b.Name, "bytes", b.Size(), "schema_version", b.SchemaVersion)

	if removed, err := r.Prune(); err != nil {
		r.o.Log.Warn("backup retention failed", "err", err)
	} else if len(removed) > 0 {
		r.o.Log.Info("old backups removed", "names", removed)
	}
	return b, nil
}

func (r *Runner) dump(ctx context.Context, path string) error {
	if r.o.DSN == "" {
		return errors.New("backup: no database URL for pg_dump")
	}
	cmd := exec.CommandContext(ctx, r.o.PGDump, "--format=custom", "--no-owner", "--file="+path, "--dbname="+r.o.DSN)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func describe(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return File{}, err
	}
	return File{Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

var nameRE = regexp.MustCompile(`^\d{8}T\d{6}Z-(nightly|manual|pre-migrate)$`)

// List returns the completed backups, newest first. Anything else in the
// directory (a partial backup, `import --replace` backups) is ignored.
func (r *Runner) List() ([]Backup, error) {
	entries, err := os.ReadDir(r.o.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Backup
	for _, e := range entries {
		if !e.IsDir() || !nameRE.MatchString(e.Name()) {
			continue
		}
		b, err := r.Get(e.Name())
		if err != nil {
			r.o.Log.Warn("unreadable backup skipped", "name", e.Name(), "err", err)
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Get reads one backup's manifest. The name must be a backup's own (no
// path tricks reach outside the backups directory).
func (r *Runner) Get(name string) (Backup, error) {
	if !nameRE.MatchString(name) {
		return Backup{}, fmt.Errorf("not a backup name: %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(r.o.Dir, name, ManifestFile))
	if err != nil {
		return Backup{}, err
	}
	var b Backup
	if err := json.Unmarshal(raw, &b); err != nil {
		return Backup{}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if b.Name != name {
		return Backup{}, fmt.Errorf("%s names %q", ManifestFile, b.Name)
	}
	return b, nil
}

// Open opens one file of a backup for download.
func (r *Runner) Open(name, file string) (*os.File, Backup, error) {
	b, err := r.Get(name)
	if err != nil {
		return nil, b, err
	}
	if !b.Has(file) {
		return nil, b, fmt.Errorf("backup %s has no file %q", name, file)
	}
	f, err := os.Open(filepath.Join(r.o.Dir, name, file))
	return f, b, err
}

// Status is the newest backup and whether it is overdue.
type Status struct {
	Latest *Backup
	Stale  bool // no backup, or the newest is older than StaleAfter
}

// Status reports the newest backup.
func (r *Runner) Status() (Status, error) {
	all, err := r.List()
	if err != nil {
		return Status{Stale: true}, err
	}
	if len(all) == 0 {
		return Status{Stale: true}, nil
	}
	latest := all[0]
	return Status{Latest: &latest, Stale: r.o.Now().Sub(latest.CreatedAt) > StaleAfter}, nil
}

// Prune applies retention and removes leftovers of interrupted backups
// older than a day. It returns the names it removed.
func (r *Runner) Prune() ([]string, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, b := range all {
		if !keep(all, b) {
			if err := os.RemoveAll(filepath.Join(r.o.Dir, b.Name)); err != nil {
				return removed, err
			}
			removed = append(removed, b.Name)
		}
	}
	entries, err := os.ReadDir(r.o.Dir)
	if err != nil {
		return removed, err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".partial-") {
			continue
		}
		if info, err := e.Info(); err == nil && r.o.Now().Sub(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(r.o.Dir, e.Name()))
		}
	}
	return removed, nil
}

// keep is the retention rule over all completed backups, newest first.
func keep(all []Backup, b Backup) bool {
	if b.Name == all[0].Name {
		return true // never remove the newest backup
	}
	if b.Kind == KindPreMigrate {
		n := 0
		for _, o := range all {
			if o.Kind != KindPreMigrate {
				continue
			}
			if o.Name == b.Name {
				return n < KeepPreMigrate
			}
			n++
		}
		return false
	}
	// Nightly and manual: the newest of each of the last KeepDaily days
	// and of each of the last KeepWeekly ISO weeks that have one.
	days, weeks := map[string]string{}, map[string]string{}
	var dayOrder, weekOrder []string
	for _, o := range all {
		if o.Kind == KindPreMigrate {
			continue
		}
		d := o.CreatedAt.UTC().Format("2006-01-02")
		if _, seen := days[d]; !seen {
			days[d] = o.Name
			dayOrder = append(dayOrder, d)
		}
		y, w := o.CreatedAt.UTC().ISOWeek()
		wk := fmt.Sprintf("%d-W%02d", y, w)
		if _, seen := weeks[wk]; !seen {
			weeks[wk] = o.Name
			weekOrder = append(weekOrder, wk)
		}
	}
	for i, d := range dayOrder {
		if i < KeepDaily && days[d] == b.Name {
			return true
		}
	}
	for i, wk := range weekOrder {
		if i < KeepWeekly && weeks[wk] == b.Name {
			return true
		}
	}
	return false
}

// RestoreOptions control Restore.
type RestoreOptions struct {
	// Force restores over a tracker that already has accounts or cases.
	Force bool
}

// Restore loads a backup's database.dump (path is a backup directory or
// the .dump file itself) into the database, replacing what is there, in
// one transaction. It refuses a database that already holds accounts or
// cases unless Force is set. Migrate afterwards: the backup may be from an
// older schema.
func (r *Runner) Restore(ctx context.Context, path string, o RestoreOptions) error {
	dump := path
	if info, err := os.Stat(path); err != nil {
		return err
	} else if info.IsDir() {
		dump = filepath.Join(path, DumpFile)
	}
	if _, err := os.Stat(dump); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if !o.Force {
		inUse, err := r.inUse(ctx)
		if err != nil {
			return err
		}
		if inUse != "" {
			return fmt.Errorf("restore: this tracker already has %s; restoring replaces everything (use --force after taking a backup)", inUse)
		}
	}
	cmd := exec.CommandContext(ctx, r.o.PGRestore, "--clean", "--if-exists", "--no-owner", "--no-privileges",
		"--single-transaction", "--exit-on-error", "--dbname="+r.o.DSN, dump)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	r.o.Log.Info("backup restored", "from", dump)
	return nil
}

// inUse names what a restore would overwrite ("" when nothing).
func (r *Runner) inUse(ctx context.Context) (string, error) {
	for _, q := range []struct{ table, what string }{
		{"tracker_users", "user accounts"},
		{"fraud_cases", "cases"},
	} {
		var exists bool
		if err := r.o.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, q.table).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		var any bool
		if err := r.o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+q.table+`)`).Scan(&any); err != nil {
			return "", err
		}
		if any {
			return q.what, nil
		}
	}
	return "", nil
}
