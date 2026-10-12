// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"bytes"
	"context"
	"flag"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// The operator guide (docs/operator-guide.md) has readers start a test
// instance from docs/sample/sample-bundle.tar.gz: a small, entirely
// fictional dataset. This test keeps that file importable as the bundle
// format and schema move on, and keeps every button, page and field the
// guide names (in bold) present in the UI. Regenerate the sample with
//
//	DATABASE_URL_TEST=… go test ./standalone/server -run TestSampleBundle -update-sample-bundle

var updateSampleBundle = flag.Bool("update-sample-bundle", false, "rewrite docs/sample/sample-bundle.tar.gz")

var sampleBundlePath = filepath.Join("..", "..", "docs", "sample", "sample-bundle.tar.gz")

func TestSampleBundle(t *testing.T) {
	if *updateSampleBundle {
		writeSampleBundle(t)
	}
	m, err := bundle.VerifyFile(sampleBundlePath)
	if err != nil {
		t.Fatalf("docs sample bundle no longer verifies (regenerate with -update-sample-bundle): %v", err)
	}
	data, err := os.ReadFile(sampleBundlePath)
	if err != nil {
		t.Fatal(err)
	}

	// The guide's path: a fresh instance, the wizard's "Start from an
	// export", then the administrator.
	s, base := serving(t, server.Options{})
	b := newBrowser(t, base)
	_, page := b.get("/setup")
	ui := []string{page}
	token := csrfField.FindStringSubmatch(page)[1]
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("bundle", "sample-bundle.tar.gz")
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+server.SetupImportPath+"?csrf_token="+url.QueryEscape(token), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup?imported="+m.ExportID {
		t.Fatalf("wizard import of the sample = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := b.post("/setup", "/setup", url.Values{"username": {"admin"},
		"password": {userPassword}, "password_confirm": {userPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup after import = %d", resp.StatusCode)
	}
	for _, path := range []string{"/tracker/corruption", "/tracker/corruption/case/1", "/admin/fraud/claims?state=all"} {
		if resp, body := b.get(path); resp.StatusCode != http.StatusOK || strings.Contains(body, "No report has been generated") {
			t.Errorf("%s after importing the sample = %d", path, resp.StatusCode)
		}
	}
	var cases int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM fraud_cases`).Scan(&cases); err != nil || cases < 3 {
		t.Errorf("sample cases imported = %d (err %v)", cases, err)
	}

	for _, path := range []string{"/admin/users", "/admin/settings", "/admin/ingestion", "/admin/backups",
		"/admin/handoff", "/admin/fraud/claims?state=all"} {
		_, body := b.get(path)
		ui = append(ui, body)
	}
	checkGuideNamesUI(t, ui)
}

// guideBold is a bold phrase in the guide. Those ending in punctuation
// ("**Log in to the server.**") are lead-ins, not UI names.
var guideBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)

// checkGuideNamesUI fails for any name the operator guide puts in bold that
// appears neither in the rendered pages nor in a template (for text only
// shown in another state: frozen, quarantined, an unreadable secret).
func checkGuideNamesUI(t *testing.T, pages []string) {
	t.Helper()
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "operator-guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus strings.Builder
	for _, p := range pages {
		corpus.WriteString(html.UnescapeString(p))
	}
	for _, glob := range []string{"templates/*.tmpl", "../localauth/templates/*.tmpl"} {
		files, _ := filepath.Glob(glob)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			corpus.Write(b)
		}
	}
	text := strings.Join(strings.Fields(corpus.String()), " ")
	seen := map[string]bool{}
	for _, m := range guideBold.FindAllStringSubmatch(string(guide), -1) {
		name := strings.Join(strings.Fields(m[1]), " ")
		if seen[name] || strings.ContainsAny(name[len(name)-1:], ".,:;?!") {
			continue
		}
		seen[name] = true
		if !strings.Contains(text, name) {
			t.Errorf("the operator guide names %q, but no page or template shows it", name)
		}
	}
	if len(seen) < 40 {
		t.Errorf("found only %d UI names in the guide; has its bold convention changed?", len(seen))
	}
}

// writeSampleBundle builds the fictional sample dataset and exports it with
// a pinned clock and ids (row timestamps still come from the database).
func writeSampleBundle(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	seedCases(t, st)
	seeder, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "system:seed", Label: "seed", Kind: "system"})
	if err != nil {
		t.Fatal(err)
	}
	// Case 3 verified too, and a claim waiting in quarantine.
	if _, err := st.VerifyFraudCase(ctx, 3, seeder); err != nil {
		t.Fatal(err)
	}
	q, _, err := st.CreateFraudClaim(ctx, domain.FraudClaim{Headline: "Court report mentions a suppression order",
		SourceType: domain.FraudSourceNews, ContentHash: "sample-quarantine",
		Payload: []byte(`{"is_fraud_case":true,"case_stage":"charged"}`)}, nil, "seed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{ClaimID: q.ID, To: domain.ReviewQuarantined,
		ReviewerID: &seeder, ReviewerLabel: "seed", Note: "suppression mentioned"}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ids := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004"}
	n := 0
	path, _, err := bundle.Export(ctx, st.Pool, t.TempDir(), bundle.ExportOptions{
		Source:     bundle.Source{InstanceID: "00000000-0000-4000-8000-0000000000aa", Kind: bundle.KindStandalone},
		AppVersion: "sample",
		Now:        func() time.Time { return at },
		NewID:      func() string { n++; return ids[(n-1)%len(ids)] },
		Log:        quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sampleBundlePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sampleBundlePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
