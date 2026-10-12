// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server

import (
	"bytes"
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/web"
)

// SettingsPath is the admin-only Settings page.
const SettingsPath = "/admin/settings"

// testTimeout bounds a Test-connection call.
const testTimeout = 45 * time.Second

type settingsPage struct {
	svc    *settings.Service
	st     *store.Store // handoff state, for the frozen note
	layout host.Layout
	audit  host.AccessAuditor
	log    *slog.Logger
	tmpl   *template.Template
}

func newSettingsPage(svc *settings.Service, st *store.Store, layout host.Layout, audit host.AccessAuditor, log *slog.Logger) (*settingsPage, error) {
	t, err := template.ParseFS(templateFS, "templates/settings.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &settingsPage{svc: svc, st: st, layout: layout, audit: audit, log: log, tmpl: t}, nil
}

func (p *settingsPage) routes(r chi.Router) {
	r.Use(requireAdmin)
	r.Get("/", p.show)
	r.Post("/", p.save)
	r.Post("/test/llm", p.testLLM)
	r.Post("/test/imap", p.testIMAP)
}

type settingsGroup struct {
	Name   settings.Group
	Fields []settings.Field
}

func (p *settingsPage) show(w http.ResponseWriter, r *http.Request) {
	var groups []settingsGroup
	for _, f := range p.svc.Fields() {
		if len(groups) == 0 || groups[len(groups)-1].Name != f.Group {
			groups = append(groups, settingsGroup{Name: f.Group})
		}
		groups[len(groups)-1].Fields = append(groups[len(groups)-1].Fields, f)
	}
	// Settings stay editable during a handoff freeze (§11.3), but the
	// bundle was or will be taken from the frozen state: say so.
	frozen := false
	if state, err := p.st.GetInstanceState(r.Context()); err != nil {
		p.log.Warn("settings page: instance state", "err", err)
	} else {
		frozen = !state.Writable()
	}
	var body bytes.Buffer
	if err := p.tmpl.Execute(&body, struct {
		Groups        []settingsGroup
		CSRF          string
		LLMConfigured bool
		Frozen        bool
	}{groups, p.layout.CSRFToken(r), p.svc.LLMConfigured(), frozen}); err != nil {
		p.log.Error("render settings page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p.layout.Render(w, r, host.Page{
		Name: "admin_settings", Title: "Settings", Section: "admin",
		Head: template.HTML(`<link rel="stylesheet" href="` + web.StaticPrefix + `tracker.css">`),
		Body: template.HTML(body.String()),
	})
}

func (p *settingsPage) save(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	clear := map[string]bool{}
	for _, n := range r.PostForm["clear"] {
		clear[n] = true
	}
	var changes []settings.Change
	var names []string
	for _, f := range p.svc.Fields() {
		if f.Locked {
			continue
		}
		_, posted := r.PostForm[f.Name]
		if !posted && !clear[f.Name] {
			continue
		}
		v := r.PostFormValue(f.Name)
		if !f.Secret && v == f.Value {
			continue
		}
		if f.Secret && v == "" && !clear[f.Name] {
			continue
		}
		changes = append(changes, settings.Change{Name: f.Name, Value: v, Clear: clear[f.Name]})
		names = append(names, f.Name)
	}
	if len(changes) == 0 {
		p.layout.SetFlash(w, "info", "No changes.")
		http.Redirect(w, r, SettingsPath, http.StatusSeeOther)
		return
	}
	// Turning naming on is never a side effect: it needs its own
	// confirmation, and is audited on its own.
	naming := ""
	for _, c := range changes {
		if c.Name == settings.NamingEnabled {
			naming = c.Value
		}
	}
	if naming == "on" && r.PostFormValue("naming_confirm") != "yes" {
		p.layout.SetFlash(w, "error", "Not saved: to show participants' names, tick the confirmation under Naming.")
		http.Redirect(w, r, SettingsPath, http.StatusSeeOther)
		return
	}
	u, _ := localauth.CurrentUser(r)
	actor := u.ActorID
	if err := p.svc.Update(r.Context(), &actor, changes); err != nil {
		// Validation messages are written for operators; nothing secret
		// is in them.
		p.layout.SetFlash(w, "error", "Not saved: "+err.Error())
		http.Redirect(w, r, SettingsPath, http.StatusSeeOther)
		return
	}
	// The audit names which settings changed, never their values.
	if err := p.audit.LogAccess(r.Context(), u.Principal(), "admin", "settings_update", joinNames(names)); err != nil {
		p.log.Error("audit settings change", "err", err)
	}
	if naming != "" {
		surface := "naming_disabled"
		if naming == "on" {
			surface = "naming_enabled"
		}
		if err := p.audit.LogAccess(r.Context(), u.Principal(), "admin", surface, "instance naming switch"); err != nil {
			p.log.Error("audit naming switch", "err", err)
		}
		p.log.Warn("instance naming switch changed", "by", u.Username, "naming", naming)
	}
	p.log.Info("settings updated", "by", u.Username, "settings", names)
	p.layout.SetFlash(w, "success", "Settings saved.")
	http.Redirect(w, r, SettingsPath, http.StatusSeeOther)
}

func (p *settingsPage) testLLM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	msg, err := p.svc.TestLLM(ctx)
	p.flashResult(w, r, "settings_test_llm", msg, err)
}

func (p *settingsPage) testIMAP(w http.ResponseWriter, r *http.Request) {
	msg, err := p.svc.TestIMAP()
	p.flashResult(w, r, "settings_test_imap", msg, err)
}

func (p *settingsPage) flashResult(w http.ResponseWriter, r *http.Request, surface, msg string, err error) {
	u, _ := localauth.CurrentUser(r)
	detail := "ok"
	if err != nil {
		detail = "failed"
		p.layout.SetFlash(w, "error", err.Error())
	} else {
		p.layout.SetFlash(w, "success", msg)
	}
	if aerr := p.audit.LogAccess(r.Context(), u.Principal(), "admin", surface, detail); aerr != nil {
		p.log.Error("audit settings test", "err", aerr)
	}
	http.Redirect(w, r, SettingsPath, http.StatusSeeOther)
}

func joinNames(ns []string) string {
	var b bytes.Buffer
	for i, n := range ns {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(n)
	}
	return b.String()
}
