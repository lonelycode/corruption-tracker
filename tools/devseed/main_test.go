// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func TestRequireLocal(t *testing.T) {
	for dsn, ok := range map[string]bool{
		"postgres://localhost:5432/db":          true,
		"postgres://127.0.0.1:5433/db":          true,
		"postgres://[::1]:5432/db":              true,
		"postgres://db.example.invalid:5432/db": false,
		"postgres://192.0.2.5/db":               false,
		"postgres://localhost.evil.example/db":  false,
		"":                                      false,
	} {
		if err := requireLocal(dsn); (err == nil) != ok {
			t.Errorf("requireLocal(%q) = %v, want ok=%v", dsn, err, ok)
		}
	}
}

func TestSeedWorksTheQueueWithASyntheticActor(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Material as an import leaves it: unverified cases and a pending
	// spreadsheet problem-row claim. Names are synthetic.
	day := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)
	var caseIDs []int64
	for _, name := range []string{"Quill", "Marrow", "Tansy"} {
		c, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case: domain.FraudCase{CaseName: "Devseed " + name, TypologyRaw: "tax fraud"},
			Participants: []store.FraudParticipantInput{{
				PersonName: "Pat " + name, Participant: domain.FraudCaseParticipant{Role: "defendant"},
			}},
			Events: []domain.FraudCaseEvent{{EventType: domain.EventSentencing, EventDate: &day}},
		})
		if err != nil {
			t.Fatal(err)
		}
		caseIDs = append(caseIDs, c.ID)
	}
	claim, _, err := st.CreateFraudClaim(ctx, domain.FraudClaim{
		Headline: "[sheet row 7] Devseed problem row", NeedsManualExtraction: true,
		SourceType: domain.FraudSourceManual, SourcePublisher: "Fraud/Bribery/Corruption spreadsheet",
		LicenceBasis: "manual-research-spreadsheet", ContentHash: "devseed-sheet-7",
	}, nil, "test")
	if err != nil {
		t.Fatal(err)
	}

	if err := seed(ctx, st, log); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var actorID int64
	var ref, kind string
	if err := st.Pool.QueryRow(ctx, `SELECT id, external_ref, kind FROM tracker_actors WHERE external_ref = 'devseed:reviewer'`).
		Scan(&actorID, &ref, &kind); err != nil {
		t.Fatalf("synthetic reviewer actor: %v", err)
	}
	if kind != "system" {
		t.Errorf("reviewer actor kind = %q, want system", kind)
	}

	for _, id := range caseIDs {
		var state string
		var by *int64
		if err := st.Pool.QueryRow(ctx, `SELECT review_state, verified_by FROM fraud_cases WHERE id=$1`, id).Scan(&state, &by); err != nil {
			t.Fatal(err)
		}
		wantVerified := id%9 != 0
		if (state == "verified") != wantVerified {
			t.Errorf("case %d review_state = %s, want verified=%v", id, state, wantVerified)
		}
		if wantVerified && (by == nil || *by != actorID) {
			t.Errorf("case %d verified_by = %v, want the devseed actor %d", id, by, actorID)
		}
	}

	var claimState string
	var reviewers []int64
	if err := st.Pool.QueryRow(ctx, `SELECT review_state FROM fraud_claims WHERE id=$1`, claim.ID).Scan(&claimState); err != nil {
		t.Fatal(err)
	}
	if claimState != "verified" {
		t.Errorf("sheet claim state = %s, want verified", claimState)
	}
	rows, err := st.Pool.Query(ctx, `SELECT reviewer FROM fraud_claim_reviews WHERE claim_id=$1 AND reviewer IS NOT NULL`, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		reviewers = append(reviewers, r)
	}
	rows.Close()
	if len(reviewers) == 0 {
		t.Error("sheet claim review trail has no attributed reviewer")
	}
	for _, r := range reviewers {
		if r != actorID {
			t.Errorf("sheet claim reviewed by actor %d, want devseed actor %d", r, actorID)
		}
	}

	var periods, revised int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(DISTINCT period), count(*) FILTER (WHERE period='2026-W23' AND version=2)
		  FROM fraud_report_snapshots`).Scan(&periods, &revised); err != nil {
		t.Fatal(err)
	}
	if periods != len(snapshotPeriods) || revised != 1 {
		t.Errorf("snapshots: %d periods, %d revised 2026-W23; want %d and 1", periods, revised, len(snapshotPeriods))
	}
}

func TestRunRefusesRemoteBeforeConnecting(t *testing.T) {
	err := run(context.Background(), "postgres://db.example.invalid:5432/db", nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to seed a non-local database") {
		t.Fatalf("run = %v", err)
	}
}
