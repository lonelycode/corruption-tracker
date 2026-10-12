// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/ingest/newsfeed"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store"
)

// The Settings page: admin only, saves apply at once (branding and the
// LLM-configured state), secrets are never rendered back, Test buttons
// explain what is missing, and every save is audited without values.
func TestSettingsPage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	resp, page := h.b.get("/admin/settings/")
	if resp.StatusCode != 200 || !strings.Contains(page, "LLM not configured") {
		t.Fatalf("settings page = %d\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, `href="/admin/settings"`) {
		t.Error("Settings missing from the admin nav")
	}

	// Test buttons before anything is configured say what to set.
	h.b.post("/admin/settings/", "/admin/settings/test/llm", url.Values{})
	if _, page = h.b.get("/admin/settings/"); !strings.Contains(page, "Set the API key first.") {
		t.Error("LLM test did not explain the missing key")
	}
	h.b.post("/admin/settings/", "/admin/settings/test/imap", url.Values{})
	if _, page = h.b.get("/admin/settings/"); !strings.Contains(page, "Set the mail server first.") {
		t.Error("IMAP test did not explain the missing server")
	}

	const key = "test-value-not-to-be-rendered-WXYZ"
	resp, _ = h.b.post("/admin/settings/", "/admin/settings/", url.Values{
		"site.name":   {"Example Watch"},
		"llm.api_key": {key},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, page = h.b.get("/admin/settings/")
	if strings.Contains(page, key) {
		t.Fatal("the saved secret is rendered back on the page")
	}
	for _, want := range []string{"Settings saved.", "set (…WXYZ)", "Example Watch"} {
		if !strings.Contains(page, want) {
			t.Errorf("page after save missing %q", want)
		}
	}
	if strings.Contains(page, "LLM not configured") {
		t.Error("LLM still reported unconfigured after saving a key")
	}
	if _, home := h.b.get("/admin/ingestion/"); !strings.Contains(home, "Example Watch") {
		t.Error("site name not applied to other pages without a restart")
	}

	// Invalid input is refused with the operator-facing reason.
	h.b.post("/admin/settings/", "/admin/settings/", url.Values{"site.timezone": {"Mars/Base"}})
	if _, page = h.b.get("/admin/settings/"); !strings.Contains(page, "Not saved: Time zone: unknown time zone") {
		t.Error("invalid time zone not explained")
	}

	// Audit names the settings, never the values.
	var detail string
	if err := h.s.Pool.QueryRow(ctx, `SELECT detail FROM tracker_access_log WHERE surface = 'settings_update'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "llm.api_key") || strings.Contains(detail, key) {
		t.Errorf("settings audit detail = %q", detail)
	}

	// Admin only.
	admin, err := h.s.Accounts.UserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Accounts.CreateUser(ctx, admin, localauth.NewUser{Username: "rev",
		Password: adminPassword, Role: localauth.RoleReviewer, Note: "test"}); err != nil {
		t.Fatal(err)
	}
	rev := newBrowser(t, h.b.base)
	rev.post("/login", "/login", url.Values{"username": {"rev"}, "password": {adminPassword}})
	if resp, _ := rev.get("/admin/settings/"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("reviewer GET settings = %d, want 403", resp.StatusCode)
	}
	if resp, _ := rev.post("/admin/fraud/", "/admin/settings/", url.Values{"site.name": {"Hijack"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("reviewer POST settings = %d, want 403", resp.StatusCode)
	}
	anon := newBrowser(t, h.b.base)
	if resp, _ := anon.get("/admin/settings/"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous settings = %d, want 303", resp.StatusCode)
	}
}

// The feed list saved on the Settings page is what the next classify-news
// run polls: CLIDeps reads it per run.
func TestNewsFeedsFromSettingsReachTheRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, page := h.b.get("/admin/settings/"); !strings.Contains(page, `<textarea id="s-news.feeds" name="news.feeds"`) ||
		!strings.Contains(page, "nzherald-crime") {
		t.Fatal("the feed list is not an editable text area showing the defaults")
	}
	resp, _ := h.b.post("/admin/settings/", "/admin/settings/", url.Values{
		"news.feeds": {"court https://court.example.org/feed.xml\r\nblog https://blog.example.org/atom commentary"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	conn, err := server.CLIDeps(h.s.Pool, h.s.SettingsStore, slog.New(slog.DiscardHandler)).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	src, ok := conn.News.(*newsfeed.Source)
	if !ok {
		t.Fatalf("news source = %T, want *newsfeed.Source", conn.News)
	}
	if len(src.Feeds) != 2 || src.Feeds[0].URL != "https://court.example.org/feed.xml" || src.Feeds[1].Corpus != "commentary" {
		t.Errorf("feeds = %+v", src.Feeds)
	}
}

// Turning naming on needs the explicit confirmation, is audited on its own
// and shows who did it; turning it off needs none.
func TestNamingNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, page := h.b.get("/admin/settings/")
	if !strings.Contains(page, `name="naming.enabled"`) || !strings.Contains(page, `name="naming_confirm"`) {
		t.Fatalf("no naming switch with a confirmation on the page:\n%s", page)
	}
	h.b.post("/admin/settings/", "/admin/settings/", url.Values{"naming.enabled": {"on"}})
	if _, page = h.b.get("/admin/settings/"); !strings.Contains(page, "tick the confirmation") || h.s.SettingsStore.NamingOn(ctx) {
		t.Fatal("naming turned on without the confirmation")
	}
	h.b.post("/admin/settings/", "/admin/settings/", url.Values{"naming.enabled": {"on"}, "naming_confirm": {"yes"}})
	if !h.s.SettingsStore.NamingOn(ctx) {
		t.Fatal("confirmed naming did not turn on")
	}
	if _, page = h.b.get("/admin/settings/"); !strings.Contains(page, "changed by admin") || strings.Contains(page, `name="naming_confirm"`) {
		t.Errorf("after enabling: who/when missing, or the confirmation still shown")
	}
	h.b.post("/admin/settings/", "/admin/settings/", url.Values{"naming.enabled": {"off"}})
	if h.s.SettingsStore.NamingOn(ctx) {
		t.Error("naming did not turn off")
	}
	for surface, want := range map[string]int{"naming_enabled": 1, "naming_disabled": 1} {
		var n int
		if err := h.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_access_log WHERE surface = $1`, surface).Scan(&n); err != nil || n != want {
			t.Errorf("%s audit rows = %d, %v; want %d", surface, n, err, want)
		}
	}
}

// Through serve: with naming off the case page withholds the name and
// says why; once an admin turns naming on, the name shows (.46, .67).
func TestCasePageFollowsNamingSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	when := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	c, err := h.s.Store.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Serve switch case", TypologyRaw: "theft by employee"},
		Participants: []store.FraudParticipantInput{{PersonName: "Avery Servefixture",
			Participant: domain.FraudCaseParticipant{Role: "defendant"}}},
		Events: []domain.FraudCaseEvent{{EventType: domain.EventSentencing, EventDate: &when}},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceAgencyRelease, Publisher: "Agency",
			URL: "https://agency.example.org/serve", ContentHash: "serve-switch"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Pool.Exec(ctx, `UPDATE fraud_cases SET status = 'convicted' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	admin, err := h.s.Accounts.UserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Store.VerifyFraudCase(ctx, c.ID, admin.ActorID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/tracker/corruption/case/%d", c.ID)
	_, page := h.b.get(path)
	if strings.Contains(page, "Servefixture") || !strings.Contains(page, "names withheld (naming is off for this tracker)") {
		t.Fatalf("naming off: case page shows the name or the wrong reason:\n%s", page)
	}
	h.b.post("/admin/settings/", "/admin/settings/", url.Values{"naming.enabled": {"on"}, "naming_confirm": {"yes"}})
	if _, page = h.b.get(path); !strings.Contains(page, "Avery Servefixture") || strings.Contains(page, "naming is off") {
		t.Errorf("naming on: the name is not shown")
	}
}
