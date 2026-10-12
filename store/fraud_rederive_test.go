// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"testing"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/store"
)

// seedStaleAppealCase creates a case with a dated sentencing and an undated
// dismissed appeal (derives to upheld), then writes back the status the
// pre-fix derivation stored for it: convicted.
func seedStaleAppealCase(t *testing.T, ctx context.Context, d *store.Store, name string) int64 {
	t.Helper()
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: name, TypologyRaw: "tax fraud"},
		Participants: []store.FraudParticipantInput{{
			PersonName:  "Sam " + name,
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Events: []domain.FraudCaseEvent{
			{EventType: domain.EventSentencing, EventDate: datePtr("2018-06-01"), OutcomeDetail: "9 months home detention"},
			{EventType: domain.EventAppealOutcome, OutcomeDetail: "appeal dismissed — lost appeal 2019"},
		},
	})
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if c.Status != domain.CaseUpheld {
		t.Fatalf("seed %s derived %q, want upheld", name, c.Status)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE fraud_cases SET status='convicted' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func statusOf(t *testing.T, ctx context.Context, d *store.Store, id int64) string {
	t.Helper()
	var s string
	if err := d.Pool.QueryRow(ctx, `SELECT status FROM fraud_cases WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func statusRevisions(t *testing.T, ctx context.Context, d *store.Store, id int64) []store.CaseRevisionRow {
	t.Helper()
	revs, err := d.ListCaseRevisions(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.CaseRevisionRow
	for _, rv := range revs {
		if rv.FieldName == "status" {
			out = append(out, rv)
		}
	}
	return out
}

func TestRederiveFraudCaseStatuses(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	stale := seedStaleAppealCase(t, ctx, d, "StaleAppeal")
	current := seedFraudCase(t, ctx, d, "CurrentCase").ID // convicted, and derives to convicted
	pinned := seedStaleAppealCase(t, ctx, d, "PinnedAppeal")
	if err := d.SetFraudCaseStatusOverride(ctx, pinned, "convicted", "editor@test"); err != nil {
		t.Fatal(err)
	}

	// Dry run: reports the stale case only, writes nothing.
	st, err := d.RederiveFraudCaseStatuses(ctx, false, "test note")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.StatusRederive{{CaseID: stale, From: domain.CaseConvicted, To: domain.CaseUpheld}}
	if st.Checked != 2 || st.Overridden != 1 || st.Applied != 0 || len(st.Changed) != 1 || st.Changed[0] != want[0] {
		t.Fatalf("dry run stats = %+v, want checked 2, overridden 1, changed %+v, applied 0", st, want)
	}
	if got := statusOf(t, ctx, d, stale); got != "convicted" {
		t.Fatalf("dry run wrote status %q", got)
	}
	if revs := statusRevisions(t, ctx, d, stale); len(revs) != 0 {
		t.Fatalf("dry run wrote %d status revisions", len(revs))
	}

	// Apply: the stale case flips, with one attributable status revision.
	st, err = d.RederiveFraudCaseStatuses(ctx, true, "status re-derived by test")
	if err != nil {
		t.Fatal(err)
	}
	if st.Applied != 1 || len(st.Changed) != 1 || st.Changed[0] != want[0] {
		t.Fatalf("apply stats = %+v", st)
	}
	if got := statusOf(t, ctx, d, stale); got != "upheld" {
		t.Errorf("stale case status = %q, want upheld", got)
	}
	revs := statusRevisions(t, ctx, d, stale)
	if len(revs) != 1 {
		t.Fatalf("status revisions = %d, want 1", len(revs))
	}
	rv := revs[0]
	if rv.ActorKind != "system" || rv.ChangeKind != "update" || rv.Note != "status re-derived by test" {
		t.Errorf("revision = %+v", rv)
	}
	if b, ok := rv.Before.(map[string]any); !ok || b["status"] != "convicted" {
		t.Errorf("revision before = %#v", rv.Before)
	}
	if a, ok := rv.After.(map[string]any); !ok || a["status"] != "upheld" {
		t.Errorf("revision after = %#v", rv.After)
	}

	// The current and the overridden case are untouched.
	if got := statusOf(t, ctx, d, current); got != "convicted" {
		t.Errorf("current case status = %q", got)
	}
	if got := statusOf(t, ctx, d, pinned); got != "convicted" {
		t.Errorf("overridden case status = %q, want convicted (override sticky)", got)
	}
	if n := len(statusRevisions(t, ctx, d, pinned)); n != 1 { // just the override itself
		t.Errorf("overridden case status revisions = %d, want 1", n)
	}

	// Idempotent: a second apply finds nothing.
	st, err = d.RederiveFraudCaseStatuses(ctx, true, "again")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changed) != 0 || st.Applied != 0 {
		t.Errorf("second apply stats = %+v, want no changes", st)
	}
}
