// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package settings_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/ingest/newsfeed"
	"github.com/lonelycode/corruption-tracker/llm"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func open(t *testing.T, st *store.Store, dir string, e map[string]string) *settings.Service {
	t.Helper()
	s, err := settings.Open(context.Background(), st.Pool, settings.Options{LookupEnv: env(e), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func field(t *testing.T, s *settings.Service, name string) settings.Field {
	t.Helper()
	for _, f := range s.Fields() {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %s", name)
	return settings.Field{}
}

func TestSettingsStoreSecretsEncryptedAndApplyLive(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, st, dir, nil)

	// No secret yet, so no key file yet.
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key file before any secret: %v", err)
	}

	// Defaults.
	if s.LLMConfigured() {
		t.Error("fresh instance must not report an LLM")
	}
	if _, ok := s.IMAP(); ok {
		t.Error("fresh instance must not report a mailbox")
	}
	if s.Value(settings.IMAPFolder) != "INBOX" || s.Timezone() == nil || s.Timezone().String() != "Pacific/Auckland" {
		t.Errorf("defaults wrong: folder %q tz %v", s.Value(settings.IMAPFolder), s.Timezone())
	}

	actor, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "local:1", Label: "admin@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	const apiKey = "test-llm-credential-0123456789abcd"
	if err := s.Update(ctx, &actor, []settings.Change{
		{Name: settings.LLMAPIKey, Value: apiKey},
		{Name: settings.IMAPAddr, Value: "imap.example.org"},
		{Name: settings.IMAPUser, Value: "alerts@example.org"},
		{Name: settings.IMAPPassword, Value: "mailbox-pass"},
		{Name: settings.SiteName, Value: "Example Tracker"},
		{Name: settings.CrawlerName, Value: "ExampleBot"},
		{Name: settings.CrawlerContact, Value: "https://example.org/bot"},
	}); err != nil {
		t.Fatal(err)
	}

	// The first secret generates the key file, owner-only.
	fi, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secret.key mode = %v, want 0600", fi.Mode().Perm())
	}

	// Applied without a restart.
	if !s.LLMConfigured() {
		t.Error("LLM key saved but not configured")
	}
	if c, ok := s.IMAP(); !ok || c.Addr != "imap.example.org" || c.Password != "mailbox-pass" || c.Folder != "INBOX" {
		t.Errorf("IMAP = %+v %v", c, ok)
	}
	if got := s.Branding(ctx).Name; got != "Example Tracker" {
		t.Errorf("branding name = %q", got)
	}
	if got := s.Crawler().UserAgent(); got != "ExampleBot/1.0 (+https://example.org/bot)" {
		t.Errorf("crawler UA = %q", got)
	}

	// Rows: secrets are ciphertext only, never the plaintext.
	rows, err := st.Pool.Query(ctx, `SELECT key, value, secret, ciphertext FROM tracker_settings`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var k, v string
		var secret bool
		var ct []byte
		if err := rows.Scan(&k, &v, &secret, &ct); err != nil {
			t.Fatal(err)
		}
		n++
		if secret && (v != "" || len(ct) == 0 || bytes.Contains(ct, []byte(apiKey)) || bytes.Contains(ct, []byte("mailbox-pass"))) {
			t.Errorf("secret %s stored badly: value %q, %d ciphertext bytes", k, v, len(ct))
		}
	}
	rows.Close()
	if n != 7 {
		t.Errorf("stored rows = %d, want 7", n)
	}

	// The page sees a hint, never the secret.
	f := field(t, s, settings.LLMAPIKey)
	if f.Value != "" || !f.IsSet || f.Hint != "…abcd" || f.UpdatedBy != "admin@example.org" {
		t.Errorf("secret field = %+v", f)
	}

	// An empty secret on save keeps it; Clear removes it.
	if err := s.Update(ctx, &actor, []settings.Change{{Name: settings.LLMAPIKey, Value: ""}}); err != nil {
		t.Fatal(err)
	}
	if s.Value(settings.LLMAPIKey) != apiKey {
		t.Error("empty secret input must keep the stored secret")
	}
	if err := s.Update(ctx, &actor, []settings.Change{{Name: settings.LLMAPIKey, Clear: true}}); err != nil {
		t.Fatal(err)
	}
	if s.LLMConfigured() {
		t.Error("cleared key still configured")
	}

	// A fresh Service over the same key file reads everything back.
	s2 := open(t, st, dir, nil)
	if c, ok := s2.IMAP(); !ok || c.Password != "mailbox-pass" {
		t.Errorf("reopened IMAP = %+v %v", c, ok)
	}

	// A different key (lost key file) cannot decrypt: flagged, not leaked.
	s3 := open(t, st, t.TempDir(), nil)
	if c, _ := s3.IMAP(); c.Password != "" {
		t.Error("password decrypted with the wrong key")
	}
	if f := field(t, s3, settings.IMAPPassword); !f.Undecryptable || !f.IsSet {
		t.Errorf("undecryptable secret field = %+v", f)
	}
}

func TestSettingsEnvOverridesAndLocks(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	s := open(t, st, "", map[string]string{
		settings.SecretEnv:    "a long operator secret",
		"TRACKER_LLM_API_KEY": "env-key",
		"TRACKER_TIMEZONE":    "UTC",
	})
	if !s.LLMConfigured() || s.Timezone().String() != "UTC" {
		t.Errorf("env not applied: llm %v tz %v", s.LLMConfigured(), s.Timezone())
	}
	if f := field(t, s, settings.LLMAPIKey); !f.Locked || f.Source != settings.SourceEnv {
		t.Errorf("env field not locked: %+v", f)
	}
	err := s.Update(ctx, nil, []settings.Change{{Name: settings.SiteTimezone, Value: "Europe/London"}})
	if !errors.Is(err, settings.ErrLocked) {
		t.Errorf("change to locked field: err = %v, want ErrLocked", err)
	}
	// Unchanged locked values (what a form round-trips) are accepted.
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.SiteTimezone, Value: "UTC"}, {Name: settings.SiteName, Value: "X"}}); err != nil {
		t.Errorf("unchanged locked value refused: %v", err)
	}
	// TRACKER_SECRET derives the key: secrets work with no data directory.
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.IMAPPassword, Value: "p"}}); err != nil {
		t.Fatal(err)
	}
	s2 := open(t, st, "", map[string]string{settings.SecretEnv: "a long operator secret"})
	if s2.Value(settings.IMAPPassword) != "p" {
		t.Error("TRACKER_SECRET-derived key did not round-trip")
	}
}

func TestSettingsValidation(t *testing.T) {
	st := storetest.Open(t)
	s := open(t, st, t.TempDir(), nil)
	for _, c := range []settings.Change{
		{Name: settings.SiteTimezone, Value: "Mars/Olympus"},
		{Name: settings.SiteAccent, Value: "red"},
		{Name: settings.CrawlerContact, Value: "example.org"},
		{Name: settings.SiteLogoURL, Value: "http://example.org/logo.png"},
		{Name: settings.LLMProvider, Value: "other"},
		{Name: settings.SiteName, Value: "two\nlines"},
		{Name: "no.such.setting", Value: "x"},
	} {
		if err := s.Update(context.Background(), nil, []settings.Change{c}); err == nil {
			t.Errorf("%s=%q accepted", c.Name, c.Value)
		}
	}
}

type fakeLLM struct {
	content string
	err     error
}

func (f fakeLLM) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Content: []byte(f.content), Model: "fake-model"}, f.err
}

func TestSettingsTestLLM(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	var next fakeLLM
	s, err := settings.Open(ctx, st.Pool, settings.Options{LookupEnv: env(nil), DataDir: t.TempDir(),
		NewLLM: func(settings.LLMConfig) llm.Completer { return next }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TestLLM(ctx); err == nil {
		t.Error("TestLLM without a key should explain what to set")
	}
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.LLMAPIKey, Value: "k"}}); err != nil {
		t.Fatal(err)
	}
	next = fakeLLM{content: `{"ok":true}`}
	if msg, err := s.TestLLM(ctx); err != nil || msg == "" {
		t.Errorf("TestLLM ok: %q %v", msg, err)
	}
	next = fakeLLM{err: errors.New("401 invalid x-api-key")}
	if _, err := s.TestLLM(ctx); err == nil {
		t.Error("TestLLM should report the provider error")
	}
}

// Without a usable data directory, plain settings still work; only saving a
// secret fails, and it says why. Fixing the volume needs no restart.
func TestSettingsKeyLoadedOnlyForSecrets(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, st, filepath.Join(blocker, "data"), nil)
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.SiteName, Value: "Example Watch"}}); err != nil {
		t.Fatalf("plain setting without a data directory: %v", err)
	}
	if s.SiteName() != "Example Watch" {
		t.Errorf("site name = %q", s.SiteName())
	}
	err := s.Update(ctx, nil, []settings.Change{{Name: settings.LLMAPIKey, Value: "test-llm-credential-abcdef"}})
	if err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("secret without a data directory: err = %v", err)
	}
	if s.LLMConfigured() {
		t.Error("a failed secret save must not apply")
	}
	if _, err := settings.Open(ctx, st.Pool, settings.Options{LookupEnv: env(map[string]string{settings.SecretEnv: " "}),
		DataDir: t.TempDir()}); err == nil {
		t.Error("a blank TRACKER_SECRET must refuse to open")
	}
}

// The news feed list is a multi-line setting: it defaults to
// newsfeed.DefaultFeeds, a text area's CRLFs and padding don't count as a
// change, a bad line is refused, and the env var locks it.
func TestSettingsNewsFeeds(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	s := open(t, st, t.TempDir(), nil)

	def, err := s.NewsFeeds()
	if err != nil || len(def) == 0 || field(t, s, settings.NewsFeeds).Value != newsfeed.DefaultFeeds {
		t.Fatalf("default feeds = %v, %v", def, err)
	}
	// Submitting the default back, as a browser would, stores nothing.
	asBrowser := "  " + strings.ReplaceAll(newsfeed.DefaultFeeds, "\n", "  \r\n") + "\r\n"
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.NewsFeeds, Value: asBrowser}}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_settings WHERE key = $1`, settings.NewsFeeds).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("default resubmitted: %d rows, %v", rows, err)
	}

	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.NewsFeeds,
		Value: "court https://court.example.org/feed.xml\r\nblog https://blog.example.org/atom commentary\r\n"}}); err != nil {
		t.Fatal(err)
	}
	feeds, err := s.NewsFeeds()
	if err != nil || len(feeds) != 2 || feeds[0].Slug != "court" || feeds[1].Corpus != "commentary" {
		t.Errorf("custom feeds = %+v, %v", feeds, err)
	}
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.NewsFeeds, Value: "court ftp://court.example.org/feed"}}); err == nil {
		t.Error("a non-http feed was accepted")
	}
	if err := s.Update(ctx, nil, []settings.Change{{Name: settings.NewsFeeds, Value: "# none for now"}}); err != nil {
		t.Fatal(err)
	}
	if feeds, _ := s.NewsFeeds(); len(feeds) != 0 {
		t.Errorf("commented-out list = %+v, want no feeds", feeds)
	}

	locked := open(t, st, t.TempDir(), map[string]string{"TRACKER_NEWS_FEEDS": "env https://env.example.org/rss"})
	if f := field(t, locked, settings.NewsFeeds); !f.Locked {
		t.Error("TRACKER_NEWS_FEEDS did not lock the field")
	}
	if feeds, _ := locked.NewsFeeds(); len(feeds) != 1 || feeds[0].Slug != "env" {
		t.Errorf("env feeds = %+v", feeds)
	}
	bad := open(t, st, t.TempDir(), map[string]string{"TRACKER_NEWS_FEEDS": "not a url"})
	if _, err := bad.NewsFeeds(); err == nil {
		t.Error("an invalid TRACKER_NEWS_FEEDS was not reported")
	}
}

// Naming is off on a fresh instance, has no environment override (turning
// it on must be a recorded admin action), and never arrives by import.
func TestNamingSwitchSetting(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	s := open(t, st, t.TempDir(), map[string]string{"TRACKER_NAMING": "on", "TRACKER_NAMING_ENABLED": "on"})
	if s.NamingOn(ctx) {
		t.Fatal("a fresh instance names people")
	}
	if f := field(t, s, settings.NamingEnabled); f.Locked || f.Env != "" {
		t.Errorf("naming field = %+v, want no env lock", f)
	}
	if known, _ := s.SettingKind(settings.NamingEnabled); known {
		t.Error("naming.enabled would be carried over by a bundle import")
	}
	if known, _ := s.SettingKind(settings.SiteName); !known {
		t.Error("ordinary settings must still import")
	}
	actor, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "local:1", Label: "admin@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, &actor, []settings.Change{{Name: settings.NamingEnabled, Value: "maybe"}}); err == nil {
		t.Error("an invalid value was accepted")
	}
	if err := s.Update(ctx, &actor, []settings.Change{{Name: settings.NamingEnabled, Value: "on"}}); err != nil {
		t.Fatal(err)
	}
	if !s.NamingOn(ctx) {
		t.Error("naming did not turn on")
	}
	if f := field(t, s, settings.NamingEnabled); f.UpdatedBy != "admin@example.org" || f.UpdatedAt.IsZero() {
		t.Errorf("who/when not recorded: %+v", f)
	}
}
