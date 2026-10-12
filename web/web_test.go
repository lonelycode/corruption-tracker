// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/web/internal/render"
	"github.com/lonelycode/corruption-tracker/web/reader"
)

type stubAuth struct {
	signedIn bool
	access   host.Access
}

func (a stubAuth) Principal(*http.Request) (host.Principal, bool) {
	return host.Principal{ExternalRef: "test:1", Label: "t@x"}, a.signedIn
}
func (a stubAuth) Access(context.Context, *http.Request) (host.Access, error) { return a.access, nil }
func (stubAuth) LoginURL(r *http.Request) string                              { return "/login?next=" + r.URL.RequestURI() }

// Every back-office and reader template parses with the tracker FuncMap
// (a missing func or partial fails here, not on first request).
func TestTemplatesParse(t *testing.T) {
	if _, err := New(Deps{Auth: stubAuth{}}); err != nil {
		t.Fatalf("web.New: %v", err)
	}
	if _, err := reader.New(reader.Options{Auth: stubAuth{}}); err != nil {
		t.Fatalf("reader.New: %v", err)
	}
}

// The back-office gate: anonymous → 303 to the host login, signed-in
// non-operators → 403. Neither reaches a handler (no store needed).
func TestRequireOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth stubAuth
		code int
		loc  string
	}{
		{"anonymous", stubAuth{}, http.StatusSeeOther, "/login?next=/admin/fraud/cases"},
		{"viewer", stubAuth{signedIn: true, access: host.Access{View: true, Export: true}}, http.StatusForbidden, ""},
	} {
		h, err := New(Deps{Auth: tc.auth})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.Admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cases", nil))
		// Mounted under /admin/fraud in a host, the request URI includes the
		// prefix; here the bare router sees /cases.
		if rec.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.code)
		}
		if tc.loc != "" && !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
			t.Errorf("%s: Location %q", tc.name, rec.Header().Get("Location"))
		}
	}
}

func TestStaticServesStylesheet(t *testing.T) {
	h, err := New(Deps{Auth: stubAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.Static.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, StaticPrefix+"tracker.css", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || !strings.Contains(string(body), ".fraud-timeline") {
		t.Fatalf("tracker.css: %d %q", rec.Code, string(body[:min(len(body), 80)]))
	}
	if !strings.Contains(string(render.Head()), "tracker.css?v="+render.CSSVersion()) {
		t.Errorf("Head() = %s", render.Head())
	}
}
