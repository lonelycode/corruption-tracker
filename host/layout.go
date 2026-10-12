// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package host

import (
	"context"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Page is one rendered tracker page handed to the host chrome.
type Page struct {
	// Name is the page's stable identifier ("report", "case", "explorer",
	// "admin_cases", …) so the host can apply its own per-page touches —
	// e.g. alitheia's title suffixes — without the module carrying branding.
	Name    string
	Title   string        // plain text, without any site suffix
	Section string        // "tracker" | "admin"
	Status  int           // 0 => 200
	Head    template.HTML // extra <head> markup (the tracker stylesheet link)
	Body    template.HTML // rendered tracker content
}

// Layout wraps tracker pages in the host's chrome and owns the per-request
// form plumbing. CSRF *validation* stays host middleware; the tracker only
// renders the token into its forms.
type Layout interface {
	Render(w http.ResponseWriter, r *http.Request, pg Page)
	CSRFToken(r *http.Request) string
	// SetFlash queues a message the host chrome shows on the next render.
	SetFlash(w http.ResponseWriter, level, msg string)
}

// AdminExtension lets the host keep its own pieces of /admin/fraud (in
// alitheia: the team-flag register). Routes are mounted inside the
// tracker's back-office router, behind the Operate gate.
type AdminExtension interface {
	DashboardPanel(ctx context.Context, r *http.Request) (template.HTML, error)
	Routes(r chi.Router)
}

// Banner is an instance-wide notice the host chrome shows above every
// tracker page — today the handoff freeze. A nil *Banner means none.
type Banner struct {
	Level    string // "frozen" | "handed_off"
	Message  string
	LinkURL  string // optional, https only (e.g. the successor instance)
	LinkText string
}
