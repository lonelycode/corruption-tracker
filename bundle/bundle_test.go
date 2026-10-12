// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

var pinnedNow = time.Date(2026, 11, 2, 3, 14, 15, 0, time.UTC)

func pinned(log *slog.Logger) ExportOptions {
	return ExportOptions{
		Source:     Source{InstanceID: "5f0c1e2a-0000-4000-8000-000000000001", Kind: KindStandalone, BaseURL: "https://tracker.example"},
		AppVersion: "test",
		Now:        func() time.Time { return pinnedNow },
		NewID:      func() string { return "b3f6a9c0-0000-4000-8000-000000000002" },
		Log:        log,
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// seed writes a small synthetic dataset touching actors, cases, their child
// rows, a claim and a case revision.
func seed(t *testing.T, st *store.Store) []int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "test:reviewer", Label: "Reviewer Rowan", Kind: "user"}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC)
	var ids []int64
	for _, name := range []string{"Quillfeather", "Marrowby"} {
		c, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case: domain.FraudCase{CaseName: "Case " + name, TypologyRaw: "tax fraud", Summary: "line one\nline two \"quoted\""},
			Participants: []store.FraudParticipantInput{{
				PersonName: "Pat " + name, Participant: domain.FraudCaseParticipant{Role: "defendant"},
			}},
			Organisations: []store.FraudOrgInput{{Name: name + " Holdings Ltd", Role: "vehicle"}},
			Events:        []domain.FraudCaseEvent{{EventType: domain.EventSentencing, EventDate: &day, OutcomeDetail: "2 years"}},
			Sources: []domain.FraudCaseSource{{URL: "https://example.test/" + name, SourceType: domain.FraudSourceAgencyRelease,
				Publisher: "SFO", ContentHash: "hash-" + name}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if err := st.SetFraudCaseStatusOverride(ctx, ids[0], "discontinued", "editor@test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateFraudClaim(ctx, domain.FraudClaim{
		Headline: "Synthetic claim", SourceType: domain.FraudSourceManual, SourcePublisher: "Test",
		LicenceBasis: "test", ContentHash: "claim-1",
	}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	return ids
}

func exportBytes(t *testing.T, st *store.Store, opts ExportOptions) ([]byte, Manifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := Write(context.Background(), st.Pool, &buf, opts)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.Bytes(), m
}

type entry struct {
	hdr  tar.Header
	body []byte
}

func unpack(t *testing.T, archive []byte) []entry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{*h, b})
	}
}

func repack(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExportTwiceIsByteStable(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)

	a, ma := exportBytes(t, st, pinned(quietLog()))
	b, mb := exportBytes(t, st, pinned(quietLog()))

	ea, eb := unpack(t, a), unpack(t, b)
	if len(ea) != len(eb) {
		t.Fatalf("entry counts differ: %d vs %d", len(ea), len(eb))
	}
	for i := range ea {
		if ea[i].hdr.Name != eb[i].hdr.Name {
			t.Fatalf("entry %d: %s vs %s", i, ea[i].hdr.Name, eb[i].hdr.Name)
		}
		if ea[i].hdr.Name == ManifestFile {
			continue
		}
		if !bytes.Equal(ea[i].body, eb[i].body) {
			t.Errorf("%s differs between two exports of the same data", ea[i].hdr.Name)
		}
	}
	// The manifests differ at most in the snapshot ids, which name the
	// transaction rather than the data.
	ma.Snapshot, mb.Snapshot = Snapshot{}, Snapshot{}
	ja, _ := json.Marshal(ma)
	jb, _ := json.Marshal(mb)
	if !bytes.Equal(ja, jb) {
		t.Errorf("manifests differ beyond the snapshot block:\n%s\n%s", ja, jb)
	}
	// When the snapshot ids also agree (nothing else wrote in between),
	// the whole archive is byte-identical.
	if ea[0].hdr.Name == ManifestFile && bytes.Equal(ea[0].body, eb[0].body) && !bytes.Equal(a, b) {
		t.Error("identical entries but different archive bytes")
	}
}

// The container itself is deterministic: the same staged files and
// manifest always produce the same archive bytes.
func TestWriteArchiveIsDeterministic(t *testing.T) {
	stage := t.TempDir()
	if err := os.Mkdir(filepath.Join(stage, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, _ := writeStaged(filepath.Join(stage, SettingsFile), []byte("{}\n"))
	tsum, _ := writeStaged(filepath.Join(stage, "tables/fraud_cases.ndjson"), []byte(`{"id":1}`+"\n"))
	m := Manifest{Format: Format, FormatVersion: 1, Settings: FileRef{File: SettingsFile, SHA256: sum},
		Tables: []TableEntry{{Name: "fraud_cases", File: "tables/fraud_cases.ndjson", Rows: 1, SHA256: tsum}}}
	var a, b bytes.Buffer
	if err := writeArchive(&a, stage, m, pinnedNow); err != nil {
		t.Fatal(err)
	}
	if err := writeArchive(&b, stage, m, pinnedNow); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("same input, different archive bytes")
	}
	for _, e := range unpack(t, a.Bytes()) {
		if e.hdr.Mode != 0o644 || e.hdr.Uid != 0 || e.hdr.Gid != 0 || !e.hdr.ModTime.Equal(pinnedNow) {
			t.Errorf("%s header = mode %o uid %d gid %d mtime %s", e.hdr.Name, e.hdr.Mode, e.hdr.Uid, e.hdr.Gid, e.hdr.ModTime)
		}
	}
}

func TestManifestMatchesSpec(t *testing.T) {
	st := storetest.Open(t)
	caseIDs := seed(t, st)
	archive, m := exportBytes(t, st, pinned(quietLog()))

	if m.Format != Format || m.FormatVersion != 1 || m.ExportID == "" || m.CreatedAt != "2026-11-02T03:14:15Z" {
		t.Errorf("header fields = %+v", m)
	}
	var want uint
	if err := st.Pool.QueryRow(context.Background(), `SELECT version FROM tracker_schema_migrations`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != want || m.SchemaVersion < 4 {
		t.Errorf("schema_version = %d, want %d", m.SchemaVersion, want)
	}
	if m.Snapshot.Isolation != "repeatable read" || m.Snapshot.PGSnapshot == "" || m.Snapshot.PGServerVersion == "" {
		t.Errorf("snapshot = %+v", m.Snapshot)
	}
	if m.Handoff != nil || m.Secrets != nil || m.Options != (Options{}) {
		t.Errorf("handoff/secrets/options = %v %v %+v", m.Handoff, m.Secrets, m.Options)
	}

	// Tables in load order, every v1 table accounted for.
	var names []string
	byName := map[string]TableEntry{}
	for _, te := range m.Tables {
		names = append(names, te.Name)
		byName[te.Name] = te
	}
	if !slices.IsSortedFunc(names, func(a, b string) int {
		return slices.Index(TableSet, a) - slices.Index(TableSet, b)
	}) {
		t.Errorf("tables out of load order: %v", names)
	}
	omitted := map[string]string{}
	for _, o := range m.OmittedTables {
		omitted[o.Name] = o.Reason
	}
	for _, name := range TableSet {
		if _, ok := byName[name]; !ok && omitted[name] == "" {
			t.Errorf("%s neither exported nor omitted", name)
		}
	}
	if omitted[AccessLogTable] != "option include_access_log=false" {
		t.Errorf("access log omission = %q", omitted[AccessLogTable])
	}
	for _, name := range []string{"tracker_actors", "fraud_cases", "fraud_case_revisions", "tracker_job_runs"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("%s not exported", name)
		}
	}

	cases := byName["fraud_cases"]
	if cases.Rows != int64(len(caseIDs)) || !slices.Equal(cases.PK, []string{"id"}) ||
		cases.MaxID == nil || *cases.MaxID != slices.Max(caseIDs) {
		t.Errorf("fraud_cases entry = rows %d pk %v max_id %v", cases.Rows, cases.PK, cases.MaxID)
	}
	if cases.Columns[0] != (Column{Name: "id", Type: "bigint"}) {
		t.Errorf("fraud_cases first column = %+v", cases.Columns[0])
	}
	typ := byName["fraud_case_typologies"]
	if !slices.Equal(typ.PK, []string{"case_id", "typology_id"}) || typ.MaxID != nil {
		t.Errorf("fraud_case_typologies pk %v max_id %v", typ.PK, typ.MaxID)
	}
	if runs := byName["tracker_job_runs"]; runs.Rows != 0 || runs.MaxID != nil || runs.SHA256 != fmt.Sprintf("%x", sha256Of(nil)) {
		t.Errorf("empty table entry = %+v", runs)
	}

	// Row lines: keys in column order, one object per line, newline-terminated.
	for _, e := range unpack(t, archive) {
		if e.hdr.Name != cases.File {
			continue
		}
		lines := strings.Split(string(e.body), "\n")
		if lines[len(lines)-1] != "" || len(lines)-1 != int(cases.Rows) {
			t.Fatalf("fraud_cases file is not %d newline-terminated lines", cases.Rows)
		}
		dec := json.NewDecoder(strings.NewReader(lines[0]))
		if tok, _ := dec.Token(); tok != json.Delim('{') {
			t.Fatal("line is not an object")
		}
		for i := 0; dec.More(); i++ {
			key, _ := dec.Token()
			if key != cases.Columns[i].Name {
				t.Fatalf("key %d = %v, want %s", i, key, cases.Columns[i].Name)
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatal(err)
			}
		}
	}
	if FileName(m) != "corruption-tracker-5f0c1e2a-20261102T031415Z.tar.gz" {
		t.Errorf("file name = %s", FileName(m))
	}
}

// Every tracker table in the schema is either in the v1 set or explicitly
// never exported, so a new table can't silently fall out of bundles.
func TestEveryTrackerTableIsClassified(t *testing.T) {
	st := storetest.Open(t)
	rows, err := st.Pool.Query(context.Background(), `
		SELECT tablename FROM pg_tables
		 WHERE schemaname = current_schema() AND (tablename LIKE 'fraud\_%' OR tablename LIKE 'tracker\_%')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(TableSet, name) && name != AccessLogTable && !slices.Contains(NeverExported, name) {
			t.Errorf("table %s is neither in bundle.TableSet nor bundle.NeverExported", name)
		}
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	archive, _ := exportBytes(t, st, pinned(quietLog()))
	if _, err := Verify(bytes.NewReader(archive)); err != nil {
		t.Fatalf("clean bundle failed verification: %v", err)
	}

	entries := unpack(t, archive)
	tableIdx := slices.IndexFunc(entries, func(e entry) bool { return e.hdr.Name == "tables/fraud_cases.ndjson" })
	clone := func() []entry {
		out := make([]entry, len(entries))
		for i, e := range entries {
			out[i] = entry{e.hdr, slices.Clone(e.body)}
		}
		return out
	}
	withManifest := func(edit func(*Manifest)) []entry {
		es := clone()
		var m Manifest
		if err := json.Unmarshal(es[0].body, &m); err != nil {
			t.Fatal(err)
		}
		edit(&m)
		es[0].body, _ = json.Marshal(m)
		return es
	}

	cases := map[string]struct {
		archive []byte
		want    string
	}{
		"flipped byte in a table": {func() []byte {
			es := clone()
			es[tableIdx].body[5] ^= 0x01
			return repack(t, es)
		}(), "checksum mismatch"},
		"missing table file": {repack(t, slices.Delete(clone(), tableIdx, tableIdx+1)), "missing or out of order"},
		"missing last file":  {repack(t, clone()[:len(entries)-1]), "is missing"},
		"extra entry":        {repack(t, append(clone(), entry{tar.Header{Name: "tables/extra.ndjson", Mode: 0o644, Typeflag: tar.TypeReg}, []byte("{}\n")})), "unexpected entry"},
		"manifest not first": {func() []byte {
			es := clone()
			es[0], es[1] = es[1], es[0]
			return repack(t, es)
		}(), "first entry"},
		"newer format": {repack(t, withManifest(func(m *Manifest) { m.FormatVersion = 2 })), "upgrade first"},
		"row count lie": {repack(t, withManifest(func(m *Manifest) {
			for i := range m.Tables {
				if m.Tables[i].Name == "fraud_cases" {
					m.Tables[i].Rows++
				}
			}
		})), "rows, manifest says"},
		"unaccounted table": {repack(t, withManifest(func(m *Manifest) {
			m.OmittedTables = slices.DeleteFunc(m.OmittedTables, func(o OmittedTable) bool { return o.Name == AccessLogTable })
		})), "neither exported nor listed as omitted"},
		"unknown manifest field": {func() []byte {
			es := clone()
			es[0].body = bytes.Replace(es[0].body, []byte(`"format":`), []byte(`"surprise":1,"format":`), 1)
			return repack(t, es)
		}(), "unknown field"},
		"corrupt gzip": {func() []byte {
			b := slices.Clone(archive)
			b[len(b)/2] ^= 0xff
			return b
		}(), ""},
	}
	for name, tc := range cases {
		_, err := Verify(bytes.NewReader(tc.archive))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Verify = %v, want error containing %q", name, err, tc.want)
		}
	}
}

// A writer committing mid-export must not produce a torn bundle: the case
// it adds after fraud_cases was read must not show up through its
// participants either.
func TestConcurrentWriterDoesNotTearTheBundle(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	ctx := context.Background()

	var added int64
	opts := pinned(quietLog())
	opts.afterTable = func(table string) {
		if table != "fraud_cases" {
			return
		}
		c, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case: domain.FraudCase{CaseName: "Late case", TypologyRaw: "bribery"},
			Participants: []store.FraudParticipantInput{{
				PersonName: "Late Person", Participant: domain.FraudCaseParticipant{Role: "defendant"},
			}},
		})
		if err != nil {
			t.Errorf("concurrent write: %v", err)
			return
		}
		added = c.ID
	}
	archive, m := exportBytes(t, st, opts)
	if added == 0 {
		t.Fatal("concurrent write did not happen")
	}
	var dbParticipants int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_case_participants`).Scan(&dbParticipants); err != nil {
		t.Fatal(err)
	}
	for _, e := range unpack(t, archive) {
		switch e.hdr.Name {
		case "tables/fraud_case_participants.ndjson", "tables/fraud_cases.ndjson", "tables/fraud_persons.ndjson":
			if bytes.Contains(e.body, []byte("Late")) || bytes.Contains(e.body, []byte(fmt.Sprintf(`"case_id":%d,`, added))) {
				t.Errorf("%s includes a row committed after the snapshot", e.hdr.Name)
			}
		}
	}
	for _, te := range m.Tables {
		if te.Name == "fraud_case_participants" && te.Rows != dbParticipants-1 {
			t.Errorf("participants rows = %d, want %d (snapshot excludes the late one)", te.Rows, dbParticipants-1)
		}
	}
	if _, err := Verify(bytes.NewReader(archive)); err != nil {
		t.Fatalf("bundle exported under concurrent writes fails verification: %v", err)
	}
}

func TestExportFileAndNoPIIInLogs(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	var logs bytes.Buffer
	opts := pinned(slog.New(slog.NewTextHandler(&logs, nil)))
	dir := t.TempDir()
	path, m, err := Export(context.Background(), st.Pool, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != FileName(m) {
		t.Errorf("path = %s", path)
	}
	if _, err := VerifyFile(path); err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".corruption-tracker-export-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
	out := logs.String()
	for _, pii := range []string{"Quillfeather", "Marrowby", "Pat ", "Rowan", "Holdings", "example.test"} {
		if strings.Contains(out, pii) {
			t.Errorf("export log contains %q:\n%s", pii, out)
		}
	}
	if !strings.Contains(out, "tracker export written") {
		t.Errorf("no summary log line:\n%s", out)
	}
}

func TestExportRefusals(t *testing.T) {
	st := storetest.Open(t)
	for name, edit := range map[string]func(*ExportOptions){
		"secrets on alitheia": func(o *ExportOptions) { o.Source.Kind = KindAlitheiaEmbedded; o.IncludeSecrets = true },
		"secrets standalone":  func(o *ExportOptions) { o.IncludeSecrets = true },
		"unknown kind":        func(o *ExportOptions) { o.Source.Kind = "other" },
		"no instance id":      func(o *ExportOptions) { o.Source.InstanceID = "" },
	} {
		opts := pinned(quietLog())
		edit(&opts)
		if _, err := Write(context.Background(), st.Pool, io.Discard, opts); err == nil {
			t.Errorf("%s: export did not refuse", name)
		}
	}
}

func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }

// Manifest row counts equal count(*) (no writes in between, so the live
// count is the snapshot's), and nothing secret is in the bundle by default.
func TestManifestCountsMatchAndNoSecretsByDefault(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	archive, m := exportBytes(t, st, pinned(quietLog()))
	ctx := context.Background()
	for _, te := range m.Tables {
		var n int64
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+te.Name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != te.Rows {
			t.Errorf("%s: manifest rows %d, count(*) %d", te.Name, te.Rows, n)
		}
	}
	if m.Secrets != nil || m.Options.IncludeSecrets {
		t.Errorf("secrets in a default export: %+v %+v", m.Secrets, m.Options)
	}
	for _, e := range unpack(t, archive) {
		if e.hdr.Name == SecretsFile {
			t.Error("secrets.enc present in a default export")
		}
		if e.hdr.Name == SettingsFile && string(e.body) != "{}\n" {
			t.Errorf("settings.json = %q", e.body)
		}
		for _, never := range NeverExported {
			if e.hdr.Name == "tables/"+never+".ndjson" {
				t.Errorf("%s exported", never)
			}
		}
	}
}
