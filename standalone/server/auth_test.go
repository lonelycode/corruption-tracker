// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// End-to-end checks of standalone auth through the real
// server: the router serve runs, over a real listener.

const userPassword = "correct horse battery"

// serving boots `serve` on an empty database over a real listener, returns
// its base URL and stops it when the test ends.
func serving(t *testing.T, o server.Options) (*server.Server, string) {
	t.Helper()
	o.DatabaseURL = emptyDatabase(t)
	if o.Log == nil {
		o.Log = quiet()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o.Listener = ln
	ctx, cancel := context.WithCancel(context.Background())
	s, err := server.Boot(ctx, o)
	if err != nil {
		cancel()
		t.Fatalf("boot: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("serve did not shut down")
		}
		s.Pool.Close()
	})
	return s, "http://" + ln.Addr().String()
}

func (b *browser) login(t *testing.T, user string) {
	t.Helper()
	if resp, body := b.post("/login", "/login", url.Values{"username": {user}, "password": {userPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s = %d\n%s", user, resp.StatusCode, body)
	}
}

// seedAccounts creates, through the setup wizard's service, the first admin
// and one account per remaining role.
func seedAccounts(t *testing.T, svc *localauth.Service) {
	t.Helper()
	ctx := context.Background()
	admin, err := svc.Setup(ctx, "admin", userPassword)
	if err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]localauth.NewUser{
		"viewer":        {Role: localauth.RoleViewer, Note: "agreement #1"},
		"viewer_export": {Role: localauth.RoleViewer, CanExport: true, Note: "agreement #2"},
		"reviewer":      {Role: localauth.RoleReviewer, Note: "staff"},
		"reviewer2":     {Role: localauth.RoleReviewer, Note: "staff"},
	} {
		n.Username, n.Password = name, userPassword
		if _, err := svc.CreateUser(ctx, admin, n); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
}

// seedCases builds the fixture alitheia's role matrix (the .27 goldens) was
// recorded against: case 1 verified, case 2 unverified, case 3, claims 1–2
// and a verified + dev report for 2024-W23.
func seedCases(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	seeder, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "system:seed", Label: "seed", Kind: "system"})
	if err != nil {
		t.Fatal(err)
	}
	day := func(s string) *time.Time { v, _ := time.Parse("2006-01-02", s); return &v }
	for i, c := range []struct {
		person string
		ev     domain.FraudEventType
		when   string
		src    domain.FraudSourceType
	}{
		{"Vera Verified", domain.EventSentencing, "2024-06-04", domain.FraudSourceAgencyRelease},
		{"Alan Alleged", domain.EventChargesLaid, "2024-06-05", domain.FraudSourceNews},
		{"Petra Pending", domain.EventAppealFiled, "2023-06-01", domain.FraudSourceJudgment},
	} {
		fc, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case: domain.FraudCase{CaseName: c.person, TypologyRaw: "tax fraud"},
			Participants: []store.FraudParticipantInput{{PersonName: c.person,
				Participant: domain.FraudCaseParticipant{Role: "defendant"}}},
			Events:  []domain.FraudCaseEvent{{EventType: c.ev, EventDate: day(c.when)}},
			Sources: []domain.FraudCaseSource{{SourceType: c.src, ContentHash: c.person}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := st.VerifyFraudCase(ctx, fc.ID, seeder); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, headline := range []string{"Director sentenced", "Paywalled report"} {
		claim := domain.FraudClaim{Headline: headline, SourceType: domain.FraudSourceNews, ContentHash: headline}
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

// roles map onto the alitheia roles the .27 goldens recorded (design D3):
// anon → anon, viewer → flagged team, viewer_export → export-flagged team,
// reviewer → fraud_admin, admin → full admin.
var roles = []string{"anon", "viewer", "viewer_export", "reviewer", "admin"}

// roleMatrix is alitheia's GET matrix (cmd/api/testdata/fraud_snapshots/
// matrix.txt) for those roles, plus the standalone-only admin pages. A 303
// is always to /login?next=<path>.
var roleMatrix = []struct {
	path  string
	codes [5]int // anon, viewer, viewer_export, reviewer, admin
}{
	{"/tracker/corruption", [5]int{303, 200, 200, 200, 200}},
	{"/tracker/corruption?period=2024-W23", [5]int{303, 200, 200, 200, 200}},
	{"/tracker/corruption?week=2024-W23", [5]int{303, 200, 200, 200, 200}},
	{"/tracker/corruption?period=nope", [5]int{303, 404, 404, 404, 404}},
	{"/tracker/corruption/export?period=2024-W23&format=csv", [5]int{404, 404, 200, 200, 200}},
	{"/tracker/corruption/export?period=2024-W23&format=md", [5]int{404, 404, 200, 200, 200}},
	{"/tracker/corruption/export", [5]int{404, 404, 400, 400, 400}},
	{"/tracker/corruption/explorer", [5]int{303, 404, 200, 200, 200}},
	{"/tracker/corruption/explorer?status=convicted&q=GST", [5]int{303, 404, 200, 200, 200}},
	{"/tracker/corruption/explorer?format=csv", [5]int{303, 404, 200, 200, 200}},
	{"/tracker/corruption/case/1", [5]int{303, 200, 200, 200, 200}},
	{"/tracker/corruption/case/2", [5]int{303, 404, 404, 200, 200}},
	{"/tracker/corruption/case/999999", [5]int{303, 404, 404, 404, 404}},
	{"/admin/fraud", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/claims", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/claims?state=all", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/claims?state=all&manual=1", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/claims/1", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/claims/2", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/duplicates", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/cases", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/cases?review=unverified", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/cases/1", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/cases/1/edit", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/cases/1/history", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/appeals", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/submit", [5]int{303, 403, 403, 200, 200}},
	{"/admin/fraud/submit?case=3&origin=appeals", [5]int{303, 403, 403, 200, 200}},
	// Standalone-only, admin-only (alitheia's /admin/* outside the fraud
	// back office 403s for fraud_admin).
	{"/admin/users", [5]int{303, 403, 403, 403, 200}},
	{"/admin/ingestion", [5]int{303, 403, 403, 403, 200}},
}

// The acceptance path end to end on `serve`: a fresh instance belongs to
// the setup wizard, which then locks itself out; afterwards every role
// reaches every route exactly as its alitheia equivalent does.
func TestServeSetupAndRoleMatrix(t *testing.T) {
	s, base := serving(t, server.Options{})
	fresh := newBrowser(t, base)
	for _, path := range []string{"/", "/login", "/tracker/corruption", "/admin/fraud", "/admin/users", "/admin/ingestion"} {
		if resp, _ := fresh.get(path); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
			t.Errorf("fresh %s = %d → %q, want 303 → /setup", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, _ := fresh.post("/setup", "/setup", url.Values{"username": {"admin"},
		"password": {userPassword}, "password_confirm": {userPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	for _, c := range []*browser{fresh, newBrowser(t, base)} {
		if resp, _ := c.get("/setup"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /setup after setup = %d, want 404", resp.StatusCode)
		}
	}
	// A replayed wizard submission (token from before setup) creates nobody.
	again := newBrowser(t, base)
	_, loginPage := again.get("/login")
	token := csrfField.FindStringSubmatch(loginPage)[1]
	if resp, err := again.c.PostForm(base+"/setup", url.Values{"csrf_token": {token}, "username": {"intruder"},
		"password": {userPassword}, "password_confirm": {userPassword}}); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /setup after setup = %v %v, want 404", resp.StatusCode, err)
	}
	if _, err := s.Accounts.UserByName(context.Background(), "intruder"); !errors.Is(err, localauth.ErrNotFound) {
		t.Error("a second setup created a user")
	}

	// Seed the other roles and the fixture, then walk the matrix.
	ctx := context.Background()
	admin, _ := s.Accounts.UserByName(ctx, "admin")
	for name, n := range map[string]localauth.NewUser{
		"viewer":        {Role: localauth.RoleViewer, Note: "agreement #1"},
		"viewer_export": {Role: localauth.RoleViewer, CanExport: true, Note: "agreement #2"},
		"reviewer":      {Role: localauth.RoleReviewer, Note: "staff"},
	} {
		n.Username, n.Password = name, userPassword
		if _, err := s.Accounts.CreateUser(ctx, admin, n); err != nil {
			t.Fatal(err)
		}
	}
	seedCases(t, s.Store)
	bs := map[string]*browser{"anon": newBrowser(t, base), "admin": fresh}
	for _, role := range []string{"viewer", "viewer_export", "reviewer"} {
		bs[role] = newBrowser(t, base)
		bs[role].login(t, role)
	}
	for _, rt := range roleMatrix {
		for i, role := range roles {
			resp, body := bs[role].get(rt.path)
			if resp.StatusCode != rt.codes[i] {
				t.Errorf("%s as %s = %d, want %d (%.100s)", rt.path, role, resp.StatusCode, rt.codes[i], body)
				continue
			}
			if rt.codes[i] == http.StatusSeeOther {
				if loc, want := resp.Header.Get("Location"), "/login?next="+rt.path; loc != want {
					t.Errorf("%s as %s → %q, want %q", rt.path, role, loc, want)
				}
			}
		}
	}
}

// Releasing a quarantined claim needs two distinct reviewers: a reviewer's
// own vote doesn't let them release it.
func TestServeQuarantineReleaseNeedsTwoReviewers(t *testing.T) {
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	seedCases(t, s.Store)
	r1, r2 := newBrowser(t, base), newBrowser(t, base)
	r1.login(t, "reviewer")
	r2.login(t, "reviewer2")
	claim := "/admin/fraud/claims/1"
	state := func() domain.ReviewState {
		c, _, _, err := s.Store.GetFraudClaim(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		return c.ReviewState
	}
	if resp, _ := r1.post(claim, claim+"/transition", url.Values{"to": {string(domain.ReviewQuarantined)}, "note": {"suppression signal"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("quarantine = %d", resp.StatusCode)
	}
	if got := state(); got != domain.ReviewQuarantined {
		t.Fatalf("after quarantine: %s", got)
	}
	release := url.Values{"to": {"in_review"}, "note": {"release"}, "attestation": {"1"}}
	r1.post(claim, claim+"/release-vote", url.Values{"note": {"checked"}})
	r1.post(claim, claim+"/transition", release)
	if got := state(); got != domain.ReviewQuarantined {
		t.Fatalf("one reviewer released their own quarantine vote: %s", got)
	}
	r2.post(claim, claim+"/transition", release)
	if got := state(); got != domain.ReviewInReview {
		t.Fatalf("second reviewer's release: %s, want in_review", got)
	}
}

// Sign-in is rate-limited with the server's defaults (5 per 15 minutes).
func TestServeLoginRateLimit(t *testing.T) {
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	b := newBrowser(t, base)
	for i := 1; i <= 5; i++ {
		if resp, _ := b.post("/login", "/login", url.Values{"username": {"viewer"}, "password": {"wrong password"}}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, resp.StatusCode)
		}
	}
	if resp, body := b.post("/login", "/login", url.Values{"username": {"viewer"}, "password": {userPassword}}); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many") {
		t.Fatalf("6th attempt = %d, want 429", resp.StatusCode)
	}
}

// Auth cookies are Secure when the server is reached over HTTPS: directly
// over TLS, or through a trusted proxy's X-Forwarded-Proto. The header is
// ignored unless TRACKER_TRUST_PROXY is set.
func TestServeSecureCookies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trustProxy bool
		tls        bool
		proto      string
		secure     bool
	}{
		{"plain http", false, false, "", false},
		{"https direct", false, true, "", true},
		{"trusted proxy, https", true, false, "https", true},
		{"trusted proxy, http", true, false, "http", false},
		{"untrusted forwarded header", false, false, "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.Open(t)
			s, err := server.Build(context.Background(), st.Pool, server.Options{Log: quiet(), TrustProxy: tc.trustProxy})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Accounts.Setup(context.Background(), "admin", userPassword); err != nil {
				t.Fatal(err)
			}
			var srv *httptest.Server
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			if tc.tls {
				srv = httptest.NewTLSServer(s.Handler)
				client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			} else {
				srv = httptest.NewServer(s.Handler)
			}
			t.Cleanup(srv.Close)
			do := func(method, path string, form url.Values, cookies []*http.Cookie) *http.Response {
				var body io.Reader
				if form != nil {
					body = strings.NewReader(form.Encode())
				}
				req, _ := http.NewRequest(method, srv.URL+path, body)
				if form != nil {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				if tc.proto != "" {
					req.Header.Set("X-Forwarded-Proto", tc.proto)
				}
				for _, c := range cookies {
					req.AddCookie(c)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return resp
			}
			first := do(http.MethodGet, "/login", nil, nil)
			var csrf *http.Cookie
			for _, c := range first.Cookies() {
				if c.Name == "tracker_csrf" {
					csrf = c
				}
			}
			if csrf == nil || csrf.Secure != tc.secure {
				t.Fatalf("csrf cookie %+v, want Secure=%v", csrf, tc.secure)
			}
			resp := do(http.MethodPost, "/login", url.Values{"csrf_token": {csrf.Value}, "username": {"admin"},
				"password": {userPassword}}, []*http.Cookie{{Name: csrf.Name, Value: csrf.Value}})
			var sess *http.Cookie
			for _, c := range resp.Cookies() {
				if c.Name == "tracker_session" {
					sess = c
				}
			}
			if sess == nil {
				t.Fatalf("no session cookie (status %d)", resp.StatusCode)
			}
			if sess.Secure != tc.secure || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie Secure=%v HttpOnly=%v SameSite=%v, want Secure=%v HttpOnly Lax",
					sess.Secure, sess.HttpOnly, sess.SameSite, tc.secure)
			}
		})
	}
}

// The account-recovery path in the real binary: `corruption-tracker user
// reset-password NAME` prints a new password that signs in, and ends the
// user's existing sessions.
func TestBinaryUserResetPassword(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	b := newBrowser(t, base)
	b.login(t, "viewer")

	bin := filepath.Join(t.TempDir(), "corruption-tracker")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../../cmd/corruption-tracker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "user", "reset-password", "viewer")
	cmd.Env = append(os.Environ(), "DATABASE_URL="+s.Pool.Config().ConnString())
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("user reset-password: %v\n%s", err, stderr.String())
	}
	m := regexp.MustCompile(`New password for viewer: (\S+)`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("no password printed: %q", stdout.String())
	}
	if resp, _ := b.get("/tracker/corruption"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("old session survived the reset: %d", resp.StatusCode)
	}
	fresh := newBrowser(t, base)
	if resp, _ := fresh.post("/login", "/login", url.Values{"username": {"viewer"}, "password": {m[1]}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("new password doesn't sign in: %d", resp.StatusCode)
	}
}
