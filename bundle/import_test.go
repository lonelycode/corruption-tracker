// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/migrations"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// seedWithSnapshots adds snapshot history (incl. a Revised re-issue) to
// seed's dataset.
func seedWithSnapshots(t *testing.T, st *store.Store) []int64 {
	t.Helper()
	ids := seed(t, st)
	ctx := context.Background()
	b := &fraudreport.Builder{DB: st, Log: quietLog(), IncludeUnverified: true}
	for _, note := range []string{"", "revised: late event"} {
		if _, _, err := b.Build(ctx, "2025-W11", note); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := b.Build(ctx, "2025", ""); err != nil {
		t.Fatal(err)
	}
	return ids
}

func exportFile(t *testing.T, st *store.Store) (string, Manifest) {
	t.Helper()
	path, m, err := Export(context.Background(), st.Pool, t.TempDir(), pinned(quietLog()))
	if err != nil {
		t.Fatal(err)
	}
	return path, m
}

func tableBytes(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, e := range unpack(t, archive) {
		if e.hdr.Name != ManifestFile {
			out[e.hdr.Name] = e.body
		}
	}
	return out
}

// export(A) -> import(B) -> export(B) gives identical table and settings
// files, and B reads back the same cases, snapshots and case history.
func TestImportRoundTrip(t *testing.T) {
	a := storetest.Open(t)
	caseIDs := seedWithSnapshots(t, a)
	path, m := exportFile(t, a)
	archiveA, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	b := storetest.Open(t)
	ctx := context.Background()
	rep, err := ImportFile(ctx, b.Pool, path, ImportOptions{Log: quietLog()})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(rep.Tables) != len(m.Tables) || rep.ExportID != m.ExportID {
		t.Fatalf("report = %+v", rep)
	}
	for _, tr := range rep.Tables {
		if !tr.OK {
			t.Errorf("report: %s not ok", tr.Name)
		}
	}

	archiveB, _ := exportBytes(t, b, pinned(quietLog()))
	ta, tb := tableBytes(t, archiveA), tableBytes(t, archiveB)
	if len(ta) != len(tb) {
		t.Fatalf("file sets differ: %d vs %d", len(ta), len(tb))
	}
	for name, body := range ta {
		if !bytes.Equal(body, tb[name]) {
			t.Errorf("%s differs after the round trip", name)
		}
	}

	// The same reads as the source: cases, snapshot history, case history.
	for _, id := range caseIDs {
		ca, pa, ea, sa, err := a.GetFraudCase(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cb, pb, eb, sb, err := b.GetFraudCase(ctx, id)
		if err != nil {
			t.Fatalf("case %d missing after import: %v", id, err)
		}
		if !reflect.DeepEqual(ca, cb) || !reflect.DeepEqual(pa, pb) || !reflect.DeepEqual(ea, eb) || !reflect.DeepEqual(sa, sb) {
			t.Errorf("case %d reads differently after import", id)
		}
		ra, _ := a.ListCaseRevisions(ctx, id, 0)
		rb, _ := b.ListCaseRevisions(ctx, id, 0)
		if !reflect.DeepEqual(ra, rb) {
			t.Errorf("case %d history differs after import", id)
		}
	}
	pa, _ := a.ListFraudReportPeriods(ctx, true)
	pb, _ := b.ListFraudReportPeriods(ctx, true)
	if !reflect.DeepEqual(pa, pb) || len(pa) == 0 {
		t.Errorf("snapshot periods differ: %+v vs %+v", pa, pb)
	}
	sa, _ := a.GetFraudReportSnapshot(ctx, "2025-W11", true)
	sb, _ := b.GetFraudReportSnapshot(ctx, "2025-W11", true)
	if sb.Version != 2 || !reflect.DeepEqual(sa, sb) {
		t.Errorf("revised snapshot after import = v%d, want the source's v2", sb.Version)
	}

	// Sequences continue past the imported ids.
	c, err := b.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "After import", TypologyRaw: "tax fraud"}})
	if err != nil {
		t.Fatalf("insert after import: %v", err)
	}
	if c.ID <= slices.Max(caseIDs) {
		t.Errorf("new case id %d does not continue past the imported %d", c.ID, slices.Max(caseIDs))
	}

	// The instance is marked as imported and paused.
	var from string
	var paused bool
	if err := b.Pool.QueryRow(ctx, `SELECT imported_from::text, ingestion_paused FROM tracker_instance`).Scan(&from, &paused); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(from), &got)
	if got["export_id"] != m.ExportID || got["instance_id"] != m.Source.InstanceID || !paused {
		t.Errorf("instance after import: imported_from %s, paused %v", from, paused)
	}
}

// Imported actors are legacy: nobody can sign in as them until an admin
// claims one.
func TestImportedActorsAreLegacy(t *testing.T) {
	a := storetest.Open(t)
	seed(t, a)
	path, _ := exportFile(t, a)
	b := storetest.Open(t)
	ctx := context.Background()
	if _, err := ImportFile(ctx, b.Pool, path, ImportOptions{Log: quietLog()}); err != nil {
		t.Fatal(err)
	}
	svc := localauth.NewService(b.Pool)
	if _, err := svc.Authenticate(ctx, "Reviewer Rowan", "anything-at-all-123"); err == nil {
		t.Fatal("signed in as an imported actor")
	}
	needed, err := svc.SetupNeeded(ctx)
	if err != nil || !needed {
		t.Fatalf("setup needed after import = %v %v, want true (no accounts came across)", needed, err)
	}
	claimable, err := svc.ClaimableActors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range claimable {
		found = found || strings.Contains(c.Label, "Reviewer Rowan")
	}
	if !found {
		t.Errorf("imported actor not offered for claiming: %+v", claimable)
	}
}

func targetRows(t *testing.T, st *store.Store) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, name := range TableSet {
		var n int64
		if err := st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+name).Scan(&n); err == nil {
			out[name] = n
		}
	}
	return out
}

// A corrupted, truncated or tampered bundle is rejected before anything is
// written.
func TestImportRejectsBadBundlesWithoutWriting(t *testing.T) {
	a := storetest.Open(t)
	seed(t, a)
	path, _ := exportFile(t, a)
	raw, _ := os.ReadFile(path)
	b := storetest.Open(t)
	before := targetRows(t, b)

	entries := unpack(t, raw)
	idx := slices.IndexFunc(entries, func(e entry) bool { return e.hdr.Name == "tables/fraud_cases.ndjson" })
	entries[idx].body[3] ^= 0x20
	tampered := repack(t, entries)

	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"truncated": raw[:len(raw)*2/3],
		"corrupt":   append(append([]byte{}, raw[:40]...), bytes.Repeat([]byte{0xff}, 64)...),
		"tampered":  tampered,
	} {
		p := filepath.Join(dir, name+".tar.gz")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ImportFile(context.Background(), b.Pool, p, ImportOptions{Log: quietLog()})
		if err == nil || !strings.Contains(err.Error(), "bundle rejected, nothing imported") {
			t.Errorf("%s: import = %v, want a rejection", name, err)
		}
		if after := targetRows(t, b); !reflect.DeepEqual(before, after) {
			t.Errorf("%s: target changed: %v -> %v", name, before, after)
		}
	}
}

func TestImportRefusesNonEmptyTargetUnlessReplace(t *testing.T) {
	a := storetest.Open(t)
	seed(t, a)
	path, _ := exportFile(t, a)
	b := storetest.Open(t)
	ctx := context.Background()
	if _, err := b.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "Local case", TypologyRaw: "fraud"}}); err != nil {
		t.Fatal(err)
	}
	_, err := ImportFile(ctx, b.Pool, path, ImportOptions{Log: quietLog()})
	if err == nil || !strings.Contains(err.Error(), "fraud_cases") || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("import into a non-empty target = %v", err)
	}
	if _, err := ImportFile(ctx, b.Pool, path, ImportOptions{Replace: true, Log: quietLog()}); err == nil ||
		!strings.Contains(err.Error(), "backup") {
		t.Fatalf("--replace without a backup dir = %v", err)
	}
}

// --replace writes a pg_dump and a bundle of the current data first, then
// replaces it.
func TestImportReplaceBacksUpFirst(t *testing.T) {
	a := storetest.Open(t)
	seed(t, a)
	path, m := exportFile(t, a)
	b := storetest.Open(t)
	ctx := context.Background()
	if _, err := b.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "Local case", TypologyRaw: "fraud"}}); err != nil {
		t.Fatal(err)
	}

	// A stand-in pg_dump that records its arguments and writes the file.
	bin := filepath.Join(t.TempDir(), "pg_dump")
	script := "#!/bin/sh\nfor a in \"$@\"; do case $a in --file=*) echo dump > \"${a#--file=}\";; esac; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	backups := t.TempDir()
	rep, err := ImportFile(ctx, b.Pool, path, ImportOptions{Replace: true, BackupDir: backups, PGDump: bin,
		DSN: "postgres://unused/db", Log: quietLog()})
	if err != nil {
		t.Fatalf("--replace import: %v", err)
	}
	if len(rep.Backups) != 2 {
		t.Fatalf("backups = %v", rep.Backups)
	}
	bm, err := VerifyFile(rep.Backups[1])
	if err != nil {
		t.Fatalf("backup bundle: %v", err)
	}
	for _, te := range bm.Tables {
		if te.Name == "fraud_cases" && te.Rows != 1 {
			t.Errorf("backup bundle has %d cases, want the 1 local case", te.Rows)
		}
	}
	if _, err := os.Stat(rep.Backups[0]); err != nil {
		t.Errorf("pg_dump file: %v", err)
	}
	var n int64
	_ = b.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_cases WHERE case_name = 'Local case'`).Scan(&n)
	if n != 0 {
		t.Error("local case survived --replace")
	}
	for _, te := range m.Tables {
		var got int64
		_ = b.Pool.QueryRow(ctx, "SELECT count(*) FROM "+te.Name).Scan(&got)
		if got != te.Rows {
			t.Errorf("%s after replace: %d rows, bundle %d", te.Name, got, te.Rows)
		}
	}
}

func TestImportRefusesNewerSchema(t *testing.T) {
	a := storetest.Open(t)
	path, _ := exportFile(t, a)
	raw, _ := os.ReadFile(path)
	entries := unpack(t, raw)
	var m Manifest
	if err := json.Unmarshal(entries[0].body, &m); err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = 9999
	entries[0].body, _ = json.Marshal(m)
	p := filepath.Join(t.TempDir(), "newer.tar.gz")
	if err := os.WriteFile(p, repack(t, entries), 0o600); err != nil {
		t.Fatal(err)
	}
	b := storetest.Open(t)
	if _, err := ImportFile(context.Background(), b.Pool, p, ImportOptions{Log: quietLog()}); err == nil ||
		!strings.Contains(err.Error(), "upgrade corruption-tracker first") {
		t.Fatalf("newer schema import = %v", err)
	}
}

// A target whose tables differ from the bundle's is refused, and a load
// that doesn't re-export byte-identically is rolled back.
func TestImportShapeCheckAndRollbackOnMismatch(t *testing.T) {
	a := storetest.Open(t)
	seed(t, a)
	path, _ := exportFile(t, a)
	ctx := context.Background()

	b := storetest.Open(t)
	if _, err := b.Pool.Exec(ctx, `ALTER TABLE fraud_cases ADD COLUMN local_extra int`); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(ctx, b.Pool, path, ImportOptions{Log: quietLog()}); err == nil ||
		!strings.Contains(err.Error(), "doesn't match the bundle") {
		t.Fatalf("shape mismatch import = %v", err)
	}

	// A trigger that rewrites rows on insert makes the re-export differ.
	c := storetest.Open(t)
	if _, err := c.Pool.Exec(ctx, `
		CREATE FUNCTION mangle() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN NEW.case_name := upper(NEW.case_name); RETURN NEW; END $$;
		CREATE TRIGGER mangle BEFORE INSERT ON fraud_cases FOR EACH ROW EXECUTE FUNCTION mangle();`); err != nil {
		t.Fatal(err)
	}
	before := targetRows(t, c)
	_, err := ImportFile(ctx, c.Pool, path, ImportOptions{Log: quietLog()})
	if err == nil || !strings.Contains(err.Error(), "does not re-export identically (fraud_cases)") {
		t.Fatalf("mangled import = %v, want a rollback naming fraud_cases", err)
	}
	if after := targetRows(t, c); !reflect.DeepEqual(before, after) {
		t.Errorf("rolled-back import changed the target: %v -> %v", before, after)
	}
}

// freshDB creates an empty database next to DATABASE_URL_TEST's (which must
// carry the test-db marker), migrated to version, and drops it afterwards.
func freshDB(t *testing.T, version uint) (string, *pgxpool.Pool) {
	t.Helper()
	storetest.Open(t) // skips without DATABASE_URL_TEST; fails on an unmarked DB
	dsn := os.Getenv("DATABASE_URL_TEST")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "ct_import_" + strings.ReplaceAll(NewID()[:13], "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	if err := migrations.MigrateTo(u.String(), version); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return u.String(), pool
}

// A bundle from an older schema imports into a newer, already-migrated
// target: the target is taken to exactly the bundle's version, loaded, then
// migrated to the latest.
func TestImportOlderBundleIntoNewerTarget(t *testing.T) {
	_, srcPool := freshDB(t, 4)
	src := store.New(srcPool)
	if _, err := src.EnsureActor(context.Background(), store.Actor{ExternalRef: "test:old", Label: "Old Reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateFraudCase(context.Background(), store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Old schema case", TypologyRaw: "tax fraud"}}); err != nil {
		t.Fatal(err)
	}
	path, m, err := Export(context.Background(), srcPool, t.TempDir(), pinned(quietLog()))
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 4 {
		t.Fatalf("source bundle schema = %d, want 4", m.SchemaVersion)
	}

	latest, _ := migrations.Latest()
	dsn, pool := freshDB(t, latest)
	if _, err := ImportFile(context.Background(), pool, path, ImportOptions{DSN: dsn, Log: quietLog()}); err != nil {
		t.Fatalf("import v4 bundle into v%d target: %v", latest, err)
	}
	if v, _, err := migrations.Version(dsn); err != nil || v != latest {
		t.Fatalf("target after import at %d (%v), want %d", v, err, latest)
	}
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT case_name FROM fraud_cases`).Scan(&name); err != nil || name != "Old schema case" {
		t.Fatalf("imported case = %q %v", name, err)
	}
	var paused bool
	if err := pool.QueryRow(context.Background(), `SELECT ingestion_paused FROM tracker_instance`).Scan(&paused); err != nil || !paused {
		t.Fatalf("instance after import: paused=%v %v", paused, err)
	}
}

// A failed pg_dump stops --replace before anything changes and leaves no
// partial dump behind.
func TestImportReplaceStopsOnFailedDump(t *testing.T) {
	a := storetest.Open(t)
	seed(t, a)
	path, _ := exportFile(t, a)
	b := storetest.Open(t)
	ctx := context.Background()
	if _, err := b.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "Local case", TypologyRaw: "fraud"}}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "pg_dump")
	script := "#!/bin/sh\nfor a in \"$@\"; do case $a in --file=*) echo partial > \"${a#--file=}\";; esac; done\necho 'version mismatch' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	backups := t.TempDir()
	before := targetRows(t, b)
	_, err := ImportFile(ctx, b.Pool, path, ImportOptions{Replace: true, BackupDir: backups, PGDump: bin,
		DSN: "postgres://unused/db", Log: quietLog()})
	if err == nil || !strings.Contains(err.Error(), "nothing changed") || !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("failed dump = %v", err)
	}
	if left, _ := os.ReadDir(backups); len(left) != 0 {
		t.Errorf("partial backup left behind: %v", left)
	}
	if after := targetRows(t, b); !reflect.DeepEqual(before, after) {
		t.Errorf("target changed after a failed backup: %v -> %v", before, after)
	}
}
