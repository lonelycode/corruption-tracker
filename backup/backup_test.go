// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package backup_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/backup"
	"github.com/lonelycode/corruption-tracker/migrations"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// fakeTools writes stand-ins for pg_dump and pg_restore that record their
// arguments. The real binaries are exercised by scripts/compose-upgrade-test.sh
// against the image's PostgreSQL 16 client.
func fakeTools(t *testing.T) (dump, restore, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	dump = filepath.Join(dir, "pg_dump")
	restore = filepath.Join(dir, "pg_restore")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(dump, `echo "pg_dump $*" >> `+log+`
for a in "$@"; do case "$a" in --file=*) echo "PGDMP fake" > "${a#--file=}";; esac; done
`)
	write(restore, `echo "pg_restore $*" >> `+log+"\n")
	return dump, restore, log
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestRunListDownloadAndStatus(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	dump, _, calls := fakeTools(t)
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)}
	r := backup.New(backup.Options{Pool: st.Pool, DSN: "postgres://example/db", Dir: dir, PGDump: dump, Now: clk.now})

	if s, err := r.Status(); err != nil || s.Latest != nil || !s.Stale {
		t.Errorf("status before any backup = %+v, %v", s, err)
	}
	b, err := r.Run(ctx, backup.KindNightly, "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "20261012T030000Z-nightly" || b.SchemaVersion == 0 || !b.Has(backup.DumpFile) || len(b.Files) != 2 {
		t.Fatalf("backup = %+v", b)
	}
	var bundleFile string
	for _, f := range b.Files {
		if strings.HasSuffix(f.Name, ".tar.gz") {
			bundleFile = f.Name
		}
		if f.Size == 0 || len(f.SHA256) != 64 {
			t.Errorf("file %+v not described", f)
		}
	}
	if bundleFile == "" {
		t.Fatal("no export bundle in the backup")
	}
	raw, _ := os.ReadFile(calls)
	if !strings.Contains(string(raw), "--format=custom") || !strings.Contains(string(raw), "--dbname=postgres://example/db") {
		t.Errorf("pg_dump called as %q", raw)
	}
	// Nothing partial is left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != b.Name {
		t.Errorf("backup dir holds %v", entries)
	}

	all, err := r.List()
	if err != nil || len(all) != 1 || all[0].Name != b.Name {
		t.Fatalf("list = %+v, %v", all, err)
	}
	f, _, err := r.Open(b.Name, backup.DumpFile)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	f.Close()
	if string(body) != "PGDMP fake\n" {
		t.Errorf("dump = %q", body)
	}
	for _, bad := range [][2]string{
		{b.Name, "../../etc/passwd"}, {b.Name, "secret.key"}, {"../" + b.Name, backup.DumpFile}, {"20261012T030000Z-other", backup.DumpFile},
	} {
		if f, _, err := r.Open(bad[0], bad[1]); err == nil {
			f.Close()
			t.Errorf("Open(%q, %q) served a file", bad[0], bad[1])
		}
	}

	if s, _ := r.Status(); s.Latest == nil || s.Latest.Name != b.Name || s.Stale {
		t.Errorf("status after a backup = %+v", s)
	}
	clk.t = clk.t.Add(backup.StaleAfter + time.Minute)
	if s, _ := r.Status(); !s.Stale {
		t.Error("a backup older than StaleAfter is not reported stale")
	}
	if _, err := r.Run(ctx, "weekly", ""); err == nil {
		t.Error("unknown kind accepted")
	}
}

// A failed pg_dump leaves no backup, partial or listed.
func TestFailedDumpLeavesNothing(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	r := backup.New(backup.Options{Pool: st.Pool, DSN: "postgres://example/db", Dir: dir, PGDump: "/bin/false"})
	if _, err := r.Run(context.Background(), backup.KindManual, ""); err == nil || !strings.Contains(err.Error(), "pg_dump") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

// fakeBackup writes a completed backup's manifest only (retention reads
// nothing else).
func fakeBackup(t *testing.T, dir string, at time.Time, kind string) string {
	t.Helper()
	b := backup.Backup{Name: at.UTC().Format("20060102T150405Z") + "-" + kind, Kind: kind, CreatedAt: at.UTC()}
	if err := os.MkdirAll(filepath.Join(dir, b.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(dir, b.Name, backup.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return b.Name
}

func TestRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC) // a Monday
	kept := map[string]bool{}
	var all []string
	// 70 nightly backups, plus a manual one on the latest day.
	for i := 0; i < 70; i++ {
		all = append(all, fakeBackup(t, dir, now.AddDate(0, 0, -i), backup.KindNightly))
	}
	manual := fakeBackup(t, dir, now.Add(-2*time.Hour), backup.KindManual)
	// 5 pre-migrate backups.
	var pre []string
	for i := 0; i < 5; i++ {
		pre = append(pre, fakeBackup(t, dir, now.AddDate(0, -i-1, 0), backup.KindPreMigrate))
	}
	// An interrupted backup from two days ago, and a fresh one in progress.
	old := filepath.Join(dir, ".partial-20261010T030000Z-nightly-123")
	_ = os.MkdirAll(old, 0o700)
	_ = os.Chtimes(old, now.AddDate(0, 0, -2), now.AddDate(0, 0, -2))
	fresh := filepath.Join(dir, ".partial-20261012T030000Z-nightly-456")
	_ = os.MkdirAll(fresh, 0o700)
	_ = os.Chtimes(fresh, now, now)

	r := backup.New(backup.Options{Dir: dir, Now: func() time.Time { return now }})
	if _, err := r.Prune(); err != nil {
		t.Fatal(err)
	}
	left, _ := r.List()
	for _, b := range left {
		kept[b.Name] = true
	}
	// The last 14 days' nightlies (the latest day's newest is the nightly
	// at 03:00, newer than the manual one at 01:00).
	for i := 0; i < 14; i++ {
		if !kept[all[i]] {
			t.Errorf("daily %s removed", all[i])
		}
	}
	if kept[manual] {
		t.Error("an older backup on an already-kept day survived")
	}
	// Then one per ISO week for 8 weeks: the newest of each week.
	weeks := map[string]bool{}
	for _, b := range left {
		if b.Kind != backup.KindNightly {
			continue
		}
		y, w := b.CreatedAt.ISOWeek()
		weeks[fmt.Sprintf("%d-W%02d", y, w)] = true
	}
	if len(weeks) < 8 {
		t.Errorf("kept %d distinct weeks, want at least 8", len(weeks))
	}
	if kept[all[69]] {
		t.Errorf("a nightly from ten weeks ago survived")
	}
	nightlies := 0
	for _, b := range left {
		if b.Kind == backup.KindNightly {
			nightlies++
		}
	}
	if nightlies > backup.KeepDaily+backup.KeepWeekly {
		t.Errorf("%d nightlies kept", nightlies)
	}
	for i, n := range pre {
		if kept[n] != (i < backup.KeepPreMigrate) {
			t.Errorf("pre-migrate %d (%s) kept=%v", i, n, kept[n])
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a day-old partial backup was not cleaned up")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("an in-progress backup was removed")
	}
}

// An upgrade takes a pre-migrate backup first; a fresh or current database
// doesn't need one; a failed backup stops the migration; the escape hatch
// lets it through.
func TestBeforeMigrate(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	dump, _, _ := fakeTools(t)
	dsn := st.Pool.Config().ConnString()
	latest, err := migrations.Latest()
	if err != nil {
		t.Fatal(err)
	}
	r := backup.New(backup.Options{Pool: st.Pool, DSN: dsn, Dir: t.TempDir(), PGDump: dump})
	noEnv := func(string) (string, bool) { return "", false }

	if b, err := r.BeforeMigrate(ctx, dsn, noEnv); err != nil || b != nil {
		t.Fatalf("up to date: %+v, %v", b, err)
	}

	if _, err := st.Pool.Exec(ctx, `UPDATE tracker_schema_migrations SET version = $1`, latest-1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.Pool.Exec(ctx, `UPDATE tracker_schema_migrations SET version = $1`, latest) })
	b, err := r.BeforeMigrate(ctx, dsn, noEnv)
	if err != nil || b == nil || b.Kind != backup.KindPreMigrate || !strings.Contains(b.Note, "to") {
		t.Fatalf("upgrade: %+v, %v", b, err)
	}

	broken := backup.New(backup.Options{Pool: st.Pool, DSN: dsn, Dir: t.TempDir(), PGDump: "/bin/false"})
	if _, err := broken.BeforeMigrate(ctx, dsn, noEnv); err == nil || !strings.Contains(err.Error(), backup.SkipEnv) {
		t.Errorf("failed backup: err = %v, want it to stop the migration and name %s", err, backup.SkipEnv)
	}
	skip := func(k string) (string, bool) { return "1", k == backup.SkipEnv }
	if b, err := broken.BeforeMigrate(ctx, dsn, skip); err != nil || b != nil {
		t.Errorf("with %s=1: %+v, %v", backup.SkipEnv, b, err)
	}
}

func TestRestoreRefusesAnInUseTracker(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	_, restore, calls := fakeTools(t)
	dir := t.TempDir()
	bdir := filepath.Join(dir, "20261012T030000Z-nightly")
	_ = os.MkdirAll(bdir, 0o700)
	_ = os.WriteFile(filepath.Join(bdir, backup.DumpFile), []byte("PGDMP"), 0o600)
	r := backup.New(backup.Options{Pool: st.Pool, DSN: "postgres://example/target", PGRestore: restore})

	if err := r.Restore(ctx, bdir, backup.RestoreOptions{}); err != nil {
		t.Fatalf("restore into an empty tracker: %v", err)
	}
	raw, _ := os.ReadFile(calls)
	for _, want := range []string{"--clean", "--single-transaction", "--dbname=postgres://example/target", filepath.Join(bdir, backup.DumpFile)} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("pg_restore call %q lacks %q", raw, want)
		}
	}

	actor, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "local:1", Label: "someone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO tracker_users (actor_id, username, password_hash, role) VALUES ($1, 'someone', 'x', 'admin')`, actor); err != nil {
		t.Fatal(err)
	}
	if err := r.Restore(ctx, bdir, backup.RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "user accounts") {
		t.Errorf("restore over accounts: err = %v", err)
	}
	if err := r.Restore(ctx, bdir, backup.RestoreOptions{Force: true}); err != nil {
		t.Errorf("forced restore: %v", err)
	}
	if err := r.Restore(ctx, filepath.Join(dir, "missing"), backup.RestoreOptions{Force: true}); err == nil {
		t.Error("restore of a missing backup succeeded")
	}
}
