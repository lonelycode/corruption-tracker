// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package contracttest checks the presentation obligations core cannot
// enforce on a reader surface (design §5.9.3):
// every pre-verdict entry renders an ALLEGED mark, and every page carries
// the suppression / derivative-work notice. A wrapper runs it against its
// own readmodel.Surface; web/reader runs it in make check.
package contracttest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/readmodel"
)

// Notice is the fixture notice; surfaces must render it verbatim.
const Notice = "CONTRACT-NOTICE: suppression orders may be in force."

// Paths are the surface's URLs for the fixture report (period 2024-W23),
// case 1 and the explorer. Empty paths are not checked.
type Paths struct {
	Report, Case, Explorer string
}

// Run mounts s on a fresh router with a fixture Source at the operator tier
// (so every surface is reachable) and asserts each page renders ALLEGED and
// the notice.
func Run(t *testing.T, s readmodel.Surface, p Paths) {
	t.Helper()
	r := chi.NewRouter()
	s.Mount(r, fixtureSource{})
	for _, path := range []string{p.Report, p.Case, p.Explorer} {
		if path == "" {
			continue
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, rec.Code)
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(strings.ToUpper(body), "ALLEGED") {
			t.Errorf("GET %s: pre-verdict fixture rendered without an ALLEGED mark", path)
		}
		if !strings.Contains(body, Notice) {
			t.Errorf("GET %s: the suppression / derivative-work notice is missing", path)
		}
	}
}

type fixtureSource struct{}

func (fixtureSource) For(*http.Request) (readmodel.View, error) { return fixtureView{}, nil }

type fixtureView struct{}

var day = time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC)

func (fixtureView) Tier() readmodel.Tier { return readmodel.TierOperator }
func (fixtureView) SignedIn() bool       { return true }
func (fixtureView) Notice() string       { return Notice }

func (fixtureView) Periods(context.Context) ([]readmodel.PeriodRef, error) {
	return []readmodel.PeriodRef{{Period: "2024-W23", Version: 1}}, nil
}

func (fixtureView) Snapshot(_ context.Context, period string) (readmodel.Snapshot, error) {
	if period != "2024-W23" {
		return readmodel.Snapshot{}, readmodel.ErrNotFound
	}
	start := time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC)
	return readmodel.Snapshot{Period: period, Version: 1, Report: fraudreport.Payload{
		Period: period, PeriodKind: "week", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 7),
		GeneratedAt: start.AddDate(0, 0, 7),
		NewInPeriod: fraudreport.NewInPeriod{ChargesLaid: []fraudreport.CaseEntry{{
			CaseID: 1, Label: "Contract fixture", Status: "charges_laid", Alleged: true, EventDate: &day,
		}}},
		Pipeline: []fraudreport.StatusCount{{Status: "charges_laid", Alleged: true, Count: 1}},
	}}, nil
}

func (fixtureView) Case(_ context.Context, id int64) (readmodel.Case, error) {
	if id != 1 {
		return readmodel.Case{}, readmodel.ErrNotFound
	}
	return readmodel.Case{ID: 1, Status: "charges_laid", Alleged: true, ParticipantCount: 1,
		Participants: []readmodel.Participant{{Charges: "obtaining by deception"}},
		Events:       []readmodel.Event{{Type: "charges_laid", Date: &day}}}, nil
}

func (fixtureView) Explorer(_ context.Context, f readmodel.ExplorerFilter) (readmodel.ExplorerPage, error) {
	if f.Limit == 0 {
		f.Limit = 200
	}
	return readmodel.ExplorerPage{Total: 1, Filter: f, Rows: []readmodel.ExplorerRow{explorerFixture()}}, nil
}

func (fixtureView) ExplorerAll(context.Context, readmodel.ExplorerFilter) ([]readmodel.ExplorerRow, error) {
	return []readmodel.ExplorerRow{explorerFixture()}, nil
}

func (fixtureView) Facets(context.Context) (readmodel.Facets, error) { return readmodel.Facets{}, nil }

func (fixtureView) Digest(context.Context, string, string) ([]readmodel.DigestRow, error) {
	return []readmodel.DigestRow{{ID: 1, Status: "charges_laid", Alleged: true, NParts: 1}}, nil
}

func explorerFixture() readmodel.ExplorerRow {
	return readmodel.ExplorerRow{ID: 1, Status: "charges_laid", ReviewState: "verified", Alleged: true,
		Participants: []readmodel.ExplorerParticipant{{Charges: "obtaining by deception"}}}
}
