// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package localauth

import (
	"context"
	"crypto/subtle"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CSRF is double-submit protection: every response carries a random token
// in a cookie, forms echo it in csrf_token (or the X-CSRF-Token header), and
// any non-GET/HEAD/OPTIONS request without a matching pair gets a 403. The
// tracker's templates read the token through host.Layout.CSRFToken, which
// the standalone layout implements with CSRFToken below.
func (a *Auth) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Multipart forms carry the token in their action URL; never send
		// URLs (and so the token) to other sites in Referer headers.
		w.Header().Set("Referrer-Policy", "same-origin")
		token := ""
		if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
			token = c.Value
		} else {
			t, err := randomToken()
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			token = t
			http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: token, Path: "/",
				HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			sent := csrfFromRequest(r)
			if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, token)))
	})
}

// csrfFromRequest finds the echoed token: the X-CSRF-Token header, else
// for multipart requests the csrf_token URL query parameter (parsing a
// multipart body here would spool a whole upload to disk before the
// handler's size cap runs), else the urlencoded form field.
func csrfFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-CSRF-Token"); t != "" {
		return t
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); strings.HasPrefix(mt, "multipart/") {
		return r.URL.Query().Get("csrf_token")
	}
	return r.PostFormValue("csrf_token")
}

// RedactURL renders u's path and query with any csrf_token value removed,
// for anything that records URLs (redirect targets, access logs).
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	q := u.Query()
	if !q.Has("csrf_token") {
		return u.RequestURI()
	}
	q.Del("csrf_token")
	out := *u
	out.RawQuery = q.Encode()
	return out.RequestURI()
}

// CSRFToken is the token forms must echo (set by Auth.CSRF).
func CSRFToken(r *http.Request) string {
	t, _ := r.Context().Value(csrfKey).(string)
	return t
}

// limiter is a fixed-window attempt counter per key (client IP, username).
type limiter struct {
	mu     sync.Mutex
	burst  int
	window time.Duration
	now    func() time.Time
	hits   map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(burst int, per time.Duration, now func() time.Time) *limiter {
	return &limiter{burst: burst, window: per, now: now, hits: map[string]*window{}}
}

// allow records an attempt for key and reports whether it is within budget.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.hits[key]
	if !ok || now.Sub(w.start) >= l.window {
		if len(l.hits) > 10000 { // bound memory under a spray of keys
			for k, v := range l.hits {
				if now.Sub(v.start) >= l.window {
					delete(l.hits, k)
				}
			}
		}
		l.hits[key] = &window{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= l.burst
}
