// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/standalone/server"
)

// Handoff freeze UX through serve: while frozen, every
// page — tracker, back office and the server's own — carries the banner and
// keeps reading; dataset writes answer 503; signing in and out still works.
// A handed-off instance links to its successor.
func TestServeFrozenAndHandedOff(t *testing.T) {
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	seedCases(t, s.Store)
	ctx := context.Background()
	admin, viewer := newBrowser(t, base), newBrowser(t, base)
	admin.login(t, "admin")
	viewer.login(t, "viewer")

	pages := []struct {
		b    *browser
		path string
	}{
		{viewer, "/tracker/corruption"}, {viewer, "/tracker/corruption/case/1"},
		{admin, "/admin/fraud/"}, {admin, "/admin/fraud/cases/1"},
		{admin, "/admin/users"}, {admin, "/admin/settings"}, {admin, "/admin/ingestion/"},
		{admin, "/admin/handoff"}, {admin, "/about"},
	}
	const frozen = `<div class="sa-banner sa-banner-frozen" role="status">This tracker is frozen for a handoff`
	for _, p := range pages {
		if _, body := p.b.get(p.path); strings.Contains(body, "sa-banner") {
			t.Fatalf("%s shows a banner before any freeze", p.path)
		}
	}

	if _, err := s.Store.Freeze(ctx, "test", time.Minute); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	for _, p := range pages {
		resp, body := p.b.get(p.path)
		if resp.StatusCode != http.StatusOK || strings.Count(body, "sa-banner-frozen") != 1 || !strings.Contains(body, frozen) {
			t.Errorf("frozen %s = %d, exactly one frozen banner: %v", p.path, resp.StatusCode, strings.Count(body, "sa-banner-frozen") == 1)
		}
	}
	// Dataset writes: back office and ingestion toggles.
	for _, path := range []string{"/admin/fraud/claims/2/release-vote", "/admin/fraud/submit",
		"/admin/fraud/cases/2/verify", "/admin/ingestion/pause", "/admin/ingestion/connectors"} {
		resp, body := admin.post("/admin/fraud/", path, url.Values{"note": {"x"}})
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "read-only") {
			t.Errorf("frozen POST %s = %d (%.80s), want 503 read-only", path, resp.StatusCode, body)
		}
	}
	// Operational writes stay allowed (§11.3): settings (with a note that
	// they won't travel in the bundle) and user accounts.
	if _, body := admin.get("/admin/settings"); !strings.Contains(body, "changes made after the handoff export are not carried in the bundle") {
		t.Error("frozen settings page lacks the handoff note")
	}
	if resp, body := admin.post("/admin/settings", "/admin/settings", url.Values{"site.name": {"Frozen Watch"}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("frozen settings save = %d (%.80s), want 303", resp.StatusCode, body)
	}
	// Account changes that only touch tracker_users work (role, disable).
	// Creating an account also creates a tracker_actors row, which is part
	// of the dataset and so refused by the freeze guard: the page says so
	// instead of failing.
	viewerUser, err := s.Accounts.UserByName(ctx, "viewer_export")
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := admin.post("/admin/users", "/admin/users/"+strconv.FormatInt(viewerUser.ID, 10)+"/role",
		url.Values{"role": {"reviewer"}, "note": {"handoff staffing"}}); resp.StatusCode != http.StatusSeeOther ||
		strings.HasPrefix(resp.Header.Get("X-Flash"), "error") {
		t.Errorf("frozen role change = %d (%.80s), want 303", resp.StatusCode, body)
	}
	if u, _ := s.Accounts.UserByName(ctx, "viewer_export"); u.Role != "reviewer" {
		t.Errorf("role change while frozen not applied: %s", u.Role)
	}
	resp, _ := admin.post("/admin/users", "/admin/users", url.Values{"username": {"successor"},
		"password": {userPassword}, "role": {"admin"}, "note": {"successor operator"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("frozen user create = %d, want 303 back to Users with an explanation", resp.StatusCode)
	}
	if _, body := admin.get("/admin/users"); !strings.Contains(body, "Create the account on the new instance") {
		t.Errorf("frozen user create: the refusal isn't explained on the Users page")
	}
	// Signing out and back in still works.
	if resp, _ := viewer.post("/tracker/corruption", "/logout", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("frozen logout = %d", resp.StatusCode)
	}
	viewer.login(t, "viewer")

	// Abort: back to normal, no banner, writes accepted.
	if err := s.Store.AbortHandoff(ctx); err != nil {
		t.Fatal(err)
	}
	if _, body := viewer.get("/tracker/corruption"); strings.Contains(body, "sa-banner") {
		t.Error("banner after abort")
	}
	if _, body := admin.get("/admin/settings"); strings.Contains(body, "not carried in the bundle") {
		t.Error("settings handoff note after abort")
	}
	if resp, _ := admin.post("/admin/fraud/", "/admin/fraud/claims/2/release-vote", url.Values{"note": {"x"}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("POST after abort = %d, want 303", resp.StatusCode)
	}

	// Handed off for good: read-only, pointing at the successor.
	if _, err := s.Store.Freeze(ctx, "test", time.Minute); err != nil {
		t.Fatal(err)
	}
	id := bundle.NewID()
	if err := s.Store.RecordHandoffExport(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.CompleteHandoff(ctx, id, "https://next.example.org/tracker"); err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		_, body := p.b.get(p.path)
		if !strings.Contains(body, `sa-banner-handed_off`) ||
			!strings.Contains(body, `<a href="https://next.example.org/tracker" rel="noopener">next.example.org</a>`) {
			t.Errorf("handed-off %s lacks the successor banner", p.path)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/healthz", nil)
	if resp, err := admin.c.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz while handed off: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
