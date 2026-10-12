// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package localauth

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lonelycode/corruption-tracker/host"
)

//go:embed templates/*.html.tmpl
var templateFS embed.FS

// Options wires the standalone auth surface.
type Options struct {
	Service *Service
	Layout  host.Layout
	Log     *slog.Logger
	// SessionTTL defaults to DefaultSessionTTL.
	SessionTTL time.Duration
	Cookies    CookiePolicy
	// TrustProxy honours X-Forwarded-Proto / X-Forwarded-For from a reverse
	// proxy in front of the server (the compose deployment's Caddy).
	TrustProxy bool
	// LoginBurst attempts per LoginWindow, counted per client IP and per
	// username. Defaults: 5 per 15 minutes.
	LoginBurst  int
	LoginWindow time.Duration
	// Now defaults to time.Now (tests inject a clock).
	Now func() time.Time
}

// Auth is the standalone server's identity layer.
type Auth struct {
	o         Options
	svc       *Service
	log       *slog.Logger
	now       func() time.Time
	byIP      *limiter
	byUser    *limiter
	pages     map[string]*template.Template
	setupDone atomic.Bool
}

// New builds the auth layer.
func New(o Options) (*Auth, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = DefaultSessionTTL
	}
	if o.LoginBurst <= 0 {
		o.LoginBurst = 5
	}
	if o.LoginWindow <= 0 {
		o.LoginWindow = 15 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	a := &Auth{o: o, svc: o.Service, log: o.Log, now: o.Now,
		byIP:   newLimiter(o.LoginBurst, o.LoginWindow, o.Now),
		byUser: newLimiter(o.LoginBurst, o.LoginWindow, o.Now),
		pages:  map[string]*template.Template{}}
	for _, name := range []string{"login", "setup", "users"} {
		t, err := template.New(name).ParseFS(templateFS, "templates/"+name+".html.tmpl")
		if err != nil {
			return nil, err
		}
		a.pages[name] = t
	}
	return a, nil
}

// Routes registers /login, /logout, /setup and the admin Users page.
func (a *Auth) Routes(r chi.Router) {
	r.Get("/login", a.loginForm)
	r.Post("/login", a.login)
	r.Post("/logout", a.logout)
	r.Get("/setup", a.setupForm)
	r.Post("/setup", a.setup)
	r.Route("/admin/users", func(ur chi.Router) {
		ur.Use(a.requireAdmin)
		ur.Get("/", a.users)
		ur.Post("/", a.createUser)
		ur.Post("/claim", a.claimActor)
		ur.Post("/{id}/role", a.setRole)
		ur.Post("/{id}/export", a.setExport)
		ur.Post("/{id}/disable", a.setDisabled(true))
		ur.Post("/{id}/enable", a.setDisabled(false))
		ur.Post("/{id}/password", a.resetPassword)
	})
}

// SetupGate sends every request to the setup wizard until the first admin
// exists. The wizard itself, /about, /healthz and static assets stay
// reachable. Once setup is done the check is never repeated.
func (a *Auth) SetupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.setupDone.Load() || setupExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		needed, err := a.svc.SetupNeeded(r.Context())
		if err != nil {
			a.log.Error("setup check", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if needed {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		a.setupDone.Store(true)
		next.ServeHTTP(w, r)
	})
}

func setupExempt(path string) bool {
	return path == "/setup" || path == "/about" || path == "/healthz" || strings.HasPrefix(path, "/static/")
}

// requireAdmin gates the Users page: anonymous → sign in, others → 403.
func (a *Auth) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := CurrentUser(r)
		if !ok {
			http.Redirect(w, r, LoginURL(r), http.StatusSeeOther)
			return
		}
		if u.Role != RoleAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── sign in / out ───────────────────────────────────────────────────────────

func (a *Auth) loginForm(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, "login", 0, struct{ Next, Error, Username string }{safeNext(r.URL.Query().Get("next")), "", ""})
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.PostFormValue("username"))
	next := safeNext(r.PostFormValue("next"))
	ipOK := a.byIP.allow(a.clientIP(r))
	userOK := a.byUser.allow(strings.ToLower(username))
	if !ipOK || !userOK {
		a.page(w, r, "login", http.StatusTooManyRequests, struct{ Next, Error, Username string }{
			next, "Too many sign-in attempts. Wait a few minutes and try again.", username})
		return
	}
	u, err := a.svc.Authenticate(r.Context(), username, r.PostFormValue("password"))
	if errors.Is(err, ErrInvalidLogin) {
		a.page(w, r, "login", http.StatusUnauthorized, struct{ Next, Error, Username string }{
			next, "Unknown user or wrong password.", username})
		return
	}
	if err != nil {
		a.fail(w, "login", err)
		return
	}
	if err := a.startSession(r.Context(), w, r, u); err != nil {
		a.fail(w, "start session", err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if err := a.endSession(r.Context(), w, r); err != nil {
		a.fail(w, "end session", err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (a *Auth) clientIP(r *http.Request) string {
	if a.o.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ── first-run wizard ────────────────────────────────────────────────────────

// setupData is the wizard page's data. Imported is the export id after a
// successful "Start from an export" upload (the host's /setup/import).
type setupData struct {
	Error, Username, Imported string
}

func (a *Auth) setupOpen(ctx context.Context) (bool, error) {
	if a.setupDone.Load() {
		return false, nil
	}
	needed, err := a.svc.SetupNeeded(ctx)
	if err == nil && !needed {
		a.setupDone.Store(true)
	}
	return needed, err
}

func (a *Auth) setupForm(w http.ResponseWriter, r *http.Request) {
	open, err := a.setupOpen(r.Context())
	if err != nil {
		a.fail(w, "setup check", err)
		return
	}
	if !open {
		http.NotFound(w, r) // the wizard can't be re-run
		return
	}
	a.page(w, r, "setup", 0, setupData{Imported: r.URL.Query().Get("imported")})
}

func (a *Auth) setup(w http.ResponseWriter, r *http.Request) {
	open, err := a.setupOpen(r.Context())
	if err != nil {
		a.fail(w, "setup check", err)
		return
	}
	if !open {
		http.NotFound(w, r)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("password_confirm") {
		a.page(w, r, "setup", http.StatusUnprocessableEntity, setupData{Error: "The passwords don't match.", Username: username})
		return
	}
	u, err := a.svc.Setup(r.Context(), username, pw)
	if errors.Is(err, ErrSetupDone) {
		a.setupDone.Store(true)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.page(w, r, "setup", http.StatusUnprocessableEntity, setupData{Error: err.Error(), Username: username})
		return
	}
	a.setupDone.Store(true)
	if err := a.startSession(r.Context(), w, r, u); err != nil {
		a.fail(w, "start session", err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ── user management (admin only) ────────────────────────────────────────────

func (a *Auth) users(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := a.svc.Users(ctx)
	if err != nil {
		a.fail(w, "list users", err)
		return
	}
	claimable, err := a.svc.ClaimableActors(ctx)
	if err != nil {
		a.fail(w, "list claimable actors", err)
		return
	}
	warning, err := a.svc.ReviewerWarning(ctx)
	if err != nil {
		a.fail(w, "reviewer count", err)
		return
	}
	me, _ := CurrentUser(r)
	a.page(w, r, "users", 0, struct {
		Users     []User
		Claimable []ClaimableActor
		Warning   string
		Me        User
		Roles     []Role
		MinLen    int
	}{users, claimable, warning, me, []Role{RoleViewer, RoleReviewer, RoleAdmin}, MinPasswordLen})
}

func (a *Auth) createUser(w http.ResponseWriter, r *http.Request) {
	me, _ := CurrentUser(r)
	u, err := a.svc.CreateUser(r.Context(), me, newUserFromForm(r))
	a.back(w, r, err, "Created "+u.Username+".")
}

func (a *Auth) claimActor(w http.ResponseWriter, r *http.Request) {
	me, _ := CurrentUser(r)
	actorID, err := strconv.ParseInt(r.PostFormValue("actor_id"), 10, 64)
	if err != nil {
		a.back(w, r, errors.New("choose an actor to claim"), "")
		return
	}
	u, err := a.svc.ClaimActor(r.Context(), me, actorID, newUserFromForm(r))
	a.back(w, r, err, "Claimed as "+u.Username+".")
}

func newUserFromForm(r *http.Request) NewUser {
	return NewUser{
		Username:  r.PostFormValue("username"),
		Password:  r.PostFormValue("password"),
		Role:      Role(r.PostFormValue("role")),
		CanExport: r.PostFormValue("can_export") == "1",
		Note:      r.PostFormValue("note"),
	}
}

func (a *Auth) setRole(w http.ResponseWriter, r *http.Request) {
	id, ok := userIDParam(w, r)
	if !ok {
		return
	}
	me, _ := CurrentUser(r)
	err := a.svc.SetRole(r.Context(), me, id, Role(r.PostFormValue("role")), r.PostFormValue("note"))
	a.back(w, r, err, "Role updated.")
}

func (a *Auth) setExport(w http.ResponseWriter, r *http.Request) {
	id, ok := userIDParam(w, r)
	if !ok {
		return
	}
	me, _ := CurrentUser(r)
	err := a.svc.SetExport(r.Context(), me, id, r.PostFormValue("can_export") == "1", r.PostFormValue("note"))
	a.back(w, r, err, "Export permission updated.")
}

func (a *Auth) setDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userIDParam(w, r)
		if !ok {
			return
		}
		me, _ := CurrentUser(r)
		err := a.svc.SetDisabled(r.Context(), me, id, disabled, r.PostFormValue("note"))
		msg := "User re-enabled."
		if disabled {
			msg = "User disabled and signed out."
		}
		a.back(w, r, err, msg)
	}
}

func (a *Auth) resetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := userIDParam(w, r)
	if !ok {
		return
	}
	me, _ := CurrentUser(r)
	err := a.svc.ResetPassword(r.Context(), me, id, r.PostFormValue("password"))
	a.back(w, r, err, "Password reset; the user's sessions were ended.")
}

func userIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// back flashes the outcome and returns to the Users page.
func (a *Auth) back(w http.ResponseWriter, r *http.Request, err error, okMsg string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "25006" {
		// The handoff freeze guard: a new account needs a new actor, and
		// actors are part of the exported dataset.
		a.o.Layout.SetFlash(w, "error", "The tracker is read-only for a handoff, so new accounts can't be created "+
			"here (they belong to the exported data). Create the account on the new instance instead.")
	} else if err != nil {
		a.o.Layout.SetFlash(w, "error", err.Error())
	} else {
		a.o.Layout.SetFlash(w, "success", okMsg)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ── rendering ───────────────────────────────────────────────────────────────

func (a *Auth) page(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	t := a.pages[name]
	root := struct {
		Data      any
		CSRFToken string
	}{data, a.o.Layout.CSRFToken(r)}
	var title, body bytes.Buffer
	if err := t.ExecuteTemplate(&title, "title", root); err != nil {
		a.fail(w, "render "+name, err)
		return
	}
	if err := t.ExecuteTemplate(&body, "content", root); err != nil {
		a.fail(w, "render "+name, err)
		return
	}
	section := "auth"
	if name == "users" {
		section = "admin"
	}
	a.o.Layout.Render(w, r, host.Page{
		Name: "auth_" + name, Title: html.UnescapeString(strings.TrimSpace(title.String())),
		Section: section, Status: status, Body: template.HTML(body.String()),
	})
}

func (a *Auth) fail(w http.ResponseWriter, what string, err error) {
	a.log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
