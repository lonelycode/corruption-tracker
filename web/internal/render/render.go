// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package render is the tracker web layer's page plumbing, shared by the
// back office (web) and the reader surfaces (web/reader): the template
// FuncMap, the embedded stylesheet and its cache-busted <link>, and turning
// a page template's "title" + "content" blocks into a host.Page for the
// host Layout to wrap.
package render

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/lonelycode/corruption-tracker/host"
)

//go:embed static
var staticFS embed.FS

// StaticPrefix is where the host mounts StaticHandler.
const StaticPrefix = "/static/corruption-tracker/"

// cssVersion is the tracker.css cache-buster: the first 8 hex chars of its
// SHA-256, so any change to the stylesheet produces a fresh URL.
var cssVersion = sync.OnceValue(func() string {
	b, err := staticFS.ReadFile("static/tracker.css")
	if err != nil {
		return "dev"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:8]
})

// CSSVersion exposes the stylesheet hash (tests normalise it).
func CSSVersion() string { return cssVersion() }

// Head is the <head> markup every tracker page carries.
func Head() template.HTML {
	return template.HTML(fmt.Sprintf(`<link rel="stylesheet" href="%stracker.css?v=%s">`, StaticPrefix, cssVersion()))
}

// StaticHandler serves the embedded static files; mount it at StaticPrefix.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed layout is fixed at build time
	}
	return http.StripPrefix(StaticPrefix, http.FileServer(http.FS(sub)))
}

// Root is what every page template executes against: the handler's data
// under .Data plus the per-request values templates reach through $.
type Root struct {
	Data        any
	CSRFToken   string
	Notice      string // suppression / derivative-work disclaimer
	ChartScript string // URL of the ECharts bundle (report page)
}

// Parse builds one page's template set from fsys: the page file plus any
// shared partials, with the tracker FuncMap.
func Parse(fsys fs.FS, page string, partials ...string) (*template.Template, error) {
	files := append([]string{page}, partials...)
	t, err := template.New(page).Funcs(Funcs()).ParseFS(fsys, files...)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", page, err)
	}
	return t, nil
}

// Renderer executes a page's blocks and hands the result to the host.
type Renderer struct {
	Layout  host.Layout
	Log     *slog.Logger
	Section string // "tracker" | "admin"
}

// Page renders t's "title" and "content" blocks against root and passes
// them to the host Layout as page name. status 0 means 200.
func (rd Renderer) Page(w http.ResponseWriter, r *http.Request, name string, t *template.Template, status int, root Root) {
	if root.CSRFToken == "" {
		root.CSRFToken = rd.Layout.CSRFToken(r)
	}
	var title, body bytes.Buffer
	if err := t.ExecuteTemplate(&title, "title", root); err != nil {
		rd.fail(w, "title", err)
		return
	}
	if err := t.ExecuteTemplate(&body, "content", root); err != nil {
		rd.fail(w, "content", err)
		return
	}
	rd.Layout.Render(w, r, host.Page{
		Name: name,
		// The title block is HTML-escaped template output; the host
		// escapes Title itself, so hand it plain text.
		Title:   html.UnescapeString(strings.TrimSpace(title.String())),
		Section: rd.Section,
		Status:  status,
		Head:    Head(),
		Body:    template.HTML(body.String()),
	})
}

func (rd Renderer) fail(w http.ResponseWriter, block string, err error) {
	if rd.Log != nil {
		rd.Log.Error("tracker template execute", "block", block, "err", err)
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// Funcs is the tracker's template FuncMap. Behaviour matches the alitheia
// helpers of the same names, so moved templates render identically.
func Funcs() template.FuncMap {
	return template.FuncMap{
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"derefStr": func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		},
		"derefFloat": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"strList":   func(args ...string) []string { return args },
		"titleCase": TitleCase,
		"printDict": func(values ...any) (map[string]any, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("printDict: odd argument count")
			}
			out := make(map[string]any, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				k, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("printDict: key %d is not a string", i)
				}
				out[k] = values[i+1]
			}
			return out, nil
		},
		"add1": func(i int) int { return i + 1 },
		"toJSON": func(v any) string {
			if v == nil {
				return ""
			}
			b, err := json.Marshal(v)
			if err != nil {
				return ""
			}
			return string(b)
		},
		"humanNZD": HumanNZD,
	}
}

// HumanNZD renders an amount the way the site talks about money:
// $54.0m / $188k / $950.
func HumanNZD(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("$%.1fm", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// TitleCase upper-cases the first letter of each space- or hyphen-separated
// word, with explicit Māori macron handling.
func TitleCase(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	upNext := true
	for i, r := range runes {
		if r == ' ' || r == '-' {
			upNext = true
			continue
		}
		if upNext {
			runes[i] = toUpper(r)
			upNext = false
		}
	}
	return string(runes)
}

func toUpper(r rune) rune {
	switch r {
	case 'ā':
		return 'Ā'
	case 'ē':
		return 'Ē'
	case 'ī':
		return 'Ī'
	case 'ō':
		return 'Ō'
	case 'ū':
		return 'Ū'
	}
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}
