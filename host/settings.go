// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package host

import "time"

// IMAPConfig connects one mail-ingest poll run.
type IMAPConfig struct {
	Addr     string // host:port (993 for TLS)
	Username string
	Password string
	Folder   string // default INBOX
	// Insecure dials without TLS — tests only.
	Insecure bool
}

// Settings is the host's runtime configuration for the tracker's jobs.
type Settings interface {
	// IMAP returns the alerts mailbox; false means not configured, and
	// mail-ingest --imap skips gracefully.
	IMAP() (IMAPConfig, bool)
	// LLMConfigured reports whether an LLM backend is set up; jobs that
	// need one are skipped when it is not.
	LLMConfigured() bool
	// Timezone is the zone the job schedule's calendar times are in.
	Timezone() *time.Location
}
