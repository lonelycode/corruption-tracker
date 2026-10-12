// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lonelycode/corruption-tracker/store/storetest"
)

type fakeOpener struct{}

func (fakeOpener) OpenSecret(name string, ct []byte) (string, error) {
	return "plain:" + name + ":" + string(ct), nil
}

// settings.json carries the non-secret settings; secrets only ever travel
// re-sealed under the bundle passphrase, and only when asked for.
func TestExportSettingsAndSecrets(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO tracker_settings (key, value, secret, ciphertext) VALUES
		  ('site.name', 'Example Watch', false, NULL),
		  ('imap.folder', 'Alerts', false, NULL),
		  ('llm.api_key', '', true, 'sealed-at-rest')`); err != nil {
		t.Fatal(err)
	}

	archive, m := exportBytes(t, st, pinned(quietLog()))
	if m.Secrets != nil || m.Options.IncludeSecrets {
		t.Fatal("secrets exported without --include-secrets")
	}
	var settingsJSON []byte
	for _, e := range unpack(t, archive) {
		if e.hdr.Name == SecretsFile {
			t.Fatal("secrets.enc present without --include-secrets")
		}
		if e.hdr.Name == SettingsFile {
			settingsJSON = e.body
		}
		if bytes.Contains(e.body, []byte("sealed-at-rest")) {
			t.Errorf("%s carries the at-rest ciphertext", e.hdr.Name)
		}
	}
	want := `{"imap.folder":"Alerts","site.name":"Example Watch"}` + "\n"
	if string(settingsJSON) != want {
		t.Errorf("settings.json = %q, want %q", settingsJSON, want)
	}

	opts := pinned(quietLog())
	opts.IncludeSecrets = true
	opts.Secrets = fakeOpener{}
	opts.Passphrase = "short"
	if _, err := Write(ctx, st.Pool, io.Discard, opts); err == nil {
		t.Error("a short passphrase was accepted")
	}
	opts.Passphrase = "a long enough handoff passphrase"
	archive, m = exportBytes(t, st, opts)
	if m.Secrets == nil || !m.Options.IncludeSecrets {
		t.Fatalf("manifest secrets = %+v", m.Secrets)
	}
	if _, err := Verify(bytes.NewReader(archive)); err != nil {
		t.Fatalf("bundle with secrets fails verification: %v", err)
	}
	entries := unpack(t, archive)
	last := entries[len(entries)-1]
	if last.hdr.Name != SecretsFile {
		t.Fatalf("last entry = %s, want %s (§10.1 order)", last.hdr.Name, SecretsFile)
	}
	if bytes.Contains(last.body, []byte("plain:")) {
		t.Fatal("secrets.enc holds plaintext")
	}
	got, err := OpenSecrets(last.body, "a long enough handoff passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["llm.api_key"] != "plain:llm.api_key:sealed-at-rest" {
		t.Errorf("decrypted secrets = %v", got)
	}
	if _, err := OpenSecrets(last.body, "the wrong passphrase entirely"); !errors.Is(err, ErrBadPassphrase) {
		t.Errorf("wrong passphrase: err = %v", err)
	}
}
