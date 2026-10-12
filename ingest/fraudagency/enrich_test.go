// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/llm"
)

// stubCompleter returns a fixed extraction payload.
type stubCompleter struct{ payload string }

func (s stubCompleter) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{Content: json.RawMessage(s.payload)}, nil
}

func TestEnrichImported(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	// Article server: robots 404 (default-allow) + the linked release page.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Sentenced for wage subsidy fraud</title></head>
			<body><h1>Sentenced for wage subsidy fraud</h1>
			<p>The offending ran from April 2020 to March 2021.</p></body></html>`))
	})

	// The imported case this URL belongs to: one sentencing event, no
	// offending period (the spreadsheet had neither).
	when, _ := time.Parse("2006-01-02", "2025-09-01")
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Enrich Target", TypologyRaw: "covid fraud"},
		Participants: []store.FraudParticipantInput{{
			PersonName:  "Enrich Person",
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Events: []domain.FraudCaseEvent{{EventType: domain.EventSentencing, EventDate: &when}},
		Sources: []domain.FraudCaseSource{{
			SourceType: domain.FraudSourceNews, Publisher: "spreadsheet link",
			URL: srv.URL + "/release", ContentHash: "enrich-seed",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	targets, err := d.ListFraudEnrichTargets(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].CaseID != c.ID {
		t.Fatalf("targets = %+v, want the seeded link", targets)
	}

	deps := &Deps{DB: d, Log: slog.Default(), Delay: time.Millisecond,
		LLM: stubCompleter{payload: `{"is_fraud_case":true,"case_stage":"sentenced",
			"persons":[{"name_as_written":"Enrich Person","role":"defendant"}],
			"offending_period":{"start":"2020-04","end":"2021-03"},
			"event_date":"2025-09-01","typology_hint":"wage subsidy fraud",
			"exact_excerpt":"The offending ran from April 2020 to March 2021.","confidence":0.92}`}}
	st, err := deps.EnrichImported(ctx, targets)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if st.ClaimsNew != 1 || st.Errors != 0 || st.RobotsBlocked != 0 {
		t.Fatalf("stats: %+v", st)
	}

	// The claim is pre-attached to its case (provenance identity) and
	// pending review.
	claims, err := d.ListFraudClaimsByCase(ctx, c.ID)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %d err=%v", len(claims), err)
	}
	cl := claims[0]
	if cl.MatchStatus != domain.MatchConfirmed || cl.CaseID == nil || *cl.CaseID != c.ID {
		t.Errorf("claim must be pre-attached: match=%s case=%v", cl.MatchStatus, cl.CaseID)
	}
	if cl.ReviewState != domain.ReviewPending {
		t.Errorf("claim must await review, got %s", cl.ReviewState)
	}

	// Re-run: idempotent.
	st, err = deps.EnrichImported(ctx, targets)
	if err != nil || st.SkippedKnown != 1 || st.ClaimsNew != 0 {
		t.Fatalf("rerun: %+v err=%v", st, err)
	}
}

func TestEnrichRespectsRobots(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /blocked/\n"))
	})

	deps := &Deps{DB: d, Log: slog.Default(), Delay: time.Millisecond,
		LLM: stubCompleter{payload: `{"is_fraud_case":false,"case_stage":"other","exact_excerpt":"x"}`}}
	st, err := deps.EnrichImported(ctx, []store.FraudEnrichTarget{{CaseID: 1, URL: srv.URL + "/blocked/article"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.RobotsBlocked != 1 || st.ClaimsNew != 0 {
		t.Fatalf("robots must block: %+v", st)
	}
}

func TestBackfillFraudCaseOffendingFillOnly(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	existing, _ := time.Parse("2006-01-02", "2019-01-01")
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case:    domain.FraudCase{CaseName: "Fill", OffendingPeriodStart: &existing},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: "fill-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	later, _ := time.Parse("2006-01-02", "2021-06-01")
	prec := "month"
	if err := d.BackfillFraudCaseOffending(ctx, c.ID, &later, &later, &prec); err != nil {
		t.Fatal(err)
	}
	got, _, _, _, err := d.GetFraudCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OffendingPeriodStart.Equal(existing) {
		t.Error("backfill must not overwrite an existing start date")
	}
	if got.OffendingPeriodEnd == nil || !got.OffendingPeriodEnd.Equal(later) {
		t.Error("backfill should fill the null end date")
	}
}
