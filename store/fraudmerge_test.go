// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"testing"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
)

// dupeCase builds a case for the merge tests: a single named participant, one
// source, one sentencing event. Same name across calls => same fraud_person,
// which is what the duplicate detector keys on.
func dupeCase(t *testing.T, ctx context.Context, d *store.Store, caseName, person string, amount float64, srcHash string) domain.FraudCase {
	t.Helper()
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName: caseName, TypologyRaw: "fraud",
			TotalAmountNZD: f64(amount), AmountBasis: strptr("proven"),
		},
		Participants: []store.FraudParticipantInput{{
			PersonName:  person,
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		// Same org+role across cases exercises the composite-key dedup path.
		Organisations: []store.FraudOrgInput{{Name: "Shared Co Ltd", Role: "vehicle"}},
		Events: []domain.FraudCaseEvent{
			{EventType: domain.EventSentencing, EventDate: datePtr("2025-03-15"), OutcomeDetail: "2 years"},
		},
		Sources: []domain.FraudCaseSource{{
			URL: "https://example.test/" + caseName, SourceType: domain.FraudSourceNews,
			Publisher: "Stuff", ContentHash: srcHash,
		}},
	})
	if err != nil {
		t.Fatalf("create dupe case %q: %v", caseName, err)
	}
	return c
}

func TestFindAndMergeDuplicateFraudCases(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	// Three records of one matter: same person, same amount, different outlets.
	a := dupeCase(t, ctx, d, "painting-tvnz", "Sam Dupe", 27000, "h-tvnz")
	b := dupeCase(t, ctx, d, "painting-stuff", "Sam Dupe", 27000, "h-stuff")
	cc := dupeCase(t, ctx, d, "painting-rnz", "Sam Dupe", 27000, "h-rnz")
	// An unrelated case (different person) must NOT be grouped.
	solo := dupeCase(t, ctx, d, "other", "Pat Solo", 5000, "h-solo")

	groups, err := d.FindDuplicateFraudCaseGroups(ctx)
	if err != nil {
		t.Fatalf("find groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1 (%+v)", len(groups), groups)
	}
	g := groups[0]
	if len(g.LoserIDs) != 2 {
		t.Fatalf("losers = %d, want 2", len(g.LoserIDs))
	}
	// Winner is the oldest (lowest id) since all three score equally.
	if g.WinnerID != a.ID {
		t.Errorf("winner = %d, want %d (oldest)", g.WinnerID, a.ID)
	}

	res, err := d.MergeFraudCases(ctx, g.WinnerID, g.LoserIDs)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.LosersMerged != 2 {
		t.Errorf("losers merged = %d, want 2", res.LosersMerged)
	}
	// Both losers shared the one person => two participant rows discarded.
	if res.CollisionsDiscarded < 2 {
		t.Errorf("collisions discarded = %d, want >=2 (shared person)", res.CollisionsDiscarded)
	}

	// Losers are gone.
	for _, id := range []int64{b.ID, cc.ID} {
		if _, _, _, _, err := d.GetFraudCase(ctx, id); err == nil {
			t.Errorf("loser %d still present after merge", id)
		}
	}

	// Winner keeps one participant, gains all three sources, all three events.
	winner, parts, events, sources, err := d.GetFraudCase(ctx, g.WinnerID)
	if err != nil {
		t.Fatalf("get winner: %v", err)
	}
	if len(parts) != 1 {
		t.Errorf("winner participants = %d, want 1 (deduped)", len(parts))
	}
	if len(sources) != 3 {
		t.Errorf("winner sources = %d, want 3", len(sources))
	}
	if len(events) != 3 {
		t.Errorf("winner events = %d, want 3", len(events))
	}
	if winner.Status != domain.CaseConvicted {
		t.Errorf("winner status = %q, want convicted (recomputed)", winner.Status)
	}

	// The solo case is untouched and never grouped.
	if _, _, _, _, err := d.GetFraudCase(ctx, solo.ID); err != nil {
		t.Errorf("solo case disturbed: %v", err)
	}
}

func TestMergeFraudCasesRejectsWinnerAsLoser(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()
	c := dupeCase(t, ctx, d, "x", "Solo Person", 1, "h-x")
	if _, err := d.MergeFraudCases(ctx, c.ID, []int64{c.ID}); err == nil {
		t.Fatal("expected error when winner is also a loser")
	}
}
