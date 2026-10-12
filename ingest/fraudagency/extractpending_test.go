// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/llm"
)

// contentKeyedCompleter answers per-input: text mentioning weathertightness
// is classified not-an-enforcement-matter, everything else extracts.
type contentKeyedCompleter struct{}

func (contentKeyedCompleter) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	text := req.User
	if strings.Contains(text, "weathertightness") {
		return llm.Response{Content: json.RawMessage(`{"is_fraud_case":false,"confidence":0.9}`)}, nil
	}
	return llm.Response{Content: json.RawMessage(`{"is_fraud_case":true,"case_stage":"sentenced",
		"persons":[{"name_as_written":"Rex Tractor Person","role":"defendant"}],
		"typology_hint":"tax evasion","event_date":"2026-05-01","total_amount_nzd":50000,
		"exact_excerpt":"dishonestly used a document to obtain $50,000","confidence":0.88}`)}, nil
}

// ExtractPending over the three manual-claim shapes: a sheet problem row
// extracts in place (still pending, now bulk-approvable, match suggested),
// a URL claim whose page the extractor rejects is auto-rejected, and a
// too-thin claim is left alone without an LLM call.
func TestExtractPending(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Body Corporate v Council</h1>
			<p>A long-running weathertightness dispute over remediation costs was decided
			in the High Court yesterday, ending years of litigation between the parties.</p></body></html>`))
	})

	// The case the sheet row's extraction should suggest an attachment to.
	target, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Rex Tractor Matter"},
		Participants: []store.FraudParticipantInput{{
			PersonName:  "Rex Tractor Person",
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Sources: []domain.FraudCaseSource{{
			SourceType: domain.FraudSourceManual, ContentHash: "rex-seed",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	mkManual := func(headline, excerpt, sourceURL, hash string, payload []byte) domain.FraudClaim {
		claim, _, err := d.CreateFraudClaim(ctx, domain.FraudClaim{
			Headline: headline, Excerpt: excerpt, SourceURL: sourceURL,
			Payload: payload, NeedsManualExtraction: true,
			SourceType: domain.FraudSourceManual, SourcePublisher: "T",
			ContentHash: hash,
		}, nil, "seed")
		if err != nil {
			t.Fatalf("seed %s: %v", headline, err)
		}
		return claim
	}
	sheet := mkManual("[sheet row 9] Rex Tractor Person",
		"Convicted of dishonestly using a document to obtain a pecuniary advantage",
		"", "ep-sheet", []byte(`{"row":9,"full_name":"Rex Tractor Person","area":"tax"}`))
	notFraud := mkManual("Judgment published: Body Corporate v Council", "", srv.URL+"/article", "ep-nf", nil)
	thin := mkManual("Fraud, Bribery", "Fraud, Bribery", "", "ep-thin", nil)

	deps := &Deps{DB: d, Log: slog.Default(), Delay: time.Millisecond, LLM: contentKeyedCompleter{}}
	st, err := deps.ExtractPending(ctx, 50)
	if err != nil {
		t.Fatalf("extract-pending: %v", err)
	}
	if st.Claims != 3 || st.Extracted != 1 || st.NotFraudCase != 1 || st.TooThin != 1 || st.Errors != 0 {
		t.Fatalf("stats: %+v", st)
	}
	if st.Suggestions < 1 {
		t.Errorf("expected a match suggestion for the sheet claim, got %d", st.Suggestions)
	}

	got, cands, _, err := d.GetFraudClaim(ctx, sheet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsManualExtraction || !strings.Contains(string(got.Payload), "case_stage") {
		t.Errorf("sheet claim should carry the extraction: manual=%v payload=%s",
			got.NeedsManualExtraction, got.Payload)
	}
	if got.ReviewState != domain.ReviewPending {
		t.Errorf("extraction must not change review state, got %s", got.ReviewState)
	}
	if got.MatchStatus != domain.MatchCandidatesPending || len(cands) == 0 || cands[0].CaseID != target.ID {
		t.Errorf("sheet claim should suggest case %d: match=%s cands=%d", target.ID, got.MatchStatus, len(cands))
	}

	gotNF, _, _, _ := d.GetFraudClaim(ctx, notFraud.ID)
	if gotNF.ReviewState != domain.ReviewRejected {
		t.Errorf("not-fraud claim should auto-reject, got %s", gotNF.ReviewState)
	}
	gotThin, _, _, _ := d.GetFraudClaim(ctx, thin.ID)
	if !gotThin.NeedsManualExtraction || gotThin.ReviewState != domain.ReviewPending {
		t.Errorf("thin claim must stay queued for a human: manual=%v state=%s",
			gotThin.NeedsManualExtraction, gotThin.ReviewState)
	}

	// Re-run: only the thin claim remains on the work list; nothing changes.
	st, err = deps.ExtractPending(ctx, 50)
	if err != nil || st.Claims != 1 || st.TooThin != 1 || st.Extracted != 0 {
		t.Fatalf("rerun: %+v err=%v", st, err)
	}
}
