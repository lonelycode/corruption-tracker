// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package version

import (
	"errors"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestResolveSourceURL(t *testing.T) {
	def := SourceURL
	t.Cleanup(func() { SourceURL = def })

	if got, err := ResolveSourceURL(env(nil), ""); err != nil || got != def {
		t.Errorf("default: %q %v", got, err)
	}
	if got, _ := ResolveSourceURL(env(nil), "https://git.example/fork"); got != "https://git.example/fork" {
		t.Errorf("setting override: %q", got)
	}
	if got, _ := ResolveSourceURL(env(map[string]string{SourceURLEnv: "https://env.example/src"}), "https://git.example/fork"); got != "https://env.example/src" {
		t.Errorf("env beats setting: %q", got)
	}
	// Blanking is refused, whichever way it's attempted.
	if _, err := ResolveSourceURL(env(map[string]string{SourceURLEnv: "  "}), ""); !errors.Is(err, ErrNoSourceURL) {
		t.Errorf("blank env: %v", err)
	}
	SourceURL = ""
	if _, err := ResolveSourceURL(env(nil), ""); !errors.Is(err, ErrNoSourceURL) {
		t.Errorf("blank build default: %v", err)
	}
	for _, bad := range []string{"not a url", "ftp://x.example/src", "/relative/path"} {
		if err := CheckSourceURL(bad); !errors.Is(err, ErrNoSourceURL) {
			t.Errorf("CheckSourceURL(%q) = %v", bad, err)
		}
	}
}

func TestSourceLink(t *testing.T) {
	if got := SourceLink("https://github.com/x/y/", "abc123"); got != "https://github.com/x/y/tree/abc123" {
		t.Errorf("with commit: %q", got)
	}
	if got := SourceLink("https://github.com/x/y", ""); got != "https://github.com/x/y" {
		t.Errorf("without commit: %q", got)
	}
}
