// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/store"
)

func intptr(n int) *int { return &n }

func TestUpdateFraudCaseAdminPersistsAndAudits(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	c := seedFraudCase(t, ctx, d, "EditCase")

	// Pick a real typology + agency to assign.
	typologies, err := d.LoadFraudTaxonomy(ctx, "typology")
	if err != nil || len(typologies) == 0 {
		t.Fatalf("load typologies: %v", err)
	}
	agencies, err := d.LoadFraudTaxonomy(ctx, "agency")
	if err != nil || len(agencies) == 0 {
		t.Fatalf("load agencies: %v", err)
	}
	tID := typologies[0].ID
	aID := agencies[0].ID

	in := store.FraudCaseEdit{
		CaseName:             "EditCase (corrected)",
		TypologyID:           &tID,
		SecondaryTypologyIDs: []int64{typologies[1].ID},
		ProsecutingAgencyID:  &aID,
		TotalAmountNZD:       f64(2_500_000),
		AmountBasis:          strptr("proven"),
		AmountNote:           "revised after sentencing",
		CourtName:            "High Court",
		CourtIdentifiers:     []string{"CRI-2024-001", "CRI-2024-001", "CA123/2025"},
		Summary:              "Edited summary.",
		CovidRelated:         true,
	}
	if err := d.UpdateFraudCaseAdmin(ctx, c.ID, in, "editor@test"); err != nil {
		t.Fatalf("update fraud case: %v", err)
	}

	got, _, _, _, err := d.GetFraudCase(ctx, c.ID)
	if err != nil {
		t.Fatalf("get fraud case: %v", err)
	}
	if got.CaseName != "EditCase (corrected)" {
		t.Errorf("case_name = %q, want corrected", got.CaseName)
	}
	if got.TypologyID == nil || *got.TypologyID != tID {
		t.Errorf("typology_id = %v, want %d", got.TypologyID, tID)
	}
	if got.TotalAmountNZD == nil || *got.TotalAmountNZD != 2_500_000 {
		t.Errorf("amount = %v, want 2500000", got.TotalAmountNZD)
	}
	if !got.CovidRelated {
		t.Errorf("covid_related not persisted")
	}
	// parseIdentifiers-equivalent dedupe happens in the handler; the store
	// stores exactly what it's given, so the duplicate is retained here —
	// assert both distinct identifiers survived.
	if len(got.CourtIdentifiers) != 3 {
		t.Errorf("court_identifiers = %v, want 3 entries", got.CourtIdentifiers)
	}

	// Secondary typology set replaced to exactly one.
	secs, err := d.ListFraudCaseSecondaryTypologyIDs(ctx, c.ID)
	if err != nil {
		t.Fatalf("list secondary: %v", err)
	}
	if len(secs) != 1 || secs[0] != typologies[1].ID {
		t.Errorf("secondary typologies = %v, want [%d]", secs, typologies[1].ID)
	}

	// Agency basis flips to 'stated' when a human assigns the agency.
	labels, err := d.GetFraudCaseLabels(ctx, c.ID)
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	if labels.AgencyInferred {
		t.Errorf("agency basis should be 'stated' after human edit, got inferred")
	}

	// One audit revision recorded for the edit.
	revs, err := d.ListCaseRevisions(ctx, c.ID, 0)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("revisions = %d, want 1", len(revs))
	}
	if revs[0].ChangeKind != "update" || revs[0].ActorKind != "admin" {
		t.Errorf("revision = %+v, want admin/update", revs[0])
	}
}

func TestUpdateFraudParticipantAdminAudits(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	c := seedFraudCase(t, ctx, d, "PartCase")
	_, parts, _, _, err := d.GetFraudCase(ctx, c.ID)
	if err != nil || len(parts) == 0 {
		t.Fatalf("get case/participants: %v", err)
	}
	pid := parts[0].ID

	in := store.FraudParticipantEdit{
		Role:               "defendant",
		ProfessionRaw:      "accountant",
		IndividualCharges:  "12 charges of false accounting",
		Outcome:            strptr("convicted"),
		SentenceType:       strptr("imprisonment"),
		SentenceDetail:     "3 years",
		SentenceMonths:     intptr(36),
		SuppressionFlag:    false,
		CleanSlatePossible: false,
	}
	if err := d.UpdateFraudParticipantAdmin(ctx, c.ID, pid, in, "editor@test"); err != nil {
		t.Fatalf("update participant: %v", err)
	}

	_, parts2, _, _, err := d.GetFraudCase(ctx, c.ID)
	if err != nil {
		t.Fatalf("reget: %v", err)
	}
	if parts2[0].Outcome == nil || *parts2[0].Outcome != "convicted" {
		t.Errorf("outcome = %v, want convicted", parts2[0].Outcome)
	}
	if parts2[0].SentenceMonths == nil || *parts2[0].SentenceMonths != 36 {
		t.Errorf("sentence_months = %v, want 36", parts2[0].SentenceMonths)
	}

	// Logged under the parent case's stream with a participant_<id> field.
	revs, err := d.ListCaseRevisions(ctx, c.ID, 0)
	if err != nil {
		t.Fatalf("revisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("revisions = %d, want 1", len(revs))
	}
	wantField := fmt.Sprintf("participant_%d", pid)
	if revs[0].FieldName != wantField {
		t.Errorf("field = %q, want %q", revs[0].FieldName, wantField)
	}

	// Wrong case id must not touch a participant from another case.
	other := seedFraudCase(t, ctx, d, "OtherCase")
	if err := d.UpdateFraudParticipantAdmin(ctx, other.ID, pid, in, "x"); err == nil {
		t.Errorf("expected ErrRowMissing editing participant under wrong case")
	}
}

func TestFraudCaseStatusOverrideStickyThenRevert(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	// Seeded case has charges_laid + sentencing → derives to convicted.
	c := seedFraudCase(t, ctx, d, "StatusCase")
	if string(c.Status) != "convicted" {
		t.Fatalf("seeded status = %q, want convicted", c.Status)
	}

	// Override to discontinued.
	if err := d.SetFraudCaseStatusOverride(ctx, c.ID, "discontinued", "editor@test"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	got, _, _, _, _ := d.GetFraudCase(ctx, c.ID)
	if string(got.Status) != "discontinued" || !got.StatusOverridden {
		t.Fatalf("after override status=%q overridden=%v, want discontinued/true", got.Status, got.StatusOverridden)
	}

	// Adding a fresh verdict event must NOT clobber the overridden status.
	after, err := d.AddFraudCaseEvent(ctx, c.ID, domain.FraudCaseEvent{
		EventType: domain.EventVerdict, EventDate: datePtr("2025-06-01"), OutcomeDetail: "guilty",
	}, nil)
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	if string(after.Status) != "discontinued" {
		t.Errorf("status after event = %q, want discontinued (override sticky)", after.Status)
	}

	// Revert re-derives from events (now charges_laid, sentencing, verdict → convicted).
	if err := d.ClearFraudCaseStatusOverride(ctx, c.ID, "editor@test"); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	got2, _, _, _, _ := d.GetFraudCase(ctx, c.ID)
	if got2.StatusOverridden {
		t.Errorf("still overridden after revert")
	}
	if string(got2.Status) != "convicted" {
		t.Errorf("reverted status = %q, want convicted", got2.Status)
	}

	// Override set + revert = two status revisions.
	revs, err := d.ListCaseRevisions(ctx, c.ID, 0)
	if err != nil {
		t.Fatalf("revisions: %v", err)
	}
	statusRevs := 0
	for _, rv := range revs {
		if rv.FieldName == "status" {
			statusRevs++
		}
	}
	if statusRevs != 2 {
		t.Errorf("status revisions = %d, want 2", statusRevs)
	}
}
