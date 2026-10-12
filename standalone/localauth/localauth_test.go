// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package localauth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/store"
)

// standaloneRoles map onto the alitheia roles the .27 goldens recorded:
// anon → anon, viewer → team_view, viewer_export → team_export,
// reviewer → fraud_admin, admin → admin (design §2 D3).
var standaloneRoles = []string{"anon", "viewer", "viewer_export", "reviewer", "admin"}

// roleMatrix is alitheia's GET matrix (cmd/api/testdata/fraud_snapshots/
// matrix.txt) for those roles: the status each role gets on each route. A
// 303 is always to /login?next=<path>.
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
	// Standalone-only: user management is admin-only, as alitheia's
	// /admin/users is (fraud_admin gets a 403 there).
	{"/admin/users", [5]int{303, 403, 403, 403, 200}},
}

// postRoutes is every back-office POST (alitheia's post_matrix.txt): the
// gate must refuse each before any handler runs.
var postRoutes = []string{
	"/admin/fraud/claims/batch", "/admin/fraud/claims/1/transition", "/admin/fraud/claims/2/release-vote",
	"/admin/fraud/claims/1/match/1", "/admin/fraud/claims/1/no-match", "/admin/fraud/claims/1/promote",
	"/admin/fraud/duplicates/merge", "/admin/fraud/cases/1/edit", "/admin/fraud/cases/1/participants/1",
	"/admin/fraud/cases/1/status", "/admin/fraud/cases/1/status/revert", "/admin/fraud/cases/2/verify",
	"/admin/fraud/cases/2/naming", "/admin/fraud/appeals/checked", "/admin/fraud/submit",
	"/admin/users", "/admin/users/1/role", "/admin/users/1/disable",
}

func TestRoleMatrixMatchesAlitheia(t *testing.T) {
	h := newTestHost(t, nil)
	h.seedUsers(t)
	h.seedTracker(t)
	browsers := map[string]*browser{}
	for _, role := range standaloneRoles {
		b := h.browser(t)
		if role != "anon" {
			b.login(role, testPassword)
		}
		browsers[role] = b
	}
	for _, rt := range roleMatrix {
		for i, role := range standaloneRoles {
			resp, body := browsers[role].get(rt.path)
			if resp.StatusCode != rt.codes[i] {
				t.Errorf("%s as %s: %d, want %d (%.120s)", rt.path, role, resp.StatusCode, rt.codes[i], body)
				continue
			}
			if rt.codes[i] == http.StatusSeeOther {
				if loc, want := resp.Header.Get("Location"), "/login?next="+rt.path; loc != want {
					t.Errorf("%s as %s: Location %q, want %q", rt.path, role, loc, want)
				}
			}
		}
	}
	for _, path := range postRoutes {
		for _, role := range []string{"anon", "viewer", "viewer_export"} {
			resp, _ := browsers[role].post(path, nil)
			want := http.StatusForbidden
			if role == "anon" {
				want = http.StatusSeeOther
			}
			if resp.StatusCode != want {
				t.Errorf("POST %s as %s: %d, want %d", path, role, resp.StatusCode, want)
			}
		}
	}
}

// The wizard owns a fresh instance until the first admin exists, signs that
// admin in, and can never run again.
func TestSetupWizardLockOut(t *testing.T) {
	h := newTestHost(t, nil)
	b := h.browser(t)
	for _, path := range []string{"/tracker/corruption", "/admin/fraud", "/login"} {
		if resp, _ := b.get(path); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
			t.Errorf("fresh %s: %d → %q, want 303 → /setup", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, body := b.get("/setup"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "create the administrator") {
		t.Fatalf("setup form: %d", resp.StatusCode)
	}
	if resp, _ := b.post("/setup", url.Values{"username": {"root"}, "password": {"short"}, "password_confirm": {"short"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("weak password: %d, want 422", resp.StatusCode)
	}
	resp, _ := b.post("/setup", url.Values{"username": {"root"}, "password": {testPassword}, "password_confirm": {testPassword}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/users" {
		t.Fatalf("setup: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Signed in as the new admin.
	if resp, _ := b.get("/admin/users"); resp.StatusCode != http.StatusOK {
		t.Errorf("users page after setup: %d", resp.StatusCode)
	}
	// Locked out for good, signed in or not.
	other := h.browser(t)
	for _, c := range []*browser{b, other} {
		if resp, _ := c.get("/setup"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /setup after setup: %d, want 404", resp.StatusCode)
		}
		if resp, _ := c.post("/setup", url.Values{"username": {"evil"}, "password": {testPassword}, "password_confirm": {testPassword}}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST /setup after setup: %d, want 404", resp.StatusCode)
		}
	}
	if _, err := h.svc.UserByName(context.Background(), "evil"); !errors.Is(err, ErrNotFound) {
		t.Error("a second setup must not create a user")
	}
	if resp, _ := other.get("/login"); resp.StatusCode != http.StatusOK {
		t.Errorf("login after setup: %d, want 200", resp.StatusCode)
	}
}

// Concurrent wizard submissions create exactly one admin.
func TestSetupIsSingleShotUnderConcurrency(t *testing.T) {
	h := newTestHost(t, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.svc.Setup(ctx, "admin"+string(rune('a'+i)), testPassword)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrSetupDone):
			t.Errorf("setup: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d setups succeeded, want exactly 1", ok)
	}
}

func TestLoginRateLimit(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := newTestHost(t, func(o *Options) { o.LoginBurst, o.LoginWindow, o.Now = 3, time.Minute, clock })
	h.seedUsers(t)
	b := h.browser(t)
	wrong := url.Values{"username": {"viewer"}, "password": {"wrong password!!"}}
	for i := 0; i < 3; i++ {
		if resp, _ := b.post("/login", wrong); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i+1, resp.StatusCode)
		}
	}
	// Budget spent: even the right password is refused until the window passes.
	right := url.Values{"username": {"viewer"}, "password": {testPassword}}
	if resp, body := b.post("/login", right); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many") {
		t.Fatalf("over budget: %d, want 429", resp.StatusCode)
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	if resp, _ := b.post("/login", right); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after window: %d, want 303", resp.StatusCode)
	}
}

func TestCSRFRequired(t *testing.T) {
	h := newTestHost(t, nil)
	h.seedUsers(t)
	b := h.browser(t)
	b.get("/login")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/login",
		strings.NewReader(url.Values{"username": {"admin"}, "password": {testPassword}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := b.do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: %d, want 403", resp.StatusCode)
	}
}

// Sessions are HttpOnly, SameSite=Lax, and Secure when served over HTTPS.
func TestSessionCookieSecureOverHTTPS(t *testing.T) {
	h := newTestHost(t, func(o *Options) { o.Cookies, o.TrustProxy = CookieSecureAuto, true })
	h.seedUsers(t)
	for _, proto := range []string{"http", "https"} {
		b := h.browser(t)
		b.get("/login")
		form := url.Values{"username": {"admin"}, "password": {testPassword}, "csrf_token": {b.csrf()}}
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", proto)
		resp, _ := b.do(req)
		var sess *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookie {
				sess = c
			}
		}
		if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode {
			t.Fatalf("%s: session cookie %+v", proto, sess)
		}
		if sess.Secure != (proto == "https") {
			t.Errorf("%s: Secure=%v", proto, sess.Secure)
		}
	}
}

// Quarantine release still needs two distinct reviewers when both are
// local accounts: each user is its own tracker actor.
func TestQuarantineReleaseNeedsTwoDistinctLocalReviewers(t *testing.T) {
	h := newTestHost(t, nil)
	h.seedUsers(t)
	h.seedTracker(t)
	ctx := context.Background()
	r1, r2 := h.browser(t), h.browser(t)
	r1.login("reviewer", testPassword)
	r2.login("reviewer2", testPassword)
	reviewer, _ := h.svc.UserByName(ctx, "reviewer")
	if _, err := h.st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: 1, To: domain.ReviewQuarantined, ReviewerID: &reviewer.ActorID, ReviewerLabel: "reviewer",
		Note: "suppression indicated",
	}); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	state := func() domain.ReviewState {
		c, _, _, err := h.st.GetFraudClaim(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return c.ReviewState
	}
	release := url.Values{"to": {"in_review"}, "note": {"release"}, "attestation": {"1"}}

	r1.post("/admin/fraud/claims/1/release-vote", url.Values{"note": {"looks clear"}})
	if resp, _ := r1.post("/admin/fraud/claims/1/transition", release); !strings.HasPrefix(resp.Header.Get("X-Flash"), "error") {
		t.Errorf("same reviewer release flash = %q, want an error", resp.Header.Get("X-Flash"))
	}
	if got := state(); got != domain.ReviewQuarantined {
		t.Fatalf("after self-release attempt: %s, want quarantined", got)
	}
	if resp, _ := r2.post("/admin/fraud/claims/1/transition", release); strings.HasPrefix(resp.Header.Get("X-Flash"), "error") {
		t.Fatalf("second reviewer release refused: %s", resp.Header.Get("X-Flash"))
	}
	if got := state(); got != domain.ReviewInReview {
		t.Fatalf("after second reviewer: %s, want in_review", got)
	}
}

func TestUserAdministrationGuards(t *testing.T) {
	h := newTestHost(t, nil)
	users := h.seedUsers(t)
	ctx := context.Background()
	admin := users["admin"]

	// The last active admin can be neither demoted nor disabled…
	if err := h.svc.SetRole(ctx, admin, admin.ID, RoleReviewer, "self-demote"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("self-demote last admin: %v, want ErrLastAdmin", err)
	}
	if err := h.svc.SetDisabled(ctx, admin, admin.ID, true, ""); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disable last admin: %v, want ErrLastAdmin", err)
	}
	// …until another admin exists.
	if err := h.svc.SetRole(ctx, admin, users["reviewer"].ID, RoleAdmin, "promotion"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SetRole(ctx, admin, admin.ID, RoleReviewer, "stepping down"); err != nil {
		t.Errorf("demote with another admin: %v", err)
	}

	// Every grant needs an access-basis note.
	if _, err := h.svc.CreateUser(ctx, admin, NewUser{Username: "x", Password: testPassword, Role: RoleViewer}); !errors.Is(err, ErrNoteRequired) {
		t.Errorf("create without note: %v", err)
	}
	if err := h.svc.SetExport(ctx, admin, users["viewer"].ID, true, " "); !errors.Is(err, ErrNoteRequired) {
		t.Errorf("export grant without note: %v", err)
	}
	if err := h.svc.SetRole(ctx, admin, users["viewer"].ID, RoleReviewer, ""); !errors.Is(err, ErrNoteRequired) {
		t.Errorf("role change without note: %v", err)
	}
	if _, err := h.svc.CreateUser(ctx, admin, NewUser{Username: "VIEWER", Password: testPassword, Role: RoleViewer, Note: "n"}); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("case-insensitive duplicate username: %v", err)
	}

	// Disabling signs the user out and blocks sign-in.
	vb := h.browser(t)
	vb.login("viewer", testPassword)
	if err := h.svc.SetDisabled(ctx, admin, users["viewer"].ID, true, "left the org"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := vb.get("/tracker/corruption"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("disabled user's session still valid: %d", resp.StatusCode)
	}
	if _, err := h.svc.Authenticate(ctx, "viewer", testPassword); !errors.Is(err, ErrInvalidLogin) {
		t.Errorf("disabled user authenticated: %v", err)
	}

	// Grants and changes land in the access log, notes included.
	var n int
	if err := h.svc.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_access_log
		WHERE surface IN ('user_create','user_role','user_disable') AND detail <> ''`).Scan(&n); err != nil || n < 6 {
		t.Errorf("access-admin log rows = %d (err %v), want >= 6", n, err)
	}
}

func TestReviewerWarning(t *testing.T) {
	h := newTestHost(t, nil)
	ctx := context.Background()
	admin, err := h.svc.Setup(ctx, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := h.svc.ReviewerWarning(ctx); !strings.Contains(w, "Only 1 active user can review") {
		t.Errorf("warning with one admin = %q", w)
	}
	b := h.browser(t)
	b.login("admin", testPassword)
	if _, body := b.get("/admin/users"); !strings.Contains(body, "two different reviewers") {
		t.Error("Users page must show the reviewer warning")
	}
	if _, err := h.svc.CreateUser(ctx, admin, NewUser{Username: "rev", Password: testPassword, Role: RoleReviewer, Note: "staff"}); err != nil {
		t.Fatal(err)
	}
	if w, _ := h.svc.ReviewerWarning(ctx); w != "" {
		t.Errorf("warning with two reviewers = %q", w)
	}
}

// Imported (legacy) actors have no login until an admin claims one; the
// claim keeps the actor id, so their audit history stays theirs.
func TestClaimLegacyActor(t *testing.T) {
	h := newTestHost(t, nil)
	users := h.seedUsers(t)
	ctx := context.Background()
	legacy, err := h.st.EnsureActor(ctx, store.Actor{ExternalRef: "alitheia:user:42", Label: "old@reviewer", Kind: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.EnsureActor(ctx, store.Actor{ExternalRef: "system:recode", Label: "recode", Kind: "system"}); err != nil {
		t.Fatal(err)
	}
	claimable, err := h.svc.ClaimableActors(ctx)
	if err != nil || len(claimable) != 1 || claimable[0].ID != legacy {
		t.Fatalf("claimable = %+v (err %v), want only the legacy person", claimable, err)
	}
	// Unclaimed, nobody can sign in as them (there is no account at all).
	if _, err := h.svc.Authenticate(ctx, "old@reviewer", testPassword); !errors.Is(err, ErrInvalidLogin) {
		t.Fatalf("legacy actor authenticated before claim: %v", err)
	}
	u, err := h.svc.ClaimActor(ctx, users["admin"], legacy, NewUser{
		Username: "oldrev", Password: testPassword, Role: RoleReviewer, Note: "handed over"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ActorID != legacy || u.ExternalRef != "alitheia:user:42" {
		t.Errorf("claimed user actor = %d %q, want %d alitheia:user:42", u.ActorID, u.ExternalRef, legacy)
	}
	if _, err := h.svc.ClaimActor(ctx, users["admin"], legacy, NewUser{
		Username: "again", Password: testPassword, Role: RoleViewer, Note: "x"}); !errors.Is(err, ErrNotClaimable) {
		t.Errorf("second claim: %v, want ErrNotClaimable", err)
	}
	// The claimed user's actions are attributed to the legacy actor.
	b := h.browser(t)
	b.login("oldrev", testPassword)
	h.seedTracker(t)
	b.post("/admin/fraud/claims/2/release-vote", url.Values{"note": {"n"}})
	var reviewer int64
	if err := h.st.Pool.QueryRow(ctx, `SELECT reviewer FROM fraud_claim_reviews
		WHERE action = 'quarantine_release_vote' ORDER BY id DESC LIMIT 1`).Scan(&reviewer); err != nil || reviewer != legacy {
		t.Errorf("vote attributed to actor %d (err %v), want %d", reviewer, err, legacy)
	}
}

func TestCLIResetPassword(t *testing.T) {
	h := newTestHost(t, nil)
	h.seedUsers(t)
	ctx := context.Background()
	b := h.browser(t)
	b.login("viewer", testPassword)

	var out bytes.Buffer
	if err := RunCLI(ctx, h.svc, []string{"reset-password", "viewer"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`New password for viewer: (\S+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no generated password in %q", out.String())
	}
	if _, err := h.svc.Authenticate(ctx, "viewer", m[1]); err != nil {
		t.Errorf("generated password doesn't work: %v", err)
	}
	if _, err := h.svc.Authenticate(ctx, "viewer", testPassword); !errors.Is(err, ErrInvalidLogin) {
		t.Error("old password still works")
	}
	if resp, _ := b.get("/tracker/corruption"); resp.StatusCode != http.StatusSeeOther {
		t.Error("reset must end the user's sessions")
	}

	out.Reset()
	if err := RunCLI(ctx, h.svc, []string{"reset-password", "--password-stdin", "viewer"},
		strings.NewReader("a chosen passphrase\n"), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Authenticate(ctx, "viewer", "a chosen passphrase"); err != nil {
		t.Errorf("stdin password doesn't work: %v", err)
	}
	if err := RunCLI(ctx, h.svc, []string{"reset-password", "nobody"}, nil, &out); err == nil {
		t.Error("unknown user must fail")
	}
	out.Reset()
	if err := RunCLI(ctx, h.svc, []string{"list"}, nil, &out); err != nil || !strings.Contains(out.String(), "viewer_export") {
		t.Errorf("list: %v %q", err, out.String())
	}
}

// Person-level reads by local users are audited to tracker_access_log with
// the surface strings an embedding host receives.
func TestReaderAccessIsAudited(t *testing.T) {
	h := newTestHost(t, nil)
	h.seedUsers(t)
	h.seedTracker(t)
	b := h.browser(t)
	b.login("viewer_export", testPassword)
	b.get("/tracker/corruption/case/1")
	b.get("/tracker/corruption/explorer")
	ctx := context.Background()
	for _, surface := range []string{"case_view", "explorer_view"} {
		var label string
		err := h.svc.Pool.QueryRow(ctx, `SELECT l.actor_label FROM tracker_access_log l
			JOIN tracker_users u ON u.actor_id = l.actor_id
			WHERE l.surface = $1 AND u.username = 'viewer_export'`, surface).Scan(&label)
		if err != nil || label != "viewer_export" {
			t.Errorf("%s audit row: label %q err %v", surface, label, err)
		}
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("some long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Errorf("hash format %q", h)
	}
	if ok, err := VerifyPassword(h, "some long passphrase"); !ok || err != nil {
		t.Errorf("verify right: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "some long passphrasf"); ok {
		t.Error("verify wrong password passed")
	}
	if _, err := VerifyPassword("$2a$10$bcrypt", "x"); err == nil {
		t.Error("foreign hash must be rejected")
	}
	if safeNext("//evil.example") != "/" || safeNext("https://evil.example") != "/" || safeNext("/admin/fraud") != "/admin/fraud" {
		t.Error("safeNext must keep redirects on-site")
	}
}

// Sessions expire after SessionTTL and expired rows are swept when new
// sessions are issued.
func TestSessionExpiryAndSweep(t *testing.T) {
	// Near real time: the client cookie jar drops cookies whose Expires
	// has passed by the wall clock.
	now := time.Now().UTC()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := newTestHost(t, func(o *Options) { o.SessionTTL, o.Now = time.Hour, clock })
	h.seedUsers(t)
	old := h.browser(t)
	old.login("viewer", testPassword)
	if resp, _ := old.get("/tracker/corruption"); resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh session: %d", resp.StatusCode)
	}
	// Past the TTL server-side (the jar still holds the cookie: the
	// server, not the browser, must enforce expiry).
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	if resp, _ := old.get("/tracker/corruption"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expired session still valid: %d", resp.StatusCode)
	}
	h.browser(t).login("admin", testPassword)
	var n int
	if err := h.svc.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tracker_sessions WHERE expires_at <= $1`, clock()).Scan(&n); err != nil || n != 0 {
		t.Errorf("expired sessions left after sweep: %d (err %v)", n, err)
	}
}

// A multipart upload is checked against a header or URL-query token without
// the middleware parsing (and spooling) the body: the handler still gets
// the whole stream to bound itself. A token only inside the multipart body
// is not accepted.
func TestCSRFDoesNotConsumeMultipartBodies(t *testing.T) {
	a, err := New(Options{Service: &Service{}, Layout: testLayout{}})
	if err != nil {
		t.Fatal(err)
	}
	var gotBytes int
	var parsed bool
	h := a.CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parsed = r.MultipartForm != nil
		b, _ := io.ReadAll(r.Body)
		gotBytes = len(b)
	}))
	const token = "0123456789abcdef0123456789abcdef0123456789ab" // gitleaks:allow (synthetic CSRF token)
	payload := "--X\r\nContent-Disposition: form-data; name=\"csrf_token\"\r\n\r\n" + token +
		"\r\n--X\r\nContent-Disposition: form-data; name=\"bundle\"; filename=\"b.tar.gz\"\r\n\r\n" +
		strings.Repeat("z", 1<<20) + "\r\n--X--\r\n"
	send := func(target string) int {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(payload))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=X")
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("/setup/import"); code != http.StatusForbidden {
		t.Errorf("token only in the multipart body: %d, want 403", code)
	}
	gotBytes, parsed = 0, false
	if code := send("/setup/import?csrf_token=" + token); code != http.StatusOK {
		t.Fatalf("query token: %d", code)
	}
	if parsed || gotBytes != len(payload) {
		t.Errorf("middleware consumed the upload: parsed=%v handler read %d of %d bytes", parsed, gotBytes, len(payload))
	}
}

// The wizard offers "Start from an export" (posting to the host's
// /setup/import with a query CSRF token) until data has been imported.
func TestSetupOffersImport(t *testing.T) {
	h := newTestHost(t, nil)
	b := h.browser(t)
	_, body := b.get("/setup")
	if !strings.Contains(body, `enctype="multipart/form-data"`) || !strings.Contains(body, `action="/setup/import?csrf_token=`+b.csrf()+`"`) ||
		!strings.Contains(body, `name="bundle"`) {
		t.Errorf("setup page lacks the import form:\n%s", body)
	}
	_, body = b.get("/setup?imported=exp-123")
	if !strings.Contains(body, "Data imported from export exp-123") || strings.Contains(body, `name="bundle"`) {
		t.Errorf("after import the page should confirm and hide the upload form")
	}
}

// The query-string token is a multipart-only concession: an urlencoded POST
// must carry its token in the body. Every response forbids cross-site
// Referer, and recorded URLs (the sign-in "next") drop the token.
func TestQueryTokenHardening(t *testing.T) {
	h := newTestHost(t, nil)
	h.seedUsers(t)
	b := h.browser(t)
	resp, _ := b.get("/login")
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
	token := b.csrf()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/login?csrf_token="+token,
		strings.NewReader(url.Values{"username": {"admin"}, "password": {testPassword}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := b.do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("urlencoded POST with a query-only token = %d, want 403", resp.StatusCode)
	}

	// An anonymous multipart POST to a protected page is sent to sign in
	// without the token riding along in "next".
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+"/admin/users?keep=1&csrf_token="+token,
		strings.NewReader("--X--\r\n"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=X")
	resp, _ = b.do(req)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || strings.Contains(loc, token) ||
		loc != "/login?next=/admin/users?keep=1" {
		t.Errorf("redirect = %d %q, want 303 to /login?next=/admin/users?keep=1 without the token", resp.StatusCode, loc)
	}

	u, _ := url.Parse("/setup/import?csrf_token=secret&x=1")
	if got := RedactURL(u); got != "/setup/import?x=1" {
		t.Errorf("RedactURL = %q", got)
	}
}
