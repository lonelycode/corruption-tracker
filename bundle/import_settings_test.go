// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"context"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func noEnv(string) (string, bool) { return "", false }

const testPassphrase = "a long enough handoff passphrase"

// openSettings opens a settings store with its own instance key.
func openSettings(t *testing.T, st *store.Store) *settings.Service {
	t.Helper()
	svc, err := settings.Open(context.Background(), st.Pool, settings.Options{LookupEnv: noEnv, DataDir: t.TempDir(), Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// sourceWithSettings exports a dataset with two plain settings, one
// secret, and one stored key this version doesn't know.
func sourceWithSettings(t *testing.T) (path string, srcSettingsJSON string) {
	t.Helper()
	a := storetest.Open(t)
	seed(t, a)
	svc := openSettings(t, a)
	ctx := context.Background()
	if err := svc.Update(ctx, nil, []settings.Change{
		{Name: settings.SiteName, Value: "Example Watch"},
		{Name: settings.IMAPFolder, Value: "Alerts"},
		{Name: settings.LLMAPIKey, Value: "sk-test-secret-123456"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pool.Exec(ctx, `INSERT INTO tracker_settings (key, value) VALUES ('legacy.flag', 'on')`); err != nil {
		t.Fatal(err)
	}
	opts := pinned(quietLog())
	opts.IncludeSecrets, opts.Secrets, opts.Passphrase = true, svc, testPassphrase
	p, _, err := Export(ctx, a.Pool, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return p, `{"imap.folder":"Alerts","site.name":"Example Watch"}` + "\n"
}

func storedSecret(t *testing.T, st *store.Store, key string) []byte {
	t.Helper()
	var ct []byte
	_ = st.Pool.QueryRow(context.Background(), `SELECT ciphertext FROM tracker_settings WHERE key = $1 AND secret`, key).Scan(&ct)
	return ct
}

func TestImportSettingsAndSecrets(t *testing.T) {
	path, wantSettings := sourceWithSettings(t)
	ctx := context.Background()

	// With the passphrase: settings and the secret arrive, the secret
	// re-sealed under the target's own key.
	b := storetest.Open(t)
	svcB := openSettings(t, b)
	rep, err := ImportFile(ctx, b.Pool, path, ImportOptions{Settings: svcB, Passphrase: testPassphrase, Log: quietLog()})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rep.SettingsImported != 2 || rep.SecretsImported != 1 || rep.SecretsNotImported != "" ||
		len(rep.SettingsSkipped) != 1 || rep.SettingsSkipped[0] != "legacy.flag" {
		t.Fatalf("report = %+v", rep)
	}
	// ImportFile reloaded the store: the values are live.
	if svcB.Value(settings.SiteName) != "Example Watch" || svcB.Value(settings.IMAPFolder) != "Alerts" ||
		svcB.Value(settings.LLMAPIKey) != "sk-test-secret-123456" {
		t.Errorf("settings after import: name %q folder %q key %q", svcB.Value(settings.SiteName),
			svcB.Value(settings.IMAPFolder), svcB.Value(settings.LLMAPIKey))
	}
	ct := storedSecret(t, b, settings.LLMAPIKey)
	if pt, err := svcB.OpenSecret(settings.LLMAPIKey, ct); err != nil || pt != "sk-test-secret-123456" {
		t.Errorf("secret on the target: %q %v", pt, err)
	}
	if strings.Contains(string(ct), "sk-test") {
		t.Error("secret stored in plaintext")
	}
	// Round trip of the (known) settings.
	archiveB, _ := exportBytes(t, b, pinned(quietLog()))
	if got := string(tableBytes(t, archiveB)[SettingsFile]); got != wantSettings {
		t.Errorf("settings.json after the round trip = %q, want %q", got, wantSettings)
	}

	// Without a passphrase: settings arrive, secrets don't, and the report
	// says why.
	c := storetest.Open(t)
	svcC := openSettings(t, c)
	rep, err = ImportFile(ctx, c.Pool, path, ImportOptions{Settings: svcC, Log: quietLog()})
	if err != nil {
		t.Fatalf("import without passphrase: %v", err)
	}
	if rep.SecretsImported != 0 || !strings.Contains(rep.SecretsNotImported, "no passphrase") ||
		svcC.Value(settings.SiteName) != "Example Watch" || svcC.Value(settings.LLMAPIKey) != "" {
		t.Errorf("no-passphrase import: report %+v, key %q", rep, svcC.Value(settings.LLMAPIKey))
	}

	// A wrong passphrase refuses everything.
	d := storetest.Open(t)
	before := targetRows(t, d)
	if _, err := ImportFile(ctx, d.Pool, path, ImportOptions{Settings: openSettings(t, d),
		Passphrase: "the wrong passphrase entirely", Log: quietLog()}); err == nil ||
		!strings.Contains(err.Error(), "nothing imported") {
		t.Fatalf("wrong passphrase = %v", err)
	}
	var n int
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_settings`).Scan(&n)
	if after := targetRows(t, d); n != 0 || len(after) != len(before) || after["fraud_cases"] != before["fraud_cases"] {
		t.Errorf("wrong passphrase wrote something: %d settings, rows %v -> %v", n, before, after)
	}

	// Settings need a store to go into.
	e := storetest.Open(t)
	if _, err := ImportFile(ctx, e.Pool, path, ImportOptions{Log: quietLog()}); err == nil ||
		!strings.Contains(err.Error(), "no settings store") {
		t.Fatalf("import without a settings store = %v", err)
	}
}

// The bundle's settings win over the target's; settings only the target
// has are kept.
func TestImportSettingsUpsertKeepsTargetOnlyKeys(t *testing.T) {
	path, _ := sourceWithSettings(t)
	ctx := context.Background()
	b := storetest.Open(t)
	svcB := openSettings(t, b)
	if err := svcB.Update(ctx, nil, []settings.Change{
		{Name: settings.SiteName, Value: "Target Name"},
		{Name: settings.SiteAccent, Value: "#336699"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(ctx, b.Pool, path, ImportOptions{Settings: svcB, Log: quietLog()}); err != nil {
		t.Fatal(err)
	}
	if svcB.Value(settings.SiteName) != "Example Watch" || svcB.Value(settings.SiteAccent) != "#336699" {
		t.Errorf("after import: name %q accent %q", svcB.Value(settings.SiteName), svcB.Value(settings.SiteAccent))
	}
}

// Secrets this version doesn't recognise are reported as such, not as "no
// secrets".
func TestImportReportsUnrecognisedSecrets(t *testing.T) {
	ctx := context.Background()
	a := storetest.Open(t)
	seed(t, a)
	if _, err := a.Pool.Exec(ctx, `INSERT INTO tracker_settings (key, value, secret, ciphertext)
		VALUES ('legacy.token', '', true, 'sealed')`); err != nil {
		t.Fatal(err)
	}
	opts := pinned(quietLog())
	opts.IncludeSecrets, opts.Secrets, opts.Passphrase = true, fakeOpener{}, testPassphrase
	path, _, err := Export(ctx, a.Pool, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	b := storetest.Open(t)
	rep, err := ImportFile(ctx, b.Pool, path, ImportOptions{Settings: openSettings(t, b), Passphrase: testPassphrase, Log: quietLog()})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rep.SecretsImported != 0 || rep.SecretsNotImported != "" ||
		len(rep.SecretsSkipped) != 1 || rep.SecretsSkipped[0] != "legacy.token" {
		t.Fatalf("report = %+v", rep)
	}
	var n int
	_ = b.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_settings WHERE key = 'legacy.token'`).Scan(&n)
	if n != 0 {
		t.Error("an unrecognised secret was stored")
	}
}
