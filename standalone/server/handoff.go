// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server

import (
	"bytes"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/web"
)

// HandoffPath is the admin-only handoff page (design
// §11): freeze, abort, complete on a source; the checklist and Resume
// ingestion on an imported target.
const HandoffPath = "/admin/handoff"

type handoffPage struct {
	st     *store.Store
	layout host.Layout
	audit  host.AccessAuditor
	log    *slog.Logger
	tmpl   *template.Template
	// FreezeTimeout bounds the freeze barrier wait.
	freezeTimeout time.Duration
}

func newHandoffPage(st *store.Store, layout host.Layout, audit host.AccessAuditor, log *slog.Logger) (*handoffPage, error) {
	t, err := template.New("handoff.html.tmpl").Funcs(template.FuncMap{
		"when": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
	}).ParseFS(templateFS, "templates/handoff.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &handoffPage{st: st, layout: layout, audit: audit, log: log, tmpl: t, freezeTimeout: 2 * time.Minute}, nil
}

func (p *handoffPage) routes(r chi.Router) {
	r.Use(requireAdmin)
	r.Get("/", p.show)
	r.Post("/begin", p.begin)
	r.Post("/abort", p.abort)
	r.Post("/complete", p.complete)
	r.Post("/resume", p.resume)
}

// checklist is §11.5, shown on both instances during a handoff.
var checklist = []string{
	"A data-transfer agreement is signed between operators: no person-level republication, suppression and Clean Slate obligations pass through, corrections are actioned.",
	"The alert mailbox is transferred (same credentials), or Courts of NZ / Capital Letter subscriptions are re-pointed to the target's mailbox.",
	"The target's LLM API key is entered and the test connection passes.",
	"The target's first admin has claimed their legacy actor, and a second admin or reviewer exists (quarantine release needs two distinct reviewers).",
	"The outstanding review queue has been reviewed: counts by state, and quarantined items.",
	"Partner-org viewer accounts are created on the target (access is not transferred).",
	"The source has run handoff complete, and the bundle and any secrets.enc passphrase are destroyed per the agreement.",
}

func (p *handoffPage) show(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state, err := p.st.GetInstanceState(ctx)
	if err != nil {
		p.fail(w, err)
		return
	}
	running, err := p.st.RunningJobs(ctx)
	if err != nil {
		p.fail(w, err)
		return
	}
	var body bytes.Buffer
	if err := p.tmpl.Execute(&body, struct {
		State     store.InstanceState
		Running   []string
		Checklist []string
		Imported  bool
		CSRF      string
	}{state, running, checklist, state.ImportedFrom != nil, p.layout.CSRFToken(r)}); err != nil {
		p.fail(w, err)
		return
	}
	p.layout.Render(w, r, host.Page{Name: "admin_handoff", Title: "Handoff", Section: "admin",
		Head: template.HTML(`<link rel="stylesheet" href="` + web.StaticPrefix + `tracker.css">`),
		Body: template.HTML(body.String())})
}

func (p *handoffPage) begin(w http.ResponseWriter, r *http.Request) {
	u, _ := localauth.CurrentUser(r)
	state, err := p.st.Freeze(r.Context(), "local:"+u.Username, p.freezeTimeout)
	if errors.Is(err, store.ErrFrozen) {
		p.done(w, r, "error", "The tracker is already frozen or handed off.")
		return
	}
	if err != nil {
		p.done(w, r, "error", "Freeze failed: "+err.Error())
		return
	}
	p.auditAdmin(r, "handoff_begin", "freeze_id "+state.FreezeID)
	p.done(w, r, "success", "Frozen: the dataset is read-only and scheduled ingestion has stopped. "+
		"When no jobs are running, download the handoff bundle from Ingestion → Export.")
}

func (p *handoffPage) abort(w http.ResponseWriter, r *http.Request) {
	if err := p.st.AbortHandoff(r.Context()); err != nil {
		p.done(w, r, "error", err.Error())
		return
	}
	p.auditAdmin(r, "handoff_abort", "")
	p.done(w, r, "success", "Handoff aborted: back to normal operation. Destroy any bundle taken under the aborted freeze.")
}

func (p *handoffPage) complete(w http.ResponseWriter, r *http.Request) {
	exportID := strings.TrimSpace(r.PostFormValue("export_id"))
	successor := strings.TrimSpace(r.PostFormValue("successor_url"))
	if exportID == "" || successor == "" {
		p.done(w, r, "error", "Enter the handoff bundle's export id and the successor's https URL.")
		return
	}
	if err := p.st.CompleteHandoff(r.Context(), exportID, successor); err != nil {
		p.done(w, r, "error", err.Error())
		return
	}
	p.auditAdmin(r, "handoff_complete", "export_id "+exportID+" successor "+successor)
	p.done(w, r, "success", "Handoff complete: this tracker stays read-only and points to "+successor+".")
}

func (p *handoffPage) resume(w http.ResponseWriter, r *http.Request) {
	if err := p.st.ResumeAfterImport(r.Context(), cli.Connectors()); err != nil {
		p.done(w, r, "error", err.Error())
		return
	}
	p.auditAdmin(r, "handoff_resume", "")
	p.done(w, r, "success", "Ingestion resumed: every connector catches up from 24 hours before the source froze.")
}

func (p *handoffPage) done(w http.ResponseWriter, r *http.Request, level, msg string) {
	p.layout.SetFlash(w, level, msg)
	http.Redirect(w, r, HandoffPath, http.StatusSeeOther)
}

func (p *handoffPage) auditAdmin(r *http.Request, surface, detail string) {
	u, _ := localauth.CurrentUser(r)
	if err := p.audit.LogAccess(r.Context(), u.Principal(), "admin", surface, detail); err != nil {
		p.log.Error("audit handoff change", "surface", surface, "err", err)
	}
}

func (p *handoffPage) fail(w http.ResponseWriter, err error) {
	p.log.Error("handoff page", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// frozenWritesBlocked answers 503 to every state-changing request while the
// tracker is frozen or handed off (§11.3). The database guard is the real
// protection; this turns it into a clear page instead of an error. Sign-in,
// the handoff page itself, accounts and settings (the checklist needs them)
// stay usable.
func frozenWritesBlocked(st *store.Store, log *slog.Logger) func(http.Handler) http.Handler {
	allowed := []string{"/login", "/logout", HandoffPath, IngestionPath + "/export", "/admin/settings", "/admin/users"}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			for _, a := range allowed {
				if r.URL.Path == a || strings.HasPrefix(r.URL.Path, a+"/") {
					next.ServeHTTP(w, r)
					return
				}
			}
			state, err := st.GetInstanceState(r.Context())
			if err != nil {
				log.Error("frozen check", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if state.Writable() {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, web.FrozenMessage(state), http.StatusServiceUnavailable)
		})
	}
}
