// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	sitelayout "github.com/lonelycode/corruption-tracker/standalone/layout"
	"github.com/lonelycode/corruption-tracker/standalone/server"
)

// anonymousRoutes is every page and form target the standalone server
// mounts, with what an anonymous visitor may get once setup is done.
// Design D3: a private instance exposes only /login, /setup (until the
// first admin exists), /about, /healthz and static assets; everything else
// is a sign-in redirect or a 404 that confirms nothing.
var anonymousRoutes = []struct {
	method, path string
	want         int // 200 = public, 303 = sign-in redirect, 404 = not found
}{
	{"GET", "/healthz", 200},
	{"GET", "/about", 200},
	{"GET", "/login", 200},
	{"GET", "/static/standalone/base.css", 200},
	{"GET", "/static/corruption-tracker/tracker.css", 200},
	{"GET", "/setup", 404}, // closed after the first admin
	{"GET", "/", 303},      // → the report, which then asks for sign-in
	{"GET", "/tracker/corruption", 303},
	{"GET", "/tracker/corruption?period=2024-W23", 303},
	{"GET", "/tracker/corruption/case/1", 303},
	{"GET", "/tracker/corruption/explorer", 303},
	{"GET", "/tracker/corruption/explorer?format=csv", 303},
	{"GET", "/tracker/corruption/export?period=2024-W23&format=csv", 404},
	{"GET", "/admin/fraud", 303},
	{"GET", "/admin/fraud/claims", 303},
	{"GET", "/admin/fraud/cases/1", 303},
	{"GET", "/admin/fraud/cases/1/history", 303},
	{"GET", "/admin/fraud/appeals", 303},
	{"GET", "/admin/fraud/submit", 303},
	{"GET", "/admin/users", 303},
	{"GET", "/admin/ingestion", 303},
	{"GET", "/no/such/page", 404},
}

// Nothing about the dataset reaches an anonymous visitor: only the sign-in
// page, /about (the AGPL source offer), health and stylesheets answer, and
// none of those carry case data.
func TestAnonymousSeesOnlyPublicPages(t *testing.T) {
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	seedCases(t, s.Store)
	anon := newBrowser(t, base)
	for _, rt := range anonymousRoutes {
		req, _ := http.NewRequest(rt.method, base+rt.path, nil)
		resp, err := anon.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != rt.want {
			t.Errorf("anonymous %s %s = %d, want %d", rt.method, rt.path, resp.StatusCode, rt.want)
			continue
		}
		if loc := resp.Header.Get("Location"); rt.want == http.StatusSeeOther && !strings.HasPrefix(loc, "/login?next=") &&
			!(rt.path == "/" && loc == "/tracker/corruption") {
			t.Errorf("anonymous %s %s → %q, want the sign-in page", rt.method, rt.path, resp.Header.Get("Location"))
		}
		// Seeded names and labels never leak into what anonymous visitors get.
		for _, secret := range []string{"Vera Verified", "Alan Alleged", "Petra Pending", "Director sentenced"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("anonymous %s %s leaked %q", rt.method, rt.path, secret)
			}
		}
	}
}

// The report page draws its charts with the vendored ECharts bundle, served
// by the instance itself (no third-party CDN).
func TestReportLoadsVendoredCharts(t *testing.T) {
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	seedCases(t, s.Store)
	b := newBrowser(t, base)
	b.login(t, "viewer")
	_, body := b.get("/tracker/corruption?period=2024-W23")
	want := `<script src="` + sitelayout.ChartScript + `"></script>`
	if !strings.Contains(body, want) {
		t.Fatalf("report page lacks %s", want)
	}
	if resp, _ := b.get(sitelayout.ChartScript); resp.StatusCode != http.StatusOK {
		t.Errorf("%s = %d", sitelayout.ChartScript, resp.StatusCode)
	}
}

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A CSRF token sent in a multipart form's URL never reaches the server's
// logs or a redirect target, and every page tells browsers not to leak
// URLs to other sites.
func TestQueryTokenNeverLogged(t *testing.T) {
	var logs syncBuffer
	s, base := serving(t, server.Options{Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	seedAccounts(t, s.Accounts)
	b := newBrowser(t, base)
	resp, page := b.get("/login")
	if resp.Header.Get("Referrer-Policy") != "same-origin" {
		t.Errorf("Referrer-Policy = %q", resp.Header.Get("Referrer-Policy"))
	}
	token := csrfField.FindStringSubmatch(page)[1]
	for _, path := range []string{"/admin/fraud/submit", "/admin/users", "/setup/import", "/no/such/page"} {
		req, _ := http.NewRequest(http.MethodPost, base+path+"?csrf_token="+url.QueryEscape(token), strings.NewReader("--X--\r\n"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=X")
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if strings.Contains(resp.Header.Get("Location"), token) {
			t.Errorf("POST %s redirected with the token in %q", path, resp.Header.Get("Location"))
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("server logs contain the CSRF token:\n%s", logs.String())
	}
}
