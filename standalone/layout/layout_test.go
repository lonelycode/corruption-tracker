// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package layout_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/readmodel"
	"github.com/lonelycode/corruption-tracker/standalone/accesslog"
	"github.com/lonelycode/corruption-tracker/standalone/layout"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
	"github.com/lonelycode/corruption-tracker/version"
	"github.com/lonelycode/corruption-tracker/web"
	"github.com/lonelycode/corruption-tracker/web/reader"
)

const (
	testSource = "https://git.example.org/ops/tracker"
	testCommit = "0123456789abcdef"
	password   = "correct horse battery"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type instance struct {
	srv *httptest.Server
	st  *store.Store
	svc *localauth.Service
}

// newInstance wires the standalone stack the way the binary will: local
// auth, this layout, the tracker's back office and reader, the stylesheets.
func newInstance(t *testing.T, brand layout.Branding) *instance {
	t.Helper()
	return newInstanceWith(t, layout.StaticBranding(brand))
}

// newInstanceWith wires the stack with a live branding source (the server
// passes the settings service, so renames apply without a restart).
func newInstanceWith(t *testing.T, src layout.BrandingSource) *instance {
	t.Helper()
	st := storetest.Open(t)
	svc := localauth.NewService(st.Pool)
	authz := localauth.Authorizer()
	lay, err := layout.New(layout.Options{Auth: authz, CSRFToken: localauth.CSRFToken,
		Branding: src, SourceURL: testSource, Version: "v9.9.9", Commit: testCommit, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	a, err := localauth.New(localauth.Options{Service: svc, Layout: lay,
		Cookies: localauth.CookieSecureNever, LoginBurst: 1000, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	audit := accesslog.Auditor{Pool: st.Pool}
	wh, err := web.New(web.Deps{Store: st, Auth: authz, Audit: audit, Layout: lay, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := reader.New(reader.Options{Auth: authz, Layout: lay, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(a.CSRF, a.Session, a.SetupGate)
	a.Routes(r)
	r.Get("/about", lay.About)
	rd.Mount(r, readmodel.New(readmodel.Options{Store: st, Auth: authz, Audit: audit, Log: quiet,
		NoticeFunc: func(r *http.Request) string { return readmodel.NoticeFor(src.Branding(r.Context()).Name) }}))
	r.Mount("/admin/fraud", wh.Admin)
	r.Handle(web.StaticPrefix+"*", wh.Static)
	r.Handle(layout.StaticPrefix+"*", layout.StaticHandler())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &instance{srv: srv, st: st, svc: svc}
}

type client struct {
	t   *testing.T
	in  *instance
	c   *http.Client
	url *url.URL
}

func (in *instance) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(in.srv.URL)
	return &client{t: t, in: in, url: u, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) get(path string) (*http.Response, string) {
	c.t.Helper()
	resp, err := c.c.Get(c.in.srv.URL + path)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (c *client) post(path string, form url.Values) (*http.Response, string) {
	c.t.Helper()
	var token string
	for _, ck := range c.c.Jar.Cookies(c.url) {
		if ck.Name == "tracker_csrf" {
			token = ck.Value
		}
	}
	if token == "" {
		c.get("/about")
		return c.post(path, form)
	}
	form.Set("csrf_token", token)
	resp, err := c.c.PostForm(c.in.srv.URL+path, form)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (c *client) login(user string) {
	c.t.Helper()
	if resp, body := c.post("/login", url.Values{"username": {user}, "password": {password}}); resp.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("login %s: %d %s", user, resp.StatusCode, body)
	}
}

func (in *instance) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	admin, err := in.svc.Setup(ctx, "admin", password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.svc.CreateUser(ctx, admin, localauth.NewUser{Username: "viewer", Password: password,
		Role: localauth.RoleViewer, Note: "agreement #1"}); err != nil {
		t.Fatal(err)
	}
	day := func(s string) *time.Time { v, _ := time.Parse("2006-01-02", s); return &v }
	for i, ev := range []domain.FraudEventType{domain.EventSentencing, domain.EventChargesLaid} {
		c, err := in.st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case: domain.FraudCase{CaseName: "Case", TypologyRaw: "tax fraud"},
			Participants: []store.FraudParticipantInput{{PersonName: "Pat Person",
				Participant: domain.FraudCaseParticipant{Role: "defendant"}}},
			Events:  []domain.FraudCaseEvent{{EventType: ev, EventDate: day("2024-06-04")}},
			Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceAgencyRelease, ContentHash: "h" + string(rune('a'+i))}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := in.st.VerifyFraudCase(ctx, c.ID, admin.ActorID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := in.st.CreateFraudClaim(ctx, domain.FraudClaim{Headline: "A claim", SourceType: domain.FraudSourceNews,
		ContentHash: "claim", Payload: []byte(`{"is_fraud_case":true}`)}, nil, "seed"); err != nil {
		t.Fatal(err)
	}
	b := &fraudreport.Builder{DB: in.st, Log: quiet, IncludeUnverified: true}
	if _, _, err := b.Build(ctx, "2024-W23", "seed"); err != nil {
		t.Fatal(err)
	}
}

var alitheiaBrand = regexp.MustCompile(`(?i)open ?brief|alitheia`)

// Every tracker page renders inside the standalone shell: our stylesheets,
// the instance name, the source offer in the footer, and nothing of
// alitheia's.
func TestEveryPageRendersInTheStandaloneShell(t *testing.T) {
	in := newInstance(t, layout.Branding{Name: "Example Watch", Accent: "#0a7f5a"})
	in.seed(t)
	admin := in.client(t)
	admin.login("admin")
	anon := in.client(t)
	pages := []struct {
		c    *client
		path string
	}{
		{anon, "/login"}, {anon, "/about"},
		{admin, "/tracker/corruption"}, {admin, "/tracker/corruption?period=2024-W23"},
		{admin, "/tracker/corruption/case/1"}, {admin, "/tracker/corruption/case/2"},
		{admin, "/tracker/corruption/explorer"},
		{admin, "/admin/fraud"}, {admin, "/admin/fraud/claims"}, {admin, "/admin/fraud/claims/1"},
		{admin, "/admin/fraud/cases"}, {admin, "/admin/fraud/cases/1"}, {admin, "/admin/fraud/cases/1/edit"},
		{admin, "/admin/fraud/cases/1/history"}, {admin, "/admin/fraud/duplicates"},
		{admin, "/admin/fraud/appeals"}, {admin, "/admin/fraud/submit"}, {admin, "/admin/users"},
	}
	wantSource := `<a href="` + testSource + `/tree/` + testCommit + `" rel="noopener">Source code (AGPL-3.0)</a>`
	for _, p := range pages {
		resp, body := p.c.get(p.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", p.path, resp.StatusCode)
			continue
		}
		for _, want := range []string{
			`<link rel="stylesheet" href="/static/standalone/base.css?v=`,
			wantSource, `<a href="/about">About</a>`, "— Example Watch</title>",
			`<style>:root { --accent: #0a7f5a; --accent-2: #0a7f5a; }</style>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", p.path, want)
			}
		}
		// Tracker pages also link the tracker's own stylesheet (auth and
		// about pages are shell-only).
		shellOnly := p.path == "/login" || p.path == "/about" || p.path == "/admin/users"
		if !shellOnly && !strings.Contains(body, "/static/corruption-tracker/tracker.css?v=") {
			t.Errorf("%s: tracker.css not linked", p.path)
		}
		if m := alitheiaBrand.FindString(body); m != "" {
			t.Errorf("%s: alitheia branding %q in standalone output", p.path, m)
		}
	}
	// The stylesheets the pages link are actually served.
	for _, path := range []string{"/static/standalone/base.css", "/static/corruption-tracker/tracker.css"} {
		if resp, _ := anon.get(path); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

// /about (the AGPL network-use source offer) is reachable anonymously —
// even before setup, when everything else redirects to the wizard.
func TestAboutIsAnonymousEvenBeforeSetup(t *testing.T) {
	in := newInstance(t, layout.Branding{})
	c := in.client(t)
	if resp, _ := c.get("/tracker/corruption"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("fresh instance should send pages to /setup, got %d", resp.StatusCode)
	}
	resp, body := c.get("/about")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/about: %d", resp.StatusCode)
	}
	for _, want := range []string{"GNU Affero General Public License, version 3", "AGPL-3.0", "v9.9.9", testCommit,
		testSource + "/tree/" + testCommit, layout.DefaultName} {
		if !strings.Contains(body, strings.ReplaceAll(want, "&", "&amp;")) {
			t.Errorf("/about missing %q", want)
		}
	}
}

func TestNavFollowsEntitlements(t *testing.T) {
	in := newInstance(t, layout.Branding{})
	in.seed(t)
	viewer, admin := in.client(t), in.client(t)
	viewer.login("viewer")
	admin.login("admin")
	_, vb := viewer.get("/tracker/corruption")
	_, ab := admin.get("/tracker/corruption")
	for _, link := range []string{`href="/admin/fraud"`, `href="/admin/users"`, `href="/admin/ingestion"`, `href="/admin/handoff"`, `href="/tracker/corruption/explorer"`} {
		if strings.Contains(vb, link) {
			t.Errorf("viewer nav shows %s", link)
		}
		if !strings.Contains(ab, link) {
			t.Errorf("admin nav lacks %s", link)
		}
	}
	if !strings.Contains(vb, "Sign out") || !strings.Contains(vb, ">viewer<") {
		t.Error("signed-in header missing")
	}
}

// A flash set before a redirect shows once on the next page.
func TestFlashShowsOnce(t *testing.T) {
	in := newInstance(t, layout.Branding{})
	in.seed(t)
	admin := in.client(t)
	admin.login("admin")
	resp, _ := admin.post("/admin/users", url.Values{"username": {"newbie"}, "password": {password},
		"role": {"viewer"}, "note": {"agreement #9"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create user: %d", resp.StatusCode)
	}
	if _, body := admin.get("/admin/users"); !strings.Contains(body, `sa-flash-success`) || !strings.Contains(body, "Created newbie.") {
		t.Error("flash not shown after redirect")
	}
	if _, body := admin.get("/admin/users"); strings.Contains(body, "Created newbie.") {
		t.Error("flash shown twice")
	}
}

func TestBrandingIsSanitised(t *testing.T) {
	in := newInstance(t, layout.Branding{Name: `<script>x</script>`, Accent: "red;}body{display:none", LogoURL: "javascript:alert(1)"})
	c := in.client(t)
	_, body := c.get("/about")
	if strings.Contains(body, "<script>x") || strings.Contains(body, "display:none") || strings.Contains(body, "javascript:") {
		t.Errorf("unsanitised branding in output")
	}
}

// The server must refuse to start without somewhere to send the source
// offer; the layout refuses to build without one too.
func TestBlankSourceURLRefused(t *testing.T) {
	if _, err := layout.New(layout.Options{SourceURL: " "}); !errors.Is(err, version.ErrNoSourceURL) {
		t.Fatalf("layout.New with blank source: %v", err)
	}
	if _, err := version.ResolveSourceURL(func(string) (string, bool) { return "", true }, ""); !errors.Is(err, version.ErrNoSourceURL) {
		t.Fatalf("blank TRACKER_SOURCE_URL: %v", err)
	}
}

// liveBranding is a BrandingSource whose value changes at runtime, as the
// settings service's does.
type liveBranding struct{ b *layout.Branding }

func (l liveBranding) Branding(context.Context) layout.Branding { return *l.b }

// A renamed instance shows the new name, logo and accent on the next page,
// and the reader notice names the new operator, with no restart.
func TestBrandingAppliesWithoutRestart(t *testing.T) {
	b := layout.Branding{Name: "Before"}
	in := newInstanceWith(t, liveBranding{&b})
	in.seed(t)
	viewer := in.client(t)
	viewer.login("viewer")
	b = layout.Branding{Name: "Harbour Integrity Watch", LogoURL: "/static/logo.png", Accent: "#1a6b4c"}
	_, body := viewer.get("/tracker/corruption/case/1")
	for _, want := range []string{"— Harbour Integrity Watch</title>", `<img src="/static/logo.png"`, "--accent: #1a6b4c",
		"Harbour Integrity Watch screens for suppression"} {
		if !strings.Contains(body, want) {
			t.Errorf("after rename, page lacks %q", want)
		}
	}
}

// The vendored chart bundle is served from the module, byte-identical to
// upstream echarts@5.5.0, alongside its licence files.
func TestChartBundleServed(t *testing.T) {
	srv := httptest.NewServer(layout.StaticHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + layout.ChartScript)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("%s: %d %s", layout.ChartScript, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != "42f8329d989b6f6539dd2b15bbdf0d82025762ac112fbb60dc57b27d7bcf3946" {
		t.Errorf("echarts.min.js differs from upstream 5.5.0")
	}
	for _, f := range []string{"LICENSE", "NOTICE", "LICENSE-d3"} {
		if resp, err := http.Get(srv.URL + layout.StaticPrefix + "vendor/echarts-5.5.0/" + f); err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("licence file %s not shipped", f)
		}
	}
}
