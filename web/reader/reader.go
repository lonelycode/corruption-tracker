// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package reader is the tracker's default reader presentation: the period
// report, case timeline, case explorer and digest export, built ONLY on
// readmodel (it never touches the store). It is a readmodel.Surface:
// alitheia mounts it at /tracker/corruption behind its team-flag
// Authorizer, and the standalone server mounts it behind its login wall.
//
// Gate semantics (unchanged from alitheia): below the viewer tier every page
// 303s to the host login URL; the explorer without the export entitlement,
// and an unverified case for a non-operator, are 404s; the export is a 404
// for anyone without view+export.
package reader

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/readmodel"
	"github.com/lonelycode/corruption-tracker/web/internal/render"
)

// Base is the path the reader's routes live under. Templates link within
// it, so it is fixed rather than configurable.
const Base = "/tracker/corruption"

//go:embed templates/*.html.tmpl
var templateFS embed.FS

// Options wires the reader.
type Options struct {
	Auth   host.Authorizer // LoginURL for the 303s
	Layout host.Layout
	Log    *slog.Logger
	// ChartScript is the URL of the ECharts bundle the report page loads.
	ChartScript string
}

// Reader serves the reader surfaces.
type Reader struct {
	o     Options
	rd    render.Renderer
	pages map[string]*template.Template
}

// New parses the reader templates.
func New(o Options) (*Reader, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	rr := &Reader{o: o, rd: render.Renderer{Layout: o.Layout, Log: o.Log, Section: "tracker"},
		pages: map[string]*template.Template{}}
	for _, name := range []string{"report", "case", "explorer"} {
		t, err := render.Parse(templateFS, "templates/"+name+".html.tmpl")
		if err != nil {
			return nil, err
		}
		rr.pages[name] = t
	}
	return rr, nil
}

// Mount registers the reader routes (readmodel.Surface).
func (rr *Reader) Mount(r chi.Router, src readmodel.Source) {
	h := handlers{rr: rr, src: src}
	r.Get(Base, h.report)
	r.Get(Base+"/export", h.export)
	r.Get(Base+"/explorer", h.explorer)
	r.Get(Base+"/case/{id}", h.casePage)
}

type handlers struct {
	rr  *Reader
	src readmodel.Source
}

// view resolves the requester's read-model view; false means a response
// (500) was already written.
func (h handlers) view(w http.ResponseWriter, r *http.Request) (readmodel.View, bool) {
	v, err := h.src.For(r)
	if err != nil {
		h.rr.o.Log.Error("corruption tracker view", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	return v, true
}

func (h handlers) toLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, h.rr.o.Auth.LoginURL(r), http.StatusSeeOther)
}

func (h handlers) page(w http.ResponseWriter, r *http.Request, name string, v readmodel.View, data any) {
	h.rr.rd.Page(w, r, name, h.rr.pages[name], 0, render.Root{
		Data: data, Notice: v.Notice(), ChartScript: h.rr.o.ChartScript,
	})
}

func (h handlers) internalError(w http.ResponseWriter, msg string, err error, args ...any) {
	h.rr.o.Log.Error(msg, append(args, "err", err)...)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

var periodRe = regexp.MustCompile(`^\d{4}(-W\d{2}|-Q[1-4])?$`)

// report: GET /tracker/corruption — the period report. Reads only
// versioned snapshots, never live tables.
func (h handlers) report(w http.ResponseWriter, r *http.Request) {
	v, ok := h.view(w, r)
	if !ok {
		return
	}
	if v.Tier() < readmodel.TierViewer {
		h.toLogin(w, r)
		return
	}
	ctx := r.Context()
	periods, err := v.Periods(ctx)
	if err != nil {
		h.internalError(w, "fraud tracker periods", err)
		return
	}

	selected := r.URL.Query().Get("period")
	if selected == "" {
		selected = r.URL.Query().Get("week") // legacy links
	}
	if selected != "" && !periodRe.MatchString(selected) {
		http.NotFound(w, r)
		return
	}
	if selected == "" {
		// Default to the newest WEEKLY issue; annual/quarterly issues are
		// destinations you pick, not the landing view.
		for _, pr := range periods {
			if pr.Kind() == "week" {
				selected = pr.Period
				break
			}
		}
		if selected == "" && len(periods) > 0 {
			selected = periods[0].Period
		}
	}

	data := struct {
		Periods    []readmodel.PeriodRef
		Selected   string
		PrevPeriod string
		NextPeriod string
		Snapshot   *readmodel.Snapshot
		Report     *fraudreport.Payload
		View       *reportView
		ChartJSON  template.JS
		CanExport  bool
		DevBuild   bool
		MinCell    int
	}{Periods: periods, Selected: selected, MinCell: fraudreport.MinCellSize,
		CanExport: v.Tier() >= readmodel.TierExport}

	// Prev/next stay within the same period kind (stepping an annual issue
	// to the previous week would be nonsense).
	var sameKind []readmodel.PeriodRef
	var selectedKind string
	for _, pr := range periods {
		if pr.Period == selected {
			selectedKind = pr.Kind()
		}
	}
	for _, pr := range periods {
		if pr.Kind() == selectedKind {
			sameKind = append(sameKind, pr)
		}
	}
	for i, pr := range sameKind {
		if pr.Period == selected {
			if i+1 < len(sameKind) {
				data.PrevPeriod = sameKind[i+1].Period // list is newest-first
			}
			if i > 0 {
				data.NextPeriod = sameKind[i-1].Period
			}
		}
	}

	snap, err := v.Snapshot(ctx, selected)
	switch {
	case errors.Is(err, readmodel.ErrNotFound):
		// the page renders the empty state
	case err != nil:
		h.internalError(w, "fraud tracker snapshot", err, "period", selected)
		return
	default:
		data.Snapshot = &snap
		data.Report = &snap.Report
		data.View = buildReportView(&snap.Report)
		data.ChartJSON = chartJSON(data.View)
		data.DevBuild = snap.DevBuild
	}
	h.page(w, r, "report", v, data)
}

// casePage: GET /tracker/corruption/case/{id} — one case's full court
// lifecycle. Unverified ids 404 below the operator tier rather than
// confirming they exist.
func (h handlers) casePage(w http.ResponseWriter, r *http.Request) {
	v, ok := h.view(w, r)
	if !ok {
		return
	}
	if v.Tier() < readmodel.TierViewer {
		h.toLogin(w, r)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, err := v.Case(r.Context(), id)
	if errors.Is(err, readmodel.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "fraud case page", err, "case", id)
		return
	}
	h.page(w, r, "case", v, struct {
		readmodel.Case
		CaseID                 int64
		StatusLabel            string
		LagOffenceToConviction string
		LagChargeToConviction  string
		Timeline               []timelineEvent
	}{
		Case: c, CaseID: c.ID, StatusLabel: StatusLabel(c.Status),
		LagOffenceToConviction: lagYears(c.Lag, true),
		LagChargeToConviction:  lagYears(c.Lag, false),
		Timeline:               buildTimeline(c.Events),
	})
}
