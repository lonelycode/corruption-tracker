// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package localauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lonelycode/corruption-tracker/host"
)

const (
	sessionCookie = "tracker_session"
	csrfCookie    = "tracker_csrf"
)

// DefaultSessionTTL is how long a sign-in lasts.
const DefaultSessionTTL = 7 * 24 * time.Hour

// CookiePolicy decides the Secure flag on auth cookies.
type CookiePolicy int

const (
	// CookieSecureAuto marks cookies Secure when the request arrived over
	// HTTPS (directly, or via a proxy setting X-Forwarded-Proto: https
	// when TrustProxy is set).
	CookieSecureAuto CookiePolicy = iota
	CookieSecureAlways
	CookieSecureNever // local development over plain HTTP only
)

type ctxKey int

const (
	userKey ctxKey = iota
	csrfKey
)

// CurrentUser returns the signed-in user stored by Auth.Session.
func CurrentUser(r *http.Request) (User, bool) {
	u, ok := r.Context().Value(userKey).(User)
	return u, ok
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *Auth) secure(r *http.Request) bool {
	switch a.o.Cookies {
	case CookieSecureAlways:
		return true
	case CookieSecureNever:
		return false
	}
	if r.TLS != nil {
		return true
	}
	return a.o.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"
}

// startSession signs u in: a fresh random token in an HttpOnly cookie, its
// hash in tracker_sessions.
func (a *Auth) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u User) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	expires := a.now().Add(a.o.SessionTTL)
	// Sweep expired sessions as new ones are issued, so the table stays
	// bounded without a separate job (indexed on expires_at).
	if _, err := a.svc.Pool.Exec(ctx, `DELETE FROM tracker_sessions WHERE expires_at <= $1`, a.now()); err != nil {
		return err
	}
	if _, err := a.svc.Pool.Exec(ctx, `INSERT INTO tracker_sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)`, tokenHash(token), u.ID, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// endSession deletes the request's session and clears the cookie.
func (a *Auth) endSession(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if _, err := a.svc.Pool.Exec(ctx, `DELETE FROM tracker_sessions WHERE token_hash = $1`,
			tokenHash(c.Value)); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
	return nil
}

// lookupSession resolves a cookie token to an active user.
func (a *Auth) lookupSession(ctx context.Context, token string) (User, error) {
	u, err := scanUser(a.svc.Pool.QueryRow(ctx, `SELECT `+userCols+userFrom+`
		  JOIN tracker_sessions s ON s.user_id = u.id
		 WHERE s.token_hash = $1 AND s.expires_at > $2 AND NOT u.disabled`, tokenHash(token), a.now()))
	if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Session loads the signed-in user (if any) into the request context.
// Unknown, expired and disabled-user sessions read as anonymous.
func (a *Auth) Session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		u, err := a.lookupSession(r.Context(), c.Value)
		switch {
		case err == nil:
			r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		case !errors.Is(err, ErrNotFound):
			a.log.Error("session lookup", "err", err)
		}
		next.ServeHTTP(w, r)
	})
}

// Authorizer is the standalone host.Authorizer over local accounts.
func (a *Auth) Authorizer() host.Authorizer { return authorizer{} }

// Authorizer is the same Authorizer without an Auth instance: it reads the
// user Auth.Session stored on the request, so a Layout can be built with it
// before the Auth that renders through that Layout.
func Authorizer() host.Authorizer { return authorizer{} }

type authorizer struct{}

func (authorizer) Principal(r *http.Request) (host.Principal, bool) {
	u, ok := CurrentUser(r)
	if !ok {
		return host.Principal{}, false
	}
	return u.Principal(), true
}

func (authorizer) Access(_ context.Context, r *http.Request) (host.Access, error) {
	u, ok := CurrentUser(r)
	if !ok {
		return host.Access{}, nil
	}
	return u.Access(), nil
}

func (authorizer) LoginURL(r *http.Request) string { return LoginURL(r) }

// LoginURL is the sign-in page that returns to the current request (minus
// any CSRF token in its query, which must not end up in history or logs).
func LoginURL(r *http.Request) string {
	return "/login?next=" + RedactURL(r.URL)
}
