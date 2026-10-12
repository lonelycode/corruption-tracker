// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudrecode

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/llm"
)

// scriptedCompleter returns a canned classification, keyed on the dossier
// text so different cases in one run get different answers.
type scriptedCompleter struct {
	byMarker map[string]string // substring of dossier -> response JSON
}

func (c scriptedCompleter) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	text := req.User
	for marker, resp := range c.byMarker {
		if strings.Contains(text, marker) {
			return llm.Response{Content: json.RawMessage(resp)}, nil
		}
	}
	return llm.Response{Content: json.RawMessage(`{"typology_slug":"","typology_confidence":0,"prosecuting_agency_slug":"","prosecuting_agency_evidence":""}`)}, nil
}

func mkCase(t *testing.T, d *store.Store, in store.FraudCaseInput) domain.FraudCase {
	t.Helper()
	if len(in.Sources) == 0 {
		in.Sources = []domain.FraudCaseSource{{
			SourceType: domain.FraudSourceManual, ContentHash: "seed-" + in.Case.CaseName,
		}}
	}
	c, err := d.CreateFraudCase(context.Background(), in)
	if err != nil {
		t.Fatalf("create case %q: %v", in.Case.CaseName, err)
	}
	return c
}

func TestRecodeRun(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	undatedSentencing := []domain.FraudCaseEvent{{EventType: domain.EventSentencing, OutcomeDetail: "sentenced"}}

	// Case A: sentence-shaped raw typology, no agency anywhere in text →
	// expect LLM typology fill, alias NOT promoted (too long), agency filled
	// by the tax→IRD prior as 'inferred', period + event date recovered.
	caseA := mkCase(t, d, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName:    "marker-alpha",
			TypologyRaw: "filed false gst and paye returns over several years",
			Summary:     "marker-alpha",
		},
		Events: undatedSentencing,
	})

	// Case B: short raw term mapped with high confidence → alias promoted;
	// agency stated with evidence → 'stated'; charge texts recovered for the
	// exact-name participant only, never overwriting existing text.
	caseB := mkCase(t, d, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName:    "marker-beta",
			TypologyRaw: "trade me fraud",
			Summary:     "marker-beta",
		},
		Participants: []store.FraudParticipantInput{
			{PersonName: "Beta Defendant", Participant: domain.FraudCaseParticipant{Role: "defendant"}},
			{PersonName: "Beta CoAccused", Participant: domain.FraudCaseParticipant{
				Role: "co_accused", IndividualCharges: "existing charge text"}},
		},
		Events: undatedSentencing,
	})

	// Case C: low typology confidence → left uncoded.
	caseC := mkCase(t, d, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName:    "marker-gamma",
			TypologyRaw: "fraud (nature not specified)",
			Summary:     "marker-gamma",
		},
		Events: undatedSentencing,
	})

	deps := &Deps{
		DB: d, Log: slog.Default(), NoFetch: true, Delay: time.Millisecond,
		LLM: scriptedCompleter{byMarker: map[string]string{
			"marker-alpha": `{"typology_slug":"tax-fraud","typology_confidence":0.95,
				"prosecuting_agency_slug":"","prosecuting_agency_evidence":"",
				"region_slug":"auckland","sector_slug":"retail-hospitality",
				"offending_period_start":"2019-04","offending_period_end":"2022-03",
				"conviction_date":"2023-08-15"}`,
			"marker-beta": `{"typology_slug":"general-dishonesty","typology_confidence":0.9,
				"prosecuting_agency_slug":"nz-police",
				"prosecuting_agency_evidence":"Police prosecuted the man over the Trade Me listings.",
				"participant_charges":[
					{"name":"beta defendant","charges":"12 charges of obtaining by deception"},
					{"name":"Beta CoAccused","charges":"must not overwrite"},
					{"name":"Someone Unrelated","charges":"never lands anywhere"}]}`,
			"marker-gamma": `{"typology_slug":"general-dishonesty","typology_confidence":0.4,
				"prosecuting_agency_slug":"","prosecuting_agency_evidence":""}`,
		}},
	}

	st, err := deps.Run(ctx, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Targets < 3 {
		t.Fatalf("targets = %d, want >= 3", st.Targets)
	}

	// Case A assertions.
	a, _, eventsA, _, err := d.GetFraudCase(ctx, caseA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.TypologyID == nil {
		t.Error("case A: typology not coded")
	}
	if a.ProsecutingAgencyID == nil {
		t.Error("case A: agency not inferred from tax-fraud prior")
	}
	if a.RegionID == nil {
		t.Error("case A: region not coded")
	}
	if a.SectorID == nil {
		t.Error("case A: sector not coded")
	}
	if a.OffendingPeriodStart == nil || a.OffendingPeriodPrecision == nil || *a.OffendingPeriodPrecision != "month" {
		t.Errorf("case A: offending period start=%v precision=%v", a.OffendingPeriodStart, a.OffendingPeriodPrecision)
	}
	var agencyBasis string
	if err := d.Pool.QueryRow(ctx, `SELECT agency_basis FROM fraud_cases WHERE id=$1`, caseA.ID).Scan(&agencyBasis); err != nil {
		t.Fatal(err)
	}
	if agencyBasis != "inferred" {
		t.Errorf("case A: agency_basis = %q, want inferred", agencyBasis)
	}
	if len(eventsA) != 1 || eventsA[0].EventDate == nil {
		t.Errorf("case A: sentencing event not dated: %+v", eventsA)
	}
	// Long raw text must NOT become an alias.
	var nAlias int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_typologies WHERE 'filed false gst and paye returns over several years' = ANY(aliases)`).Scan(&nAlias); err != nil {
		t.Fatal(err)
	}
	if nAlias != 0 {
		t.Error("case A: long raw text was promoted to an alias")
	}

	// Case B assertions: stated agency + alias self-heal.
	b, _, _, _, err := d.GetFraudCase(ctx, caseB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.TypologyID == nil || b.ProsecutingAgencyID == nil {
		t.Fatalf("case B: typology/agency not coded: %+v", b)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT agency_basis FROM fraud_cases WHERE id=$1`, caseB.ID).Scan(&agencyBasis); err != nil {
		t.Fatal(err)
	}
	if agencyBasis != "stated" {
		t.Errorf("case B: agency_basis = %q, want stated", agencyBasis)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_typologies WHERE 'trade me fraud' = ANY(aliases)`).Scan(&nAlias); err != nil {
		t.Fatal(err)
	}
	if nAlias != 1 {
		t.Errorf("case B: alias not promoted (count=%d)", nAlias)
	}
	_, partsB, _, _, err := d.GetFraudCase(ctx, caseB.ID)
	if err != nil {
		t.Fatal(err)
	}
	chargesByName := map[string]string{}
	for _, p := range partsB {
		chargesByName[p.PersonName] = p.IndividualCharges
	}
	if got := chargesByName["Beta Defendant"]; got != "12 charges of obtaining by deception" {
		t.Errorf("case B: charge text not recovered (case-insensitive match): %q", got)
	}
	if got := chargesByName["Beta CoAccused"]; got != "existing charge text" {
		t.Errorf("case B: existing charge text overwritten: %q", got)
	}

	// Case C assertions: low confidence leaves typology NULL, stamps attempt.
	c, _, _, _, err := d.GetFraudCase(ctx, caseC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.TypologyID != nil {
		t.Error("case C: low-confidence typology was applied")
	}
	if st.TypologyLowConf == 0 {
		t.Error("low-confidence counter not incremented")
	}
	var recoded bool
	if err := d.Pool.QueryRow(ctx, `SELECT recoded_at IS NOT NULL FROM fraud_cases WHERE id=$1`, caseC.ID).Scan(&recoded); err != nil {
		t.Fatal(err)
	}
	if !recoded {
		t.Error("case C: recoded_at not stamped on uncodable case")
	}

	// Idempotency: a second run finds no fresh targets.
	st2, err := deps.Run(ctx, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Targets; got != 0 {
		t.Errorf("second run targets = %d, want 0", got)
	}
}

// Recode must never overwrite values that are already set.
func TestApplyFraudRecodeFillsNullsOnly(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	terms, err := d.LoadFraudTaxonomy(ctx, "typology")
	if err != nil || len(terms) < 2 {
		t.Fatalf("taxonomy: %v (%d terms)", err, len(terms))
	}
	set, other := terms[0].ID, terms[1].ID
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	prec := "day"
	c := mkCase(t, d, store.FraudCaseInput{Case: domain.FraudCase{
		CaseName: "already-coded", TypologyID: &set,
		OffendingPeriodStart: &start, OffendingPeriodPrecision: &prec,
	}})

	newStart := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	monthPrec := "month"
	out, err := d.ApplyFraudRecode(ctx, c.ID, store.FraudRecodePatch{
		TypologyID:     &other,
		OffendingStart: &newStart, OffendingPrecision: &monthPrec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.TypologyID == nil || *out.TypologyID != set {
		t.Errorf("typology overwritten: %v", out.TypologyID)
	}
	if out.OffendingPeriodStart == nil || !out.OffendingPeriodStart.Equal(start) {
		t.Errorf("offending period overwritten: %v", out.OffendingPeriodStart)
	}
}

func TestFraudCaseLagsView(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	day := "day"
	year := "year"
	offEnd := time.Date(2020, 3, 31, 0, 0, 0, 0, time.UTC)
	charge := time.Date(2021, 3, 31, 0, 0, 0, 0, time.UTC)
	conviction := time.Date(2022, 3, 31, 0, 0, 0, 0, time.UTC)
	start := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)

	c := mkCase(t, d, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName:             "lag-case",
			OffendingPeriodStart: &start, OffendingPeriodEnd: &offEnd, OffendingPeriodPrecision: &day,
		},
		Events: []domain.FraudCaseEvent{
			{EventType: domain.EventChargesLaid, EventDate: &charge, EventDatePrecision: &day},
			{EventType: domain.EventSentencing, EventDate: &conviction, EventDatePrecision: &day},
		},
	})

	lags, err := d.GetFraudCaseLags(ctx, []int64{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	l, ok := lags[c.ID]
	if !ok {
		t.Fatal("no lag row")
	}
	if l.OffenceToChargeDays == nil || *l.OffenceToChargeDays != 365 {
		t.Errorf("offence_to_charge_days = %v, want 365", l.OffenceToChargeDays)
	}
	if l.OffenceToConvictionDays == nil || *l.OffenceToConvictionDays != 730 {
		t.Errorf("offence_to_conviction_days = %v, want 730", l.OffenceToConvictionDays)
	}
	if l.ChargeToConvictionDays == nil || *l.ChargeToConvictionDays != 365 {
		t.Errorf("charge_to_conviction_days = %v, want 365", l.ChargeToConvictionDays)
	}
	if l.Approximate {
		t.Error("day-precision lag flagged approximate")
	}

	// Year precision: offence end shifts to mid-year and approximate=true.
	yearEnd := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c2 := mkCase(t, d, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName:             "lag-case-approx",
			OffendingPeriodStart: &yearEnd, OffendingPeriodPrecision: &year,
		},
		Events: []domain.FraudCaseEvent{
			{EventType: domain.EventSentencing, EventDate: &conviction, EventDatePrecision: &day},
		},
	})
	lags2, err := d.GetFraudCaseLags(ctx, []int64{c2.ID})
	if err != nil {
		t.Fatal(err)
	}
	l2 := lags2[c2.ID]
	if !l2.Approximate {
		t.Error("year-precision lag not flagged approximate")
	}
	if l2.OffenceEndEst == nil || l2.OffenceEndEst.Month() != time.July {
		t.Errorf("year-precision offence end not shifted to mid-year: %v", l2.OffenceEndEst)
	}
}

func TestBuildSchemaEnums(t *testing.T) {
	raw := BuildSchema([]string{"tax-fraud", "benefit-fraud"}, []string{"ird"}, []string{"auckland"}, []string{"finance"})
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	typ := s.Properties["typology_slug"].Enum
	if len(typ) != 3 || typ[0] != "" {
		t.Errorf("typology enum = %v, want [\"\" benefit-fraud tax-fraud]", typ)
	}
	if got := s.Properties["prosecuting_agency_slug"].Enum; len(got) != 2 || got[1] != "ird" {
		t.Errorf("agency enum = %v", got)
	}
}
