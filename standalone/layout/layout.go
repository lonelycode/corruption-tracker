// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package layout is the standalone server's host.Layout: a brand-neutral
// page shell (instance name, optional logo, accent colour), navigation by
// entitlement, the flash message, and on every page the AGPL network-use
// source offer — a "Source code (AGPL-3.0)" footer link to the running
// build's source and an anonymously reachable /about page
// (design §2 D1.3). It never carries alitheia
// branding; alitheia renders tracker pages through its own Layout.
package layout

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/version"
)

//go:embed templates/*.html.tmpl static
var files embed.FS

// StaticPrefix is where StaticHandler is mounted.
const StaticPrefix = "/static/standalone/"

// ChartScript is the vendored Apache ECharts bundle the report page draws
// its charts with (reader.Options.ChartScript). The version is in the path,
// so the URL never needs cache-busting. Third-party, Apache-2.0: see
// static/vendor/echarts-5.5.0/README.md.
const ChartScript = StaticPrefix + "vendor/echarts-5.5.0/echarts.min.js"

// DefaultName is the instance name until an operator sets one.
const DefaultName = "Corruption & Fraud Tracker"

// Branding is the operator's look for their instance.
type Branding struct {
	Name    string // shown in the header and <title>
	LogoURL string // optional; an absolute https URL or a site path
	Accent  string // optional "#rrggbb"
}

// BrandingSource supplies the current branding per request, so changes in
// the admin UI apply without a restart (.14 backs it with tracker_settings).
type BrandingSource interface {
	Branding(ctx context.Context) Branding
}

// StaticBranding is a fixed BrandingSource.
type StaticBranding Branding

func (b StaticBranding) Branding(context.Context) Branding { return Branding(b) }

// Options wires the layout.
type Options struct {
	// Auth decides the navigation (what the requester may reach).
	Auth host.Authorizer
	// CSRFToken returns the request's form token (localauth.CSRFToken).
	CSRFToken func(*http.Request) string
	// Branding defaults to StaticBranding{Name: DefaultName}.
	Branding BrandingSource
	// SourceURL is the offer's URL, already checked with
	// version.ResolveSourceURL at start-up. Commit/Version default to the
	// build's.
	SourceURL string
	Version   string
	Commit    string
	Log       *slog.Logger
	// Banner, when set, supplies the instance-wide notice shown above every
	// page (the handoff freeze: web.InstanceBanner); nil means none.
	Banner func(*http.Request) *host.Banner
}

// Layout is the standalone host.Layout.
type Layout struct {
	o     Options
	base  *template.Template
	about *template.Template
}

var _ host.Layout = (*Layout)(nil)

// New parses the shell.
func New(o Options) (*Layout, error) {
	if o.Branding == nil {
		o.Branding = StaticBranding{Name: DefaultName}
	}
	if o.Version == "" {
		o.Version = version.Version
	}
	if o.Commit == "" {
		o.Commit = version.Commit
	}
	if err := version.CheckSourceURL(o.SourceURL); err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	base, err := template.ParseFS(files, "templates/base.html.tmpl")
	if err != nil {
		return nil, err
	}
	about, err := template.ParseFS(files, "templates/about.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &Layout{o: o, base: base, about: about}, nil
}

var accentRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// navItem is one header link.
type navItem struct {
	Href, Label string
	Active      bool
}

// Render wraps a tracker page in the instance shell.
func (l *Layout) Render(w http.ResponseWriter, r *http.Request, pg host.Page) {
	b := l.o.Branding.Branding(r.Context())
	if strings.TrimSpace(b.Name) == "" {
		b.Name = DefaultName
	}
	if !accentRe.MatchString(b.Accent) {
		b.Accent = ""
	}
	p, signedIn := l.o.Auth.Principal(r)
	var acc host.Access
	if signedIn {
		var err error
		if acc, err = l.o.Auth.Access(r.Context(), r); err != nil {
			l.o.Log.Error("layout access check", "err", err)
		}
	}
	data := struct {
		Page       host.Page
		Brand      Branding
		AccentCSS  template.CSS
		Nav        []navItem
		SignedIn   bool
		User       string
		CSRFToken  string
		Flash      *flash
		Banner     *host.Banner
		CSSVersion string
		TrackerCSS template.HTML
		SourceLink string
		License    string
		Build      string
	}{
		Page: pg, Brand: b, Nav: l.nav(r.URL.Path, acc), SignedIn: signedIn, User: p.Label,
		CSRFToken: l.o.CSRFToken(r), Flash: takeFlash(w, r), Banner: l.banner(r), CSSVersion: cssVersion(),
		SourceLink: version.SourceLink(l.o.SourceURL, l.o.Commit), License: version.License,
		Build: BuildString(l.o.Version, l.o.Commit),
	}
	if b.Accent != "" {
		// Validated as #rrggbb above, so safe to splice into CSS.
		data.AccentCSS = template.CSS("--accent: " + b.Accent + "; --accent-2: " + b.Accent + ";")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if pg.Status != 0 {
		w.WriteHeader(pg.Status)
	}
	if err := l.base.ExecuteTemplate(w, "base", data); err != nil {
		l.o.Log.Error("layout render", "page", pg.Name, "err", err)
	}
}

func (l *Layout) nav(path string, acc host.Access) []navItem {
	var items []navItem
	add := func(href, label string, exact bool) {
		active := path == href || (!exact && strings.HasPrefix(path, href+"/"))
		items = append(items, navItem{Href: href, Label: label, Active: active})
	}
	if acc.View || acc.Operate || acc.Admin {
		add("/tracker/corruption", "Report", true)
	}
	if acc.Export || acc.Operate || acc.Admin {
		add("/tracker/corruption/explorer", "Explorer", false)
	}
	if acc.Operate || acc.Admin {
		add("/admin/fraud", "Back office", false)
	}
	if acc.Admin {
		add("/admin/users", "Users", false)
		add("/admin/ingestion", "Ingestion", false)
		add("/admin/settings", "Settings", false)
		add("/admin/backups", "Backups", false)
		add("/admin/handoff", "Handoff", false)
	}
	return items
}

func (l *Layout) banner(r *http.Request) *host.Banner {
	if l.o.Banner == nil {
		return nil
	}
	return l.o.Banner(r)
}

// CSRFToken is the request's form token.
func (l *Layout) CSRFToken(r *http.Request) string { return l.o.CSRFToken(r) }

// ── flash ───────────────────────────────────────────────────────────────────

const flashCookie = "tracker_flash"

type flash struct{ Level, Message string }

// SetFlash queues a message for the next page (survives one redirect).
func (l *Layout) SetFlash(w http.ResponseWriter, level, msg string) {
	v := base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(level, "|", "") + "|" + msg))
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: v, Path: "/", MaxAge: 60,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	level, msg, ok := strings.Cut(string(raw), "|")
	if !ok || msg == "" {
		return nil
	}
	switch level {
	case "success", "error", "info":
	default:
		level = "info"
	}
	return &flash{Level: level, Message: msg}
}

// ── /about and static ───────────────────────────────────────────────────────

// About serves /about: version, commit, licence and the source link. It is
// reachable anonymously (the AGPL offer is to every network user).
func (l *Layout) About(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Name, Version, Commit, License, LicenseName, SourceURL, SourceLink string
	}{
		Name: l.o.Branding.Branding(r.Context()).Name, Version: l.o.Version, Commit: l.o.Commit,
		License: version.License, LicenseName: version.LicenseName,
		SourceURL: l.o.SourceURL, SourceLink: version.SourceLink(l.o.SourceURL, l.o.Commit),
	}
	if strings.TrimSpace(data.Name) == "" {
		data.Name = DefaultName
	}
	var body strings.Builder
	if err := l.about.ExecuteTemplate(&body, "content", data); err != nil {
		l.o.Log.Error("about render", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	l.Render(w, r, host.Page{Name: "about", Title: "About", Section: "about", Body: template.HTML(body.String())})
}

// StaticHandler serves the shell's stylesheet; mount at StaticPrefix.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix(StaticPrefix, http.FileServer(http.FS(sub)))
}

var cssVersion = sync.OnceValue(func() string {
	b, err := files.ReadFile("static/base.css")
	if err != nil {
		return "dev"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:8]
})

// BuildString renders a short build identifier ("v1.2.0 (abc1234)").
func BuildString(ver, commit string) string {
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if commit == "" {
		return ver
	}
	return fmt.Sprintf("%s (%s)", ver, commit)
}
