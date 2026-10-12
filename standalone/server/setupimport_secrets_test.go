// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"html"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// The setup wizard's optional secrets passphrase: the
// field is on the upload form, the import report says what happened to the
// bundle's secrets, and the passphrase is never logged.

const wizardPassphrase = "wizard field passphrase 4242" // gitleaks:allow (synthetic test passphrase)

// secretsBundle exports a source whose settings hold an LLM key (known) and
// a secret this version doesn't recognise (unknown); with secrets, they
// travel in secrets.enc under wizardPassphrase.
func secretsBundle(t *testing.T, withSecrets, known, unknown bool) []byte {
	t.Helper()
	ctx := context.Background()
	src := storetest.Open(t)
	cfg, err := settings.Open(ctx, src.Pool, settings.Options{LookupEnv: func(string) (string, bool) { return "", false },
		DataDir: t.TempDir(), Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	changes := []settings.Change{{Name: settings.SiteName, Value: "Passphrase Watch"}}
	if known {
		changes = append(changes, settings.Change{Name: settings.LLMAPIKey, Value: "sk-field-secret-246810"}) // gitleaks:allow (synthetic)
	}
	if err := cfg.Update(ctx, nil, changes); err != nil {
		t.Fatal(err)
	}
	if unknown {
		ct, err := cfg.SealSecret("legacy.token", "from a later version")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := src.Pool.Exec(ctx, `INSERT INTO tracker_settings (key, value, secret, ciphertext)
			VALUES ('legacy.token', '', true, $1)`, ct); err != nil {
			t.Fatal(err)
		}
	}
	opts := bundle.ExportOptions{Source: bundle.Source{InstanceID: bundle.NewID(), Kind: bundle.KindStandalone}, Log: quiet()}
	if withSecrets {
		opts.IncludeSecrets, opts.Secrets, opts.Passphrase = true, cfg, wizardPassphrase
	}
	path, _, err := bundle.Export(ctx, src.Pool, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var passphraseField = regexp.MustCompile(`(?s)<form method="post" action="/setup/import\?[^"]*" enctype="multipart/form-data"[^>]*>.*?` +
	`<input id="setup-passphrase" type="password" name="passphrase" autocomplete="off">.*?</form>`)

func TestSetupWizardPassphraseField(t *testing.T) {
	withSecrets := secretsBundle(t, true, true, false)
	without := secretsBundle(t, false, true, false)
	mixed := secretsBundle(t, true, true, true)
	onlyUnknown := secretsBundle(t, true, false, true)

	for _, tc := range []struct {
		name       string
		data       []byte
		passphrase string
		imported   bool
		report     string
		llmKey     string
	}{
		{"right passphrase", withSecrets, wizardPassphrase, true,
			"Stored secrets restored from the bundle: 1. Check them with Test LLM and Test mailbox on the Settings page.", "sk-field-secret-246810"},
		{"no passphrase", withSecrets, "", true,
			"The bundle holds encrypted secrets, but no passphrase was entered, so they were not restored: enter the LLM key and mailbox password on the Settings page.", ""},
		{"no secrets in the bundle", without, wizardPassphrase, true,
			"The bundle carries no stored secrets: enter the LLM key and mailbox password on the Settings page.", ""},
		{"known and unrecognised secrets", mixed, wizardPassphrase, true,
			"Stored secrets restored from the bundle: 1. Check them with Test LLM and Test mailbox on the Settings page. Not recognised by this version, so not restored: legacy.token.", "sk-field-secret-246810"},
		{"only unrecognised secrets", onlyUnknown, wizardPassphrase, true,
			"The bundle holds stored secrets, but this version doesn't recognise them (legacy.token), so none were restored: enter the LLM key and mailbox password on the Settings page.", ""},
		{"wrong passphrase", withSecrets, "not the passphrase at all", false,
			"Import failed: nothing imported: bundle: secrets.enc did not decrypt", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs syncBuffer
			s, b := freshServer(t, func(o *server.Options) { o.Log = slog.New(slog.NewTextHandler(&logs, nil)) })
			_, page := b.get("/setup")
			if !passphraseField.MatchString(page) {
				t.Fatalf("no password-type passphrase field on the wizard's upload form:\n%s", page)
			}
			var fields map[string]string
			if tc.passphrase != "" {
				fields = map[string]string{"passphrase": tc.passphrase}
			}
			resp := b.uploadWith("/setup", server.SetupImportPath, tc.data, fields)
			loc := resp.Header.Get("Location")
			if resp.StatusCode != http.StatusSeeOther || strings.HasPrefix(loc, "/setup?imported=") != tc.imported {
				t.Fatalf("upload = %d %s", resp.StatusCode, loc)
			}
			_, page = b.get(loc)
			if !strings.Contains(strings.Join(strings.Fields(html.UnescapeString(page)), " "), tc.report) {
				t.Errorf("import report lacks %q:\n%s", tc.report, page)
			}
			if got := s.SettingsStore.Value(settings.LLMAPIKey); got != tc.llmKey {
				t.Errorf("LLM key after import = %q, want %q", got, tc.llmKey)
			}
			if !tc.imported && s.SettingsStore.Value(settings.SiteName) != "" {
				t.Error("a failed import still wrote settings")
			}
			if tc.passphrase != "" && strings.Contains(logs.String(), tc.passphrase) {
				t.Errorf("the passphrase reached the log:\n%s", logs.String())
			}
		})
	}
}
