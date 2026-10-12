// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/backup"
	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/scheduler"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/version"
	"github.com/lonelycode/corruption-tracker/web"
)

// IngestionPath is the admin-only ingestion status page.
const IngestionPath = "/admin/ingestion"

//go:embed templates/*.html.tmpl
var templateFS embed.FS

type ingestionPage struct {
	st     *store.Store
	sched  *scheduler.Scheduler
	layout host.Layout
	audit  host.AccessAuditor
	// backups, when set, adds the last-backup line; nil leaves it out.
	backups *backup.Runner
	log     *slog.Logger
	tmpl    *template.Template
}

func newIngestionPage(st *store.Store, sched *scheduler.Scheduler, layout host.Layout, audit host.AccessAuditor, backups *backup.Runner, log *slog.Logger) (*ingestionPage, error) {
	t, err := template.New("ingestion.html.tmpl").Funcs(template.FuncMap{
		"when": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"at": func(t time.Time) *time.Time { return &t },
	}).ParseFS(templateFS, "templates/ingestion.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &ingestionPage{st: st, sched: sched, layout: layout, audit: audit, backups: backups, log: log, tmpl: t}, nil
}

func (p *ingestionPage) routes(r chi.Router) {
	r.Use(requireAdmin)
	r.Get("/", p.show)
	r.Post("/pause", p.setPause)
	r.Post("/connectors", p.setConnector)
	r.Post("/export", p.export)
}

// requireAdmin: anonymous → sign in; signed in but not admin → 403.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := localauth.CurrentUser(r)
		if !ok {
			http.Redirect(w, r, localauth.LoginURL(r), http.StatusSeeOther)
			return
		}
		if u.Role != localauth.RoleAdmin || u.Disabled {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type connectorRow struct {
	Name, Label string
	Enabled     bool
}

// Connectors are the operator-switchable ingestion sources: each agency
// feed, the news classifier and the alerts mailbox.
func Connectors() []connectorRow {
	out := make([]connectorRow, 0, len(fraudagency.Sources)+2)
	for _, s := range fraudagency.Sources {
		out = append(out, connectorRow{Name: jobs.ConnectorAgencyPrefix + s.Slug, Label: s.Label})
	}
	return append(out,
		connectorRow{Name: jobs.ConnectorClassifyNews, Label: "News classifier"},
		connectorRow{Name: jobs.ConnectorMail, Label: "Alerts mailbox"})
}

func (p *ingestionPage) show(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status, err := p.sched.Status(ctx)
	if err != nil {
		p.fail(w, r, "job status", err)
		return
	}
	paused, err := p.st.IngestionPaused(ctx)
	if err != nil {
		p.fail(w, r, "pause state", err)
		return
	}
	states, err := p.st.ConnectorStates(ctx)
	if err != nil {
		p.fail(w, r, "connector state", err)
		return
	}
	conns := Connectors()
	for i := range conns {
		conns[i].Enabled = true
		if s, ok := states[conns[i].Name]; ok {
			conns[i].Enabled = s.Enabled
		}
	}
	var bk *backup.Status
	if p.backups != nil {
		s, err := p.backups.Status()
		if err != nil {
			p.log.Warn("backup status", "err", err) // shown as no backup, which warns
		}
		bk = &s
	}
	var body bytes.Buffer
	if err := p.tmpl.Execute(&body, struct {
		Paused     bool
		Backup     *backup.Status
		StaleHours int
		Jobs       []scheduler.JobStatus
		Connectors []connectorRow
		CSRF       string
	}{paused, bk, int(backup.StaleAfter.Hours()), status, conns, p.layout.CSRFToken(r)}); err != nil {
		p.fail(w, r, "render ingestion page", err)
		return
	}
	p.layout.Render(w, r, host.Page{
		Name: "admin_ingestion", Title: "Ingestion status", Section: "admin",
		Head: template.HTML(`<link rel="stylesheet" href="` + web.StaticPrefix + `tracker.css">`),
		Body: template.HTML(body.String()),
	})
}

func (p *ingestionPage) setPause(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	paused := r.PostFormValue("paused") == "on"
	detail, msg := "resumed", "Ingestion resumed: scheduled jobs run again from the next tick."
	state, err := p.st.GetInstanceState(ctx)
	if err != nil {
		p.fail(w, r, "instance state", err)
		return
	}
	if !paused && state.PendingResume() {
		// The first resume after an import arms the handoff catch-up, the
		// same as Resume ingestion on the Handoff page: resuming plainly
		// would miss items published around the cutoff.
		if err := p.st.ResumeAfterImport(ctx, cli.Connectors()); err != nil {
			p.fail(w, r, "resume after import", err)
			return
		}
		detail = "resumed after import (catch-up armed)"
		msg = "Ingestion resumed: this is the first run after the import, so every connector catches up " +
			"from 24 hours before the source froze."
	} else if err := p.st.SetIngestionPaused(ctx, paused); err != nil {
		p.fail(w, r, "set pause", err)
		return
	}
	if paused {
		detail, msg = "paused", "Ingestion paused: no scheduled job starts until you resume."
	}
	p.auditAdmin(r, "ingestion_pause", detail)
	p.layout.SetFlash(w, "success", msg)
	http.Redirect(w, r, IngestionPath, http.StatusSeeOther)
}

func (p *ingestionPage) setConnector(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("connector")
	known := false
	for _, c := range Connectors() {
		known = known || c.Name == name
	}
	if !known {
		http.Error(w, "unknown connector", http.StatusBadRequest)
		return
	}
	enabled := r.PostFormValue("enabled") == "on"
	if err := p.st.SetConnectorEnabled(r.Context(), name, enabled); err != nil {
		p.fail(w, r, "set connector", err)
		return
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	p.auditAdmin(r, "ingestion_connector", name+" "+state)
	p.layout.SetFlash(w, "success", "Connector "+name+" "+state+".")
	http.Redirect(w, r, IngestionPath, http.StatusSeeOther)
}

// export streams a fresh export bundle (§10) of this instance as a
// download. The bundle is staged in a temp dir and served once complete, so
// a failed export is an error page, never a truncated file.
func (p *ingestionPage) export(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inst, err := p.st.EnsureInstance(ctx)
	if err != nil {
		p.fail(w, r, "export: instance", err)
		return
	}
	dir, err := os.MkdirTemp("", "corruption-tracker-download-")
	if err != nil {
		p.fail(w, r, "export: temp dir", err)
		return
	}
	defer os.RemoveAll(dir)
	ver := version.Version
	if version.Commit != "" {
		ver += "+" + version.Commit
	}
	// A frozen instance's export is the handoff bundle (§11.4 step 2).
	state, err := p.st.GetInstanceState(ctx)
	if err != nil {
		p.fail(w, r, "export: instance state", err)
		return
	}
	var ho *bundle.Handoff
	if state.Mode == store.ModeFrozen && state.FrozenAt != nil {
		ho = &bundle.Handoff{FreezeID: state.FreezeID, FrozenAt: state.FrozenAt.UTC().Format(time.RFC3339),
			FrozenBy: state.FrozenBy}
	}
	path, m, err := bundle.Export(ctx, p.st.Pool, dir, bundle.ExportOptions{
		Source:     bundle.Source{InstanceID: inst.ID, Kind: bundle.KindStandalone},
		AppVersion: ver,
		Handoff:    ho,
		Log:        p.log,
	})
	if err != nil {
		p.fail(w, r, "export", err)
		return
	}
	if ho != nil {
		if err := p.st.RecordHandoffExport(ctx, m.ExportID); err != nil {
			p.fail(w, r, "export: record handoff export", err)
			return
		}
	}
	f, err := os.Open(path)
	if err != nil {
		p.fail(w, r, "export: open bundle", err)
		return
	}
	defer f.Close()
	p.auditAdmin(r, "export_bundle", "export_id "+m.ExportID)
	name := filepath.Base(path)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, time.Time{}, f)
}

// auditAdmin records an operational change in the access log; failing to
// audit is logged, not fatal (the change itself already happened).
func (p *ingestionPage) auditAdmin(r *http.Request, surface, detail string) {
	u, _ := localauth.CurrentUser(r)
	if err := p.audit.LogAccess(r.Context(), u.Principal(), "admin", surface, detail); err != nil {
		p.log.Error("audit ingestion change", "surface", surface, "err", err)
	}
}

func (p *ingestionPage) fail(w http.ResponseWriter, _ *http.Request, what string, err error) {
	p.log.Error("ingestion page: "+what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
