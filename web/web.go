// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package web is the tracker's back office (/admin/fraud/*): the claims
// review queue, case register and editing, duplicate merge, appeal-check
// worklist and manual submission, plus the tracker's static assets. It
// works directly on the store; the org-facing reader surfaces live in
// web/reader on top of the read model.
//
// The host plugs in through the ports in package host: Authorizer (who is
// asking, and whether they may operate), AccessAuditor (Privacy-Act log of
// person-level views), Layout (site chrome, CSRF token, flash) and an
// optional AdminExtension for host-owned panels and routes.
package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/web/internal/render"
)

//go:embed templates/*.html.tmpl
var templateFS embed.FS

// StaticPrefix is where the host mounts Handlers.Static.
const StaticPrefix = render.StaticPrefix

// Deps wires the back office.
type Deps struct {
	Store    *store.Store
	Auth     host.Authorizer
	Audit    host.AccessAuditor // may be nil
	Layout   host.Layout
	AdminExt host.AdminExtension // may be nil
	Log      *slog.Logger
}

// Handlers are the back office and static handlers.
type Handlers struct {
	// Admin serves /admin/fraud/*; mount it at /admin/fraud.
	Admin http.Handler
	// Static serves the tracker stylesheet; mount it at StaticPrefix.
	Static http.Handler

	o     Deps
	st    *store.Store
	log   *slog.Logger
	rd    render.Renderer
	pages map[string]*template.Template
}

var adminPages = []string{
	"dashboard", "claims", "claim_detail", "duplicates", "cases", "case_detail",
	"case_edit", "case_history", "appeals", "submit", "frozen",
}

// New parses the back-office templates and builds the router.
func New(d Deps) (*Handlers, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	h := &Handlers{
		o: d, st: d.Store, log: d.Log,
		rd:     render.Renderer{Layout: d.Layout, Log: d.Log, Section: "admin"},
		pages:  map[string]*template.Template{},
		Static: render.StaticHandler(),
	}
	for _, name := range adminPages {
		t, err := render.Parse(templateFS, "templates/"+name+".html.tmpl", "templates/_partials.html.tmpl")
		if err != nil {
			return nil, err
		}
		h.pages[name] = t
	}
	h.Admin = h.router()
	return h, nil
}

func (h *Handlers) router() http.Handler {
	fr := chi.NewRouter()
	fr.Use(h.requireOperator, h.blockWritesWhileFrozen)
	fr.Get("/", h.adminFraudDashboard)
	if h.o.AdminExt != nil {
		h.o.AdminExt.Routes(fr)
	}
	fr.Get("/claims", h.adminFraudClaims)
	fr.Post("/claims/batch", h.adminFraudClaimsBatch)
	fr.Get("/claims/{id}", h.adminFraudClaimDetail)
	fr.Post("/claims/{id}/transition", h.adminFraudClaimTransition)
	fr.Post("/claims/{id}/release-vote", h.adminFraudClaimReleaseVote)
	fr.Post("/claims/{id}/match/{cand}", h.adminFraudClaimMatchDecide)
	fr.Post("/claims/{id}/no-match", h.adminFraudClaimNoMatch)
	fr.Post("/claims/{id}/promote", h.adminFraudClaimPromote)
	fr.Get("/duplicates", h.adminFraudDuplicates)
	fr.Post("/duplicates/merge", h.adminFraudDuplicatesMerge)
	fr.Get("/cases", h.adminFraudCases)
	fr.Get("/cases/{id}", h.adminFraudCaseDetail)
	fr.Get("/cases/{id}/edit", h.adminFraudCaseEdit)
	fr.Post("/cases/{id}/edit", h.adminFraudCaseUpdate)
	fr.Post("/cases/{id}/participants/{pid}", h.adminFraudParticipantUpdate)
	fr.Post("/cases/{id}/status", h.adminFraudCaseStatusUpdate)
	fr.Post("/cases/{id}/status/revert", h.adminFraudCaseStatusRevert)
	fr.Get("/cases/{id}/history", h.adminFraudCaseHistory)
	fr.Post("/cases/{id}/verify", h.adminFraudCaseVerify)
	fr.Post("/cases/{id}/naming", h.adminFraudCaseNaming)
	fr.Get("/appeals", h.adminFraudAppeals)
	fr.Post("/appeals/checked", h.adminFraudAppealChecked)
	fr.Get("/submit", h.adminFraudSubmitForm)
	fr.Post("/submit", h.adminFraudSubmitPost)
	return fr
}

// requireOperator gates the back office: anonymous requests 303 to the
// host login, signed-in requesters without Operate get a 403.
func (h *Handlers) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.o.Auth.Principal(r); !ok {
			http.Redirect(w, r, h.o.Auth.LoginURL(r), http.StatusSeeOther)
			return
		}
		acc, err := h.o.Auth.Access(r.Context(), r)
		if err != nil {
			h.log.Error("fraud back office access check", "err", err)
		}
		if err != nil || !(acc.Operate || acc.Admin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// page renders one back-office template through the host chrome.
func (h *Handlers) page(w http.ResponseWriter, r *http.Request, name string, data any) {
	h.rd.Page(w, r, "admin_"+name, h.pages[name], 0, render.Root{Data: data})
}

// label is the requester's audit label (the email in alitheia).
func (h *Handlers) label(r *http.Request) string {
	if p, ok := h.o.Auth.Principal(r); ok {
		return p.Label
	}
	return "admin"
}

// actor resolves the requester to their tracker actor id (created on first
// action) plus the audit label. Everything the tracker records about who
// acted — reviews, verification, match decisions, appeal checks, case
// revisions — uses this id. A nil id means no signed-in principal (or the
// directory write failed, which is logged).
func (h *Handlers) actor(r *http.Request) (*int64, string) {
	p, ok := h.o.Auth.Principal(r)
	if !ok {
		return nil, "admin"
	}
	id, err := h.st.EnsureActor(r.Context(), store.Actor{
		ExternalRef: p.ExternalRef, Label: p.Label, Email: p.Email, Kind: "user",
	})
	if err != nil {
		h.log.Error("fraud actor", "ref", p.ExternalRef, "err", err)
		return nil, p.Label
	}
	return &id, p.Label
}

// auditAdmin logs back-office access to person-level data. Back-office rows
// carry no org scope: operators act for the tracker, not for their team.
func (h *Handlers) auditAdmin(r *http.Request, surface, detail string) {
	if h.o.Audit == nil {
		return
	}
	p, ok := h.o.Auth.Principal(r)
	if !ok {
		return
	}
	p.OrgRef = ""
	if err := h.o.Audit.LogAccess(r.Context(), p, host.EntitlementView, surface, detail); err != nil {
		h.log.Warn("fraud admin access log", "err", err)
	}
}

func (h *Handlers) flashRedirect(w http.ResponseWriter, r *http.Request, level, msg, target string) {
	h.o.Layout.SetFlash(w, level, msg)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func parseInt64Param(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// AdminNavItems is the back office's navigation, exported so a host drawer
// lists exactly the tracker's own sections.
func AdminNavItems() []NavItem {
	return []NavItem{
		{Href: "/admin/fraud", Label: "Dashboard", Exact: true},
		{Href: "/admin/fraud/claims", Label: "Claims queue"},
		{Href: "/admin/fraud/cases", Label: "Cases"},
		{Href: "/admin/fraud/appeals", Label: "Appeal checks"},
		{Href: "/tracker/corruption", Label: "Weekly report"},
	}
}

// NavItem is one back-office nav entry.
type NavItem struct {
	Href, Label string
	Exact       bool // active only on an exact path match
}
