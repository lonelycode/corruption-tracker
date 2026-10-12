// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudtracker

import (
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
)

func d(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func ev(id int64, typ domain.FraudEventType, date *time.Time, detail string) domain.FraudCaseEvent {
	return domain.FraudCaseEvent{ID: id, EventType: typ, EventDate: date, OutcomeDetail: detail}
}

func TestDeriveCaseStatus(t *testing.T) {
	cases := []struct {
		name   string
		events []domain.FraudCaseEvent
		want   domain.FraudCaseStatus
	}{
		{"no events -> charges_laid", nil, domain.CaseChargesLaid},
		{"charges only", []domain.FraudCaseEvent{
			ev(1, domain.EventChargesLaid, d("2025-01-10"), ""),
		}, domain.CaseChargesLaid},
		{"first appearance -> before_court", []domain.FraudCaseEvent{
			ev(1, domain.EventChargesLaid, d("2025-01-10"), ""),
			ev(2, domain.EventFirstAppearance, d("2025-02-01"), ""),
		}, domain.CaseBeforeCourt},
		{"guilty verdict -> convicted", []domain.FraudCaseEvent{
			ev(1, domain.EventVerdict, d("2025-03-01"), "found guilty on all charges"),
		}, domain.CaseConvicted},
		{"acquittal verdict -> acquitted", []domain.FraudCaseEvent{
			ev(1, domain.EventVerdict, d("2025-03-01"), "acquitted on all counts"),
		}, domain.CaseAcquitted},
		{"sentencing -> convicted", []domain.FraudCaseEvent{
			ev(1, domain.EventVerdict, d("2025-03-01"), "guilty"),
			ev(2, domain.EventSentencing, d("2025-05-01"), "3 years imprisonment"),
		}, domain.CaseConvicted},
		{"appeal filed -> appeal_pending", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2025-05-01"), ""),
			ev(2, domain.EventAppealFiled, d("2025-06-01"), ""),
		}, domain.CaseAppealPending},
		{"appeal dismissed -> upheld", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2025-05-01"), ""),
			ev(2, domain.EventAppealOutcome, d("2025-09-01"), "appeal dismissed"),
		}, domain.CaseUpheld},
		{"appeal quashed -> overturned", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2025-05-01"), ""),
			ev(2, domain.EventAppealOutcome, d("2025-09-01"), "conviction quashed"),
		}, domain.CaseOverturned},
		// The lifecycle must tolerate convicted -> appeal outcome with NO
		// appeal_filed event (filings are seldom announced anywhere pollable).
		{"convicted straight to appeal outcome", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2025-05-01"), ""),
			ev(2, domain.EventAppealOutcome, d("2026-02-01"), "appeal allowed, convictions set aside"),
		}, domain.CaseOverturned},
		{"discontinued wins", []domain.FraudCaseEvent{
			ev(1, domain.EventChargesLaid, d("2025-01-01"), ""),
			ev(2, domain.EventDiscontinued, d("2025-04-01"), "charges withdrawn"),
		}, domain.CaseDiscontinued},
		// Out-of-order insertion: ordering is by event date, not insert order.
		{"date order beats insert order", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2025-05-01"), ""),
			ev(2, domain.EventChargesLaid, d("2024-01-01"), ""),
		}, domain.CaseConvicted},
		{"retrial -> before_court", []domain.FraudCaseEvent{
			ev(1, domain.EventAppealOutcome, d("2025-05-01"), "conviction quashed"),
			ev(2, domain.EventRetrial, d("2025-08-01"), ""),
		}, domain.CaseBeforeCourt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveCaseStatus(tc.events); got != tc.want {
				t.Errorf("DeriveCaseStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPreVerdict(t *testing.T) {
	if !domain.CaseChargesLaid.PreVerdict() || !domain.CaseBeforeCourt.PreVerdict() {
		t.Error("charges_laid / before_court should be pre-verdict")
	}
	for _, s := range []domain.FraudCaseStatus{
		domain.CaseConvicted, domain.CaseAcquitted, domain.CaseAppealPending,
		domain.CaseOverturned, domain.CaseUpheld, domain.CaseDiscontinued,
	} {
		if s.PreVerdict() {
			t.Errorf("%s should not be pre-verdict", s)
		}
	}
}

func TestTermResolver(t *testing.T) {
	terms := []domain.FraudTaxonomyTerm{
		{ID: 1, Slug: "tax-fraud", Label: "Tax fraud & evasion", Aliases: []string{"tax fraud", "tax evasion"}, Active: true},
		{ID: 2, Slug: "covid-subsidy-fraud", Label: "COVID & wage-subsidy fraud", Aliases: []string{"covid fraud"}, Active: true},
		{ID: 3, Slug: "retired", Label: "Old term", Aliases: []string{"old"}, Active: false},
	}
	r := NewTermResolver(terms)

	for input, wantID := range map[string]int64{
		"Tax Fraud":       1,
		"  tax evasion. ": 1, // trims, lowercases, strips trailing punctuation
		"TAX  EVASION":    1, // collapses whitespace
		"tax-fraud":       1, // slug
		"COVID fraud":     2,
	} {
		got, ok := r.Resolve(input)
		if !ok || got.ID != wantID {
			t.Errorf("Resolve(%q) = (%v,%v), want id %d", input, got.ID, ok, wantID)
		}
	}
	if _, ok := r.Resolve("old"); ok {
		t.Error("inactive terms must not resolve")
	}
	if _, ok := r.Resolve("complete novelty"); ok {
		t.Error("unknown input must not resolve (no fuzzy matching)")
	}
}

func TestStateMachineEdges(t *testing.T) {
	if _, ok := Action(domain.ReviewVerified, domain.ReviewRejected); ok {
		t.Error("verified must be terminal")
	}
	if _, ok := Action(domain.ReviewRejected, domain.ReviewInReview); ok {
		t.Error("rejected must be terminal")
	}
	if a, ok := Action(domain.ReviewPending, domain.ReviewInReview); !ok || a != "start_review" {
		t.Errorf("pending->in_review = (%q,%v)", a, ok)
	}
	if !IsQuarantineExit(domain.ReviewQuarantined, domain.ReviewInReview) {
		t.Error("quarantine->in_review is a quarantine exit")
	}
	if !NoteRequired("reject") || NoteRequired("start_review") {
		t.Error("NoteRequired wrong")
	}
}

func TestCourtIdentifiers(t *testing.T) {
	text := `Smith appeared in the Auckland District Court (CRI-2024-004-1234).
	His appeal (CA 123/2025) was dismissed: [2025] NZCA 456. See also cri 2023 092 000777.
	Unrelated: CA9999 (not an appeal number), [2025] WRONG 1.`
	got := DetectCourtIdentifiers(text)
	want := map[string]bool{
		"CRI-2024-004-1234":   true,
		"CA123/2025":          true,
		"[2025] NZCA 456":     true,
		"CRI-2023-092-000777": true,
	}
	if len(got) != len(want) {
		t.Fatalf("DetectCourtIdentifiers = %v, want %d ids", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected/unnormalised id %q", id)
		}
	}

	// Junk the extractor might invent fails normalisation.
	for _, junk := range []string{"case number 12345", "NZHC 123", "CRI-24-4-1", ""} {
		if id := NormalizeCourtID(junk); id != "" {
			t.Errorf("NormalizeCourtID(%q) = %q, want rejection", junk, id)
		}
	}
	if id := NormalizeCourtID("cri 2024 004 1234"); id != "CRI-2024-004-1234" {
		t.Errorf("normalise spaced CRI: %q", id)
	}
	if id := NormalizeCourtID("sc 45/2025"); id != "SC45/2025" {
		t.Errorf("normalise SC: %q", id)
	}
}

// An undated event recorded later (enrichment backfill of an old first
// appearance) must never outrank a dated verdict for status derivation.
func TestDeriveCaseStatusUndatedEventsSortFirst(t *testing.T) {
	events := []domain.FraudCaseEvent{
		ev(1, domain.EventChargesLaid, d("2024-10-01"), ""),
		ev(2, domain.EventVerdict, d("2026-05-14"), "Sam Fictional: acquitted"),
		ev(3, domain.EventFirstAppearance, nil, "pleaded not guilty"), // backfilled, undated
	}
	if got := DeriveCaseStatus(events); got != domain.CaseAcquitted {
		t.Errorf("status = %q, want acquitted (undated backfill must not regress it)", got)
	}
}

// An undated appeal outcome follows the conviction it challenges, whether
// or not the conviction is dated. Dating the sentencing (recode's date
// fill) used to move the dismissed appeal in front of it and flip the case
// from upheld to convicted.
func TestDeriveCaseStatusUndatedAppealFollowsConviction(t *testing.T) {
	cases := []struct {
		name   string
		events []domain.FraudCaseEvent
		want   domain.FraudCaseStatus
	}{
		{"all undated (as imported)", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, nil, "9 months home detention"),
			ev(2, domain.EventAppealOutcome, nil, "appeal dismissed — lost appeal 2019"),
		}, domain.CaseUpheld},
		{"sentencing dated later (after recode)", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2018-06-01"), "9 months home detention"),
			ev(2, domain.EventAppealOutcome, nil, "appeal dismissed — lost appeal 2019"),
		}, domain.CaseUpheld},
		{"floor beats a lower id on the same day", []domain.FraudCaseEvent{
			ev(1, domain.EventAppealOutcome, nil, "conviction quashed"),
			ev(2, domain.EventVerdict, d("2018-06-01"), "guilty"),
		}, domain.CaseOverturned},
		{"floored to the latest dated conviction event", []domain.FraudCaseEvent{
			ev(1, domain.EventVerdict, d("2018-02-01"), "guilty"),
			ev(2, domain.EventAppealOutcome, nil, "appeal dismissed"),
			ev(3, domain.EventSentencing, d("2018-06-01"), ""),
		}, domain.CaseUpheld},
		{"undated appeal filed after dated sentencing", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2025-05-01"), ""),
			ev(2, domain.EventAppealFiled, nil, ""),
		}, domain.CaseAppealPending},
		{"undated outcome after dated appeal filed", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, nil, ""),
			ev(2, domain.EventAppealFiled, d("2025-06-01"), ""),
			ev(3, domain.EventAppealOutcome, nil, "appeal dismissed"),
		}, domain.CaseUpheld},
		// A dated event the appeal doesn't have to follow still wins.
		{"later dated retrial still wins", []domain.FraudCaseEvent{
			ev(1, domain.EventVerdict, d("2018-02-01"), "guilty"),
			ev(2, domain.EventAppealOutcome, nil, "conviction quashed"),
			ev(3, domain.EventRetrial, d("2019-03-01"), ""),
		}, domain.CaseBeforeCourt},
		{"later dated discontinuance still wins", []domain.FraudCaseEvent{
			ev(1, domain.EventSentencing, d("2018-06-01"), ""),
			ev(2, domain.EventAppealOutcome, nil, "appeal dismissed"),
			ev(3, domain.EventDiscontinued, d("2020-01-01"), ""),
		}, domain.CaseDiscontinued},
		// With no dated conviction to follow, undated events keep sorting
		// first, so a dated charge still outranks the appeal.
		{"no dated conviction: undated still sorts first", []domain.FraudCaseEvent{
			ev(1, domain.EventChargesLaid, d("2017-01-01"), ""),
			ev(2, domain.EventAppealOutcome, nil, "appeal dismissed"),
		}, domain.CaseChargesLaid},
	}
	for _, tc := range cases {
		if got := DeriveCaseStatus(tc.events); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}
