// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package version identifies the running build and carries the AGPL
// network-use source offer (design §2 D1.3):
// anyone interacting with the server over a network is offered its
// Corresponding Source. Version, Commit and SourceURL are set at build time:
//
//	go build -ldflags "-X github.com/lonelycode/corruption-tracker/version.Version=v1.2.0 \
//	  -X github.com/lonelycode/corruption-tracker/version.Commit=$(git rev-parse HEAD)"
//
// Operators who modify the code must point the offer at their modified
// source (TRACKER_SOURCE_URL, or the About setting). It may be changed but
// never blanked: the server refuses to start without one.
package version

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Build-time identity (ldflags -X).
var (
	Version   = "dev"
	Commit    = ""
	SourceURL = "https://github.com/lonelycode/corruption-tracker"
)

// License is the module's licence (D1.1).
const License = "AGPL-3.0"

// LicenseName is its human name.
const LicenseName = "GNU Affero General Public License, version 3"

// SourceURLEnv overrides the source offer's URL.
const SourceURLEnv = "TRACKER_SOURCE_URL"

// ErrNoSourceURL: the network-use source offer has nowhere to point.
var ErrNoSourceURL = errors.New("the AGPL source offer needs a source URL: set " + SourceURLEnv +
	" (or the About setting) to where this server's source code can be downloaded")

// ResolveSourceURL picks the source offer URL: the environment variable,
// then the operator's About setting, then the build-time default. A
// present-but-blank environment value is an explicit blanking and is
// refused, as is a result that isn't an absolute http(s) URL. The server
// must call this at start-up and refuse to start on error.
func ResolveSourceURL(lookupEnv func(string) (string, bool), setting string) (string, error) {
	candidate := SourceURL
	if s := strings.TrimSpace(setting); s != "" {
		candidate = s
	}
	if lookupEnv != nil {
		if v, ok := lookupEnv(SourceURLEnv); ok {
			if strings.TrimSpace(v) == "" {
				return "", fmt.Errorf("%s is set but blank: %w", SourceURLEnv, ErrNoSourceURL)
			}
			candidate = strings.TrimSpace(v)
		}
	}
	if err := CheckSourceURL(candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

// CheckSourceURL validates a source offer URL.
func CheckSourceURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrNoSourceURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("source URL %q is not an absolute http(s) URL: %w", raw, ErrNoSourceURL)
	}
	return nil
}

// SourceLink is the offer's link for this build: the source URL at the
// running commit when it is known (".../tree/<commit>").
func SourceLink(sourceURL, commit string) string {
	sourceURL = strings.TrimRight(strings.TrimSpace(sourceURL), "/")
	if commit = strings.TrimSpace(commit); commit != "" {
		return sourceURL + "/tree/" + commit
	}
	return sourceURL
}
