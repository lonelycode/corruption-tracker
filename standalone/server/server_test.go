// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/migrations"
	"github.com/lonelycode/corruption-tracker/scheduler"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// emptyDatabase creates a brand-new database on the DATABASE_URL_TEST
// server (whose own database must carry the test-db marker) and returns its
// DSN; it is dropped when the test ends.
func emptyDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set; skipping standalone server DB test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var comment *string
	if err := admin.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = current_database()`).
		Scan(&comment); err != nil {
		t.Fatalf("DATABASE_URL_TEST unusable: %v", err)
	}
	if comment == nil || !strings.HasSuffix(*comment, "test-db") {
		t.Fatal("DATABASE_URL_TEST's database is not marked as a throwaway test DB (comment must end in test-db)")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "ct_serve_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create scratch database (needs CREATEDB): %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	return u.String()
}

type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// post submits form with the CSRF token scraped from a GET of tokenPage.
func (b *browser) post(tokenPage, path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	_, page := b.get(tokenPage)
	m := csrfField.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatalf("no csrf token on %s:\n%s", tokenPage, page)
	}
	form.Set("csrf_token", m[1])
	resp, err := b.c.PostForm(b.base+path, form)
	if err != nil {
		b.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

const adminPassword = "correct-horse-battery-staple-42" // gitleaks:allow (synthetic test password)

// Against an empty Postgres, serve creates the schema and serves the app
// with no manual step: health, the setup wizard, then the app.
func TestServeOnEmptyDatabase(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.Boot(ctx, server.Options{DatabaseURL: dsn, Listener: ln, Log: quiet(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("boot on an empty database: %v", err)
	}
	defer s.Pool.Close()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	v, ok, err := migrations.Version(dsn)
	if err != nil || !ok || v < 6 {
		t.Fatalf("schema version after boot = %d ok=%v err=%v", v, ok, err)
	}
	b := newBrowser(t, "http://"+ln.Addr().String())
	if resp, _ := b.get("/healthz"); resp.StatusCode != 200 {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}
	if resp, body := b.get("/about"); resp.StatusCode != 200 || !strings.Contains(body, "AGPL") {
		t.Errorf("/about before setup = %d (AGPL offer present: %v)", resp.StatusCode, strings.Contains(body, "AGPL"))
	}
	if resp, _ := b.get("/"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("/ before setup = %d %s, want 303 /setup", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ := b.post("/setup", "/setup", url.Values{"username": {"admin"},
		"password": {adminPassword}, "password_confirm": {adminPassword}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	for _, path := range []string{"/tracker/corruption", "/admin/fraud/", "/admin/ingestion/"} {
		if resp, body := b.get(path); resp.StatusCode != 200 {
			t.Errorf("%s after setup = %d\n%s", path, resp.StatusCode, body)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type harness struct {
	s     *server.Server
	b     *browser
	clock *fakeClock
	runs  map[string]int
	mu    sync.Mutex
}

// newHarness builds a server over a storetest schema with a pinned clock,
// a two-job schedule (one of them on the mail connector) and a fake job
// runner, then signs in as the first admin.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st := storetest.Open(t)
	h := &harness{clock: &fakeClock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}, runs: map[string]int{}}
	s, err := server.Build(context.Background(), st.Pool, server.Options{
		Log: quiet(), Now: h.clock.Now, DataDir: t.TempDir(),
		Jobs: []jobs.Job{
			{Name: "fraud-recode", Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"recode"}},
			{Name: "fraud-mail-ingest", Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"mail"},
				Connector: jobs.ConnectorMail},
		},
		Run: func(_ context.Context, args []string) (scheduler.Counters, error) {
			h.mu.Lock()
			h.runs[args[0]]++
			h.mu.Unlock()
			if args[0] == "recode" {
				return scheduler.Counters{Fetched: 4}, errors.New("llm endpoint refused the connection")
			}
			return scheduler.Counters{Fetched: 2, New: 1}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.s = s
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	h.b = newBrowser(t, srv.URL)
	if _, err := s.Accounts.Setup(context.Background(), "admin", adminPassword); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.b.post("/login", "/login", url.Values{"username": {"admin"}, "password": {adminPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	return h
}

func (h *harness) tick(t *testing.T) {
	t.Helper()
	if err := h.s.Scheduler.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.s.Scheduler.Wait()
}

func (h *harness) ran(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[name]
}

func TestIngestionStatusShowsFailuresAndPauses(t *testing.T) {
	h := newHarness(t)
	h.tick(t) // both run; recode fails

	_, page := h.b.get("/admin/ingestion/")
	for _, want := range []string{"fraud-recode", "fraud-mail-ingest", "llm endpoint refused the connection",
		"Ingestion is running on schedule", "Pause ingestion"} {
		if !strings.Contains(page, want) {
			t.Errorf("status page missing %q", want)
		}
	}

	// Pause: visible on the page, and the next due tick runs nothing.
	if resp, _ := h.b.post("/admin/ingestion/", "/admin/ingestion/pause", url.Values{"paused": {"on"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("pause = %d", resp.StatusCode)
	}
	_, page = h.b.get("/admin/ingestion/")
	if !strings.Contains(page, "Ingestion is paused") || !strings.Contains(page, "Resume ingestion") {
		t.Errorf("paused state not shown:\n%s", page)
	}
	h.clock.add(time.Minute)
	h.tick(t)
	if h.ran("recode") != 1 || h.ran("mail") != 1 {
		t.Fatalf("jobs ran while paused: %v", h.runs)
	}
	_, page = h.b.get("/admin/ingestion/")
	if !strings.Contains(page, "skipped_paused") {
		t.Errorf("paused skip not visible on the status page")
	}

	// Resume: the next due tick runs again.
	h.b.post("/admin/ingestion/", "/admin/ingestion/pause", url.Values{"paused": {"off"}})
	h.clock.add(time.Minute)
	h.tick(t)
	if h.ran("recode") != 2 || h.ran("mail") != 2 {
		t.Fatalf("jobs did not resume: %v", h.runs)
	}

	// Disabling a connector skips its job but not the others.
	h.b.post("/admin/ingestion/", "/admin/ingestion/connectors", url.Values{"connector": {jobs.ConnectorMail}, "enabled": {"off"}})
	_, page = h.b.get("/admin/ingestion/")
	if !strings.Contains(page, "<strong>disabled</strong>") {
		t.Errorf("disabled connector not shown")
	}
	h.clock.add(time.Minute)
	h.tick(t)
	if h.ran("recode") != 3 || h.ran("mail") != 2 {
		t.Fatalf("after disabling mail: %v", h.runs)
	}
	var last string
	if err := h.s.Pool.QueryRow(context.Background(), `
		SELECT status FROM tracker_job_runs WHERE job = 'fraud-mail-ingest' ORDER BY id DESC LIMIT 1`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != scheduler.StatusSkippedDisabled {
		t.Errorf("mail job last status = %s, want skipped_disabled", last)
	}

	// Every toggle is in the access log.
	var audits int
	if err := h.s.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM tracker_access_log WHERE surface IN ('ingestion_pause', 'ingestion_connector')`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 3 {
		t.Errorf("audited ingestion changes = %d, want 3", audits)
	}
}

func TestIngestionPageIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin, err := h.s.Accounts.UserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Accounts.CreateUser(ctx, admin, localauth.NewUser{Username: "rev",
		Password: adminPassword, Role: localauth.RoleReviewer, Note: "test"}); err != nil {
		t.Fatal(err)
	}
	anon := newBrowser(t, h.b.base)
	if resp, _ := anon.get("/admin/ingestion/"); resp.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("anonymous = %d %s, want 303 to login", resp.StatusCode, resp.Header.Get("Location"))
	}
	rev := newBrowser(t, h.b.base)
	rev.post("/login", "/login", url.Values{"username": {"rev"}, "password": {adminPassword}})
	if resp, _ := rev.get("/admin/ingestion/"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("reviewer = %d, want 403", resp.StatusCode)
	}
	resp, _ := rev.post("/admin/fraud/", "/admin/ingestion/pause", url.Values{"paused": {"on"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("reviewer pause = %d, want 403", resp.StatusCode)
	}
	if paused, _ := h.s.Store.IngestionPaused(ctx); paused {
		t.Error("a reviewer paused ingestion")
	}
}

func TestRunNowIgnoresPauseButRecords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Store.SetIngestionPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	status, err := h.s.Scheduler.RunNow(ctx, "fraud-mail-ingest")
	if err != nil || status != scheduler.StatusOK || h.ran("mail") != 1 {
		t.Fatalf("RunNow = %s %v, runs %v", status, err, h.runs)
	}
	status, err = h.s.Scheduler.RunNow(ctx, "fraud-recode")
	if status != scheduler.StatusError || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("failing RunNow = %s %v", status, err)
	}
	if err := h.s.Store.SetConnectorEnabled(ctx, jobs.ConnectorMail, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Scheduler.RunNow(ctx, "fraud-mail-ingest"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("RunNow on a disabled connector = %v", err)
	}
	if _, err := h.s.Scheduler.RunNow(ctx, "nope"); err == nil {
		t.Fatal("RunNow of an unknown job succeeded")
	}
}

// Without a usable source URL the AGPL source offer can't be made, so the
// server refuses to start.
func TestBuildRefusesBlankSourceURL(t *testing.T) {
	st := storetest.Open(t)
	_, err := server.Build(context.Background(), st.Pool, server.Options{Log: quiet(), DataDir: t.TempDir(),
		LookupEnv: func(k string) (string, bool) {
			if k == "TRACKER_SOURCE_URL" {
				return " ", true
			}
			return "", false
		}})
	if err == nil || !strings.Contains(err.Error(), "TRACKER_SOURCE_URL") {
		t.Fatalf("Build with a blank source URL = %v, want refusal", err)
	}
}

// An admin downloads a complete, verifiable bundle from the status page;
// the download is audited and reviewers can't take one.
func TestAdminExportDownload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, page := h.b.get("/admin/ingestion/")
	m := csrfField.FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "Download export bundle") {
		t.Fatal("no export form on the status page")
	}
	resp, err := h.b.c.PostForm(h.b.base+"/admin/ingestion/export", url.Values{"csrf_token": {m[1]}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/gzip" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="corruption-tracker-`) {
		t.Fatalf("export = %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
	}
	man, err := bundle.Verify(resp.Body)
	if err != nil {
		t.Fatalf("downloaded bundle does not verify: %v", err)
	}
	inst, err := h.s.Store.EnsureInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if man.Source.Kind != bundle.KindStandalone || man.Source.InstanceID != inst.ID {
		t.Errorf("manifest source = %+v, want standalone / %s", man.Source, inst.ID)
	}
	var audited int
	if err := h.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_access_log WHERE surface = 'export_bundle' AND detail = $1`,
		"export_id "+man.ExportID).Scan(&audited); err != nil || audited != 1 {
		t.Errorf("export audit rows = %d (%v), want 1", audited, err)
	}

	admin, _ := h.s.Accounts.UserByName(ctx, "admin")
	if _, err := h.s.Accounts.CreateUser(ctx, admin, localauth.NewUser{Username: "rev2",
		Password: adminPassword, Role: localauth.RoleReviewer, Note: "test"}); err != nil {
		t.Fatal(err)
	}
	rev := newBrowser(t, h.b.base)
	rev.post("/login", "/login", url.Values{"username": {"rev2"}, "password": {adminPassword}})
	if resp, _ := rev.post("/admin/fraud/", "/admin/ingestion/export", url.Values{}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("reviewer export = %d, want 403", resp.StatusCode)
	}
}
