// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package host

import (
	"context"
	"net/http"
)

// Principal is the authenticated requester as the host knows them.
type Principal struct {
	ExternalRef string // stable host identity: "alitheia:user:42", "local:7"
	Label       string // display + audit label (the email in alitheia)
	Email       string // optional; recorded on the actor row
	OrgRef      string // host org scope for audit, e.g. "alitheia:team:3"; "" if none
}

// Access is the requester's tracker entitlement set (design doc D3):
// admin ⊃ operate ⊃ view, with export a separate grant that operators imply.
type Access struct {
	View    bool // report + verified case pages
	Export  bool // explorer + CSV/Markdown export
	Operate bool // back office, dev snapshots, unverified case pages
	Admin   bool // instance admin (full admin in alitheia)
}

// Authorizer answers who is asking and what they may see. The tracker
// reproduces today's gate semantics from it: anonymous requests get a 303 to
// LoginURL; signed-in requests without the entitlement get 303 (reader
// surfaces), 404 (capability boundaries) or 403 (back office).
type Authorizer interface {
	// Principal returns the signed-in requester; false means anonymous.
	Principal(r *http.Request) (Principal, bool)
	// Access returns the requester's entitlements. Anonymous requesters get
	// the zero Access whatever the implementation returns.
	Access(ctx context.Context, r *http.Request) (Access, error)
	// LoginURL is the 303 target for anonymous / unentitled requesters.
	LoginURL(r *http.Request) string
}

// Entitlement names used in the access audit — the alitheia team-flag names,
// kept so audit rows stay identical.
const (
	EntitlementView   = "corruption_tracker"
	EntitlementExport = "corruption_tracker_export"
)

// AccessAuditor records Privacy-Act access to person-level data. entitlement
// is EntitlementView or EntitlementExport; surface and detail are the strings
// the tracker has always written ("report_view", "admin_case_view", …).
// Callers log and ignore errors: auditing never blocks a render.
type AccessAuditor interface {
	LogAccess(ctx context.Context, p Principal, entitlement, surface, detail string) error
}
