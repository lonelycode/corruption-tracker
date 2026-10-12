// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/standalone/server"
)

// Site name, logo and accent are changed from the admin Settings page of a
// running server and show on the next page — header, <title>, accent CSS,
// logo and the reader's suppression notice — with no restart.
func TestBrandingFromSettingsWithoutRestart(t *testing.T) {
	s, base := serving(t, server.Options{})
	seedAccounts(t, s.Accounts)
	seedCases(t, s.Store)
	admin, viewer := newBrowser(t, base), newBrowser(t, base)
	admin.login(t, "admin")
	viewer.login(t, "viewer")

	if _, body := viewer.get("/tracker/corruption/case/1"); !strings.Contains(body, "Corruption &amp; Fraud Tracker screens for suppression") {
		t.Fatalf("default operator name missing from the notice")
	}
	resp, body := admin.post(server.SettingsPath, server.SettingsPath+"/", url.Values{
		"site.name": {"Harbour Integrity Watch"}, "site.logo_url": {"https://example.org/logo.png"}, "site.accent": {"#1a6b4c"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save settings = %d\n%s", resp.StatusCode, body)
	}
	_, body = viewer.get("/tracker/corruption/case/1")
	for _, want := range []string{
		"— Harbour Integrity Watch</title>",
		`<span>Harbour Integrity Watch</span>`,
		`<img src="https://example.org/logo.png"`,
		"--accent: #1a6b4c",
		"Harbour Integrity Watch screens for suppression",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("after the Settings change the page lacks %q", want)
		}
	}
	if strings.Contains(body, "Corruption &amp; Fraud Tracker screens") {
		t.Error("the notice still names the old operator")
	}
}
