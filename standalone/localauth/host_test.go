// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package localauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/readmodel"
	"github.com/lonelycode/corruption-tracker/standalone/accesslog"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
	"github.com/lonelycode/corruption-tracker/web"
	"github.com/lonelycode/corruption-tracker/web/reader"
)

// testLayout is a chrome-less host.Layout: it writes the page body and
// surfaces flashes as an X-Flash response header tests can read.
type testLayout struct{}

func (testLayout) Render(w http.ResponseWriter, _ *http.Request, pg host.Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if pg.Status != 0 {
		w.WriteHeader(pg.Status)
	}
	_, _ = io.WriteString(w, "<title>"+pg.Title+"</title>"+string(pg.Body))
}
func (testLayout) CSRFToken(r *http.Request) string { return CSRFToken(r) }
func (testLayout) SetFlash(w http.ResponseWriter, level, msg string) {
	w.Header().Set("X-Flash", level+": "+msg)
}

// testHost is a standalone server in miniature: local auth in front of the
// tracker's back office and reader, exactly as the standalone binary wires
// them (less the layout, which is .13's).
type testHost struct {
	srv  *httptest.Server
	st   *store.Store
	svc  *Service
	auth *Auth
}

func newTestHost(t *testing.T, mod func(*Options)) *testHost {
	t.Helper()
	st := storetest.Open(t)
	svc := NewService(st.Pool)
	o := Options{Service: svc, Layout: testLayout{}, Cookies: CookieSecureNever, LoginBurst: 1000,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mod != nil {
		mod(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	authz := a.Authorizer()
	audit := accesslog.Auditor{Pool: st.Pool}
	wh, err := web.New(web.Deps{Store: st, Auth: authz, Audit: audit, Layout: testLayout{}, Log: o.Log})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := reader.New(reader.Options{Auth: authz, Layout: testLayout{}, Log: o.Log})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(a.CSRF, a.Session, a.SetupGate)
	a.Routes(r)
	rd.Mount(r, readmodel.New(readmodel.Options{Store: st, Auth: authz, Audit: audit, Log: o.Log,
		Notice: readmodel.NoticeFor("Test operator")}))
	r.Mount("/admin/fraud", wh.Admin)
	r.Handle(web.StaticPrefix+"*", wh.Static)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &testHost{srv: srv, st: st, svc: svc, auth: a}
}

// browser is a cookie-keeping client that doesn't follow redirects.
type browser struct {
	t    *testing.T
	h    *testHost
	c    *http.Client
	base *url.URL
}

func (h *testHost) browser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(h.srv.URL)
	return &browser{t: t, h: h, base: base, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func (b *browser) get(path string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodGet, b.h.srv.URL+path, nil)
	return b.do(req)
}

// post submits a form with the CSRF token from the browser's cookie
// (fetching a page first if it has none yet).
func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	token := b.csrf()
	if token == "" {
		b.get("/about")
		token = b.csrf()
	}
	form.Set("csrf_token", token)
	req, _ := http.NewRequest(http.MethodPost, b.h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

func (b *browser) csrf() string {
	for _, c := range b.c.Jar.Cookies(b.base) {
		if c.Name == csrfCookie {
			return c.Value
		}
	}
	return ""
}

func (b *browser) login(username, password string) {
	b.t.Helper()
	resp, body := b.post("/login", url.Values{"username": {username}, "password": {password}, "next": {"/"}})
	if resp.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("login %s: %d %s", username, resp.StatusCode, body)
	}
}

const testPassword = "correct horse battery"

// seedUsers creates the first admin plus one user of each other role.
func (h *testHost) seedUsers(t *testing.T) map[string]User {
	t.Helper()
	ctx := context.Background()
	admin, err := h.svc.Setup(ctx, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]User{"admin": admin}
	for name, n := range map[string]NewUser{
		"viewer":        {Role: RoleViewer, Note: "agreement #1"},
		"viewer_export": {Role: RoleViewer, CanExport: true, Note: "agreement #2"},
		"reviewer":      {Role: RoleReviewer, Note: "staff"},
		"reviewer2":     {Role: RoleReviewer, Note: "staff"},
	} {
		n.Username, n.Password = name, testPassword
		u, err := h.svc.CreateUser(ctx, admin, n)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		out[name] = u
	}
	return out
}

// seedTracker builds the fixture the alitheia role matrix (.27 goldens) was
// recorded against: case 1 verified, case 2 unverified, case 3, claims 1–2,
// and a verified + dev report for 2024-W23.
func (h *testHost) seedTracker(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	st := h.st
	seedActor, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "system:seed", Label: "seed", Kind: "system"})
	if err != nil {
		t.Fatal(err)
	}
	day := func(s string) *time.Time { v, _ := time.Parse("2006-01-02", s); return &v }
	mk := func(name, person, hash string, ev domain.FraudEventType, when string, src domain.FraudSourceType) int64 {
		c, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case: domain.FraudCase{CaseName: name, TypologyRaw: "tax fraud"},
			Participants: []store.FraudParticipantInput{{
				PersonName: person, Participant: domain.FraudCaseParticipant{Role: "defendant"}}},
			Events:  []domain.FraudCaseEvent{{EventType: ev, EventDate: day(when)}},
			Sources: []domain.FraudCaseSource{{SourceType: src, ContentHash: hash, URL: "https://example.test/" + hash}},
		})
		if err != nil {
			t.Fatalf("seed case %s: %v", name, err)
		}
		return c.ID
	}
	c1 := mk("Verified", "Vera Verified", "s1", domain.EventSentencing, "2024-06-04", domain.FraudSourceAgencyRelease)
	mk("Alleged", "Alan Alleged", "s2", domain.EventChargesLaid, "2024-06-05", domain.FraudSourceNews)
	mk("Appeal", "Petra Pending", "s3", domain.EventAppealFiled, "2023-06-01", domain.FraudSourceJudgment)
	if _, err := st.VerifyFraudCase(ctx, c1, seedActor); err != nil {
		t.Fatal(err)
	}
	for i, hl := range []string{"Director sentenced", "Paywalled report"} {
		conf := 0.9
		claim := domain.FraudClaim{Headline: hl, SourceType: domain.FraudSourceNews,
			ContentHash: "claim-" + hl, ConfidenceScore: &conf}
		if i == 0 {
			claim.Payload = []byte(`{"is_fraud_case":true,"case_stage":"sentenced"}`)
		} else {
			claim.NeedsManualExtraction = true
		}
		if _, _, err := st.CreateFraudClaim(ctx, claim, nil, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	for _, dev := range []bool{false, true} {
		b := &fraudreport.Builder{DB: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), IncludeUnverified: dev}
		if _, _, err := b.Build(ctx, "2024-W23", "seed"); err != nil {
			t.Fatal(err)
		}
	}
}
