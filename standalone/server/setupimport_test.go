// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// sourceBundle exports a one-case dataset and returns the bundle's bytes.
func sourceBundle(t *testing.T) ([]byte, bundle.Manifest) {
	t.Helper()
	src := storetest.Open(t)
	if _, err := src.CreateFraudCase(context.Background(), store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Imported via wizard", TypologyRaw: "tax fraud"}}); err != nil {
		t.Fatal(err)
	}
	path, m, err := bundle.Export(context.Background(), src.Pool, t.TempDir(), bundle.ExportOptions{
		Source: bundle.Source{InstanceID: bundle.NewID(), Kind: bundle.KindStandalone}, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

func (b *browser) upload(tokenPage, path string, file []byte) *http.Response {
	b.t.Helper()
	return b.uploadWith(tokenPage, path, file, nil)
}

func (b *browser) uploadWith(tokenPage, path string, file []byte, fields map[string]string) *http.Response {
	b.t.Helper()
	_, page := b.get(tokenPage)
	m := csrfField.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatalf("no csrf token on %s", tokenPage)
	}
	// localauth reads a multipart form's CSRF token from the query string,
	// so the check never has to parse a large upload.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("bundle", "export.tar.gz")
	_, _ = fw.Write(file)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, b.base+path+"?csrf_token="+url.QueryEscape(m[1]), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func freshServer(t *testing.T, mod func(*server.Options)) (*server.Server, *browser) {
	t.Helper()
	st := storetest.Open(t)
	o := server.Options{Log: quiet(), DataDir: t.TempDir()}
	if mod != nil {
		mod(&o)
	}
	s, err := server.Build(context.Background(), st.Pool, o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	return s, newBrowser(t, srv.URL)
}

func cases(t *testing.T, s *server.Server) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM fraud_cases`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSetupWizardStartsFromAnExport(t *testing.T) {
	data, m := sourceBundle(t)
	s, b := freshServer(t, nil)

	// A corrupt upload is rejected and imports nothing.
	bad := append([]byte{}, data...)
	bad[len(bad)/2] ^= 0xff
	resp := b.upload("/setup", server.SetupImportPath, bad)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("corrupt upload = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cases(t, s) != 0 {
		t.Fatal("corrupt upload imported data")
	}

	resp = b.upload("/setup", server.SetupImportPath, data)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup?imported="+m.ExportID {
		t.Fatalf("upload = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cases(t, s) != 1 {
		t.Fatal("upload did not import the case")
	}
	if paused, _ := s.Store.IngestionPaused(context.Background()); !paused {
		t.Error("ingestion not paused after a wizard import")
	}

	// The wizard carries on: create the admin; afterwards the upload is gone.
	if resp, _ := b.post("/setup", "/setup", map[string][]string{"username": {"admin"},
		"password": {adminPassword}, "password_confirm": {adminPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup after import = %d", resp.StatusCode)
	}
	if resp := b.upload("/admin/ingestion/", server.SetupImportPath, data); resp.StatusCode != http.StatusNotFound {
		t.Errorf("upload after setup = %d, want 404", resp.StatusCode)
	}
}

func TestSetupImportRefusesOversizeUploads(t *testing.T) {
	data, _ := sourceBundle(t)
	s, b := freshServer(t, func(o *server.Options) { o.MaxImportBytes = 1024 })
	resp := b.upload("/setup", server.SetupImportPath, data)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("oversize upload = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cases(t, s) != 0 {
		t.Fatal("oversize upload imported data")
	}
	var flash string
	for _, c := range resp.Cookies() {
		if strings.Contains(c.Name, "flash") {
			flash = c.Value
		}
	}
	if flash == "" {
		t.Error("no flash explaining the refusal")
	}
}

// The wizard import restores the source's settings, and its secrets when the
// passphrase is given, re-sealed under this instance's key.
func TestSetupWizardImportsSettingsAndSecrets(t *testing.T) {
	src := storetest.Open(t)
	ctx := context.Background()
	cfg, err := settings.Open(ctx, src.Pool, settings.Options{LookupEnv: func(string) (string, bool) { return "", false },
		DataDir: t.TempDir(), Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(ctx, nil, []settings.Change{{Name: settings.SiteName, Value: "Handed-over Watch"},
		{Name: settings.LLMAPIKey, Value: "sk-wizard-secret-987654"}}); err != nil {
		t.Fatal(err)
	}
	const pass = "wizard handoff passphrase"
	path, _, err := bundle.Export(ctx, src.Pool, t.TempDir(), bundle.ExportOptions{
		Source: bundle.Source{InstanceID: bundle.NewID(), Kind: bundle.KindStandalone}, Log: quiet(),
		IncludeSecrets: true, Secrets: cfg, Passphrase: pass})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s, b := freshServer(t, nil)
	resp := b.uploadWith("/setup", server.SetupImportPath, data, map[string]string{"passphrase": pass})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/setup?imported=") {
		t.Fatalf("upload = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if s.SettingsStore == nil {
		t.Fatal("server has no settings store")
	}
	if got := s.SettingsStore.Value(settings.SiteName); got != "Handed-over Watch" {
		t.Errorf("site name after wizard import = %q", got)
	}
	if got := s.SettingsStore.Value(settings.LLMAPIKey); got != "sk-wizard-secret-987654" {
		t.Errorf("LLM key after wizard import = %q", got)
	}
}
