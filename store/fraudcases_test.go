// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

func datePtr(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func f64(v float64) *float64 { return &v }

func seedFraudCase(t *testing.T, ctx context.Context, d *store.Store, name string) domain.FraudCase {
	t.Helper()
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName:       name,
			TypologyRaw:    "tax fraud",
			TotalAmountNZD: f64(1_000_000),
			AmountBasis:    strptr("proven"),
		},
		Participants: []store.FraudParticipantInput{{
			PersonName: "Alex " + name,
			Participant: domain.FraudCaseParticipant{
				Role: "defendant", ProfessionRaw: "company director",
			},
		}},
		Organisations: []store.FraudOrgInput{{Name: name + " Holdings Ltd", Role: "vehicle"}},
		Events: []domain.FraudCaseEvent{
			{EventType: domain.EventChargesLaid, EventDate: datePtr("2024-02-01")},
			{EventType: domain.EventSentencing, EventDate: datePtr("2025-03-15"), OutcomeDetail: "2 years"},
		},
		Sources: []domain.FraudCaseSource{{
			URL: "https://example.test/" + name, SourceType: domain.FraudSourceAgencyRelease,
			Publisher: "SFO", ContentHash: "hash-" + name, Excerpt: "private excerpt",
		}},
	})
	if err != nil {
		t.Fatalf("seed fraud case: %v", err)
	}
	return c
}

func seedFraudClaim(t *testing.T, ctx context.Context, d *store.Store, headline string) domain.FraudClaim {
	t.Helper()
	claim, created, err := d.CreateFraudClaim(ctx, domain.FraudClaim{
		Headline:      headline,
		SourceType:    domain.FraudSourceAgencyRelease,
		SourceURL:     "https://example.test/r/" + headline,
		ContentHash:   "claimhash-" + headline,
		PublishedDate: datePtr("2026-05-01"),
	}, nil, "seed@test")
	if err != nil {
		t.Fatalf("seed fraud claim: %v", err)
	}
	if !created {
		t.Fatalf("seed fraud claim: expected created=true")
	}
	return claim
}

func TestFraudTaxonomySeedsAndResolver(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	terms, err := d.LoadFraudTaxonomy(ctx, "typology")
	if err != nil {
		t.Fatalf("load typologies: %v", err)
	}
	if len(terms) < 15 {
		t.Fatalf("expected >=15 seeded typologies, got %d", len(terms))
	}
	r := fraudtracker.NewTermResolver(terms)
	for input, slug := range map[string]string{
		"COVID Subsidy fraud": "covid-subsidy-fraud",
		"Tax evasion":         "tax-fraud",
		"Mortgage Fraud":      "mortgage-property-fraud",
		"money laundering":    "money-laundering",
	} {
		got, ok := r.Resolve(input)
		if !ok || got.Slug != slug {
			t.Errorf("Resolve(%q) = (%q,%v), want %q", input, got.Slug, ok, slug)
		}
	}

	agencies, err := d.LoadFraudTaxonomy(ctx, "agency")
	if err != nil {
		t.Fatalf("load agencies: %v", err)
	}
	ar := fraudtracker.NewTermResolver(agencies)
	if got, ok := ar.Resolve("NZ police"); !ok || got.Slug != "nz-police" {
		t.Errorf("agency resolve failed: %v %v", got.Slug, ok)
	}

	if _, err := d.LoadFraudTaxonomy(ctx, "people"); err == nil {
		t.Error("unknown taxonomy kind must error (whitelist)")
	}
}

func TestCreateFraudCaseCompositeAndStatus(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	c := seedFraudCase(t, ctx, d, "Composite")
	// Status must be DERIVED from the sentencing event, not defaulted.
	if c.Status != domain.CaseConvicted {
		t.Errorf("status = %q, want convicted (derived from sentencing event)", c.Status)
	}

	got, parts, events, sources, err := d.GetFraudCase(ctx, c.ID)
	if err != nil {
		t.Fatalf("get case: %v", err)
	}
	if got.CaseName != "Composite" || len(parts) != 1 || len(events) != 2 || len(sources) != 1 {
		t.Fatalf("composite mismatch: parts=%d events=%d sources=%d", len(parts), len(events), len(sources))
	}
	if parts[0].PersonName != "Alex Composite" {
		t.Errorf("participant name join: %q", parts[0].PersonName)
	}
	if !got.NamingApproved {
		t.Error("naming_approved must default to true (0128 block-list posture: a reviewer unticks to block; legal gates still withhold names pre-verdict / on suppression flags)")
	}
	if got.ReviewState != "unverified" {
		t.Errorf("review_state = %q, want unverified", got.ReviewState)
	}
}

func TestAddEventRecomputesStatus(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	c := seedFraudCase(t, ctx, d, "Appeal")
	// Convicted -> appeal outcome with NO appeal_filed event: must land on overturned.
	updated, err := d.AddFraudCaseEvent(ctx, c.ID, domain.FraudCaseEvent{
		EventType: domain.EventAppealOutcome, EventDate: datePtr("2026-04-01"),
		OutcomeDetail: "Court of Appeal quashed the conviction",
	}, &domain.FraudCaseSource{
		URL: "https://example.test/appeal", SourceType: domain.FraudSourceNews,
		Publisher: "NZ Herald", ContentHash: "hash-appeal-evt",
	})
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	if updated.Status != domain.CaseOverturned {
		t.Errorf("status = %q, want overturned", updated.Status)
	}
	// The evidencing source must be attached and linked from the event.
	_, _, events, sources, err := d.GetFraudCase(ctx, c.ID)
	if err != nil {
		t.Fatalf("get case: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	last := events[len(events)-1]
	if last.SourceID == nil {
		t.Error("appeal event should reference its evidencing source")
	}

	if _, err := d.AddFraudCaseEvent(ctx, 99999, domain.FraudCaseEvent{EventType: domain.EventPlea}, nil); !errors.Is(err, store.ErrRowMissing) {
		t.Errorf("missing case: got %v, want ErrRowMissing", err)
	}
}

func TestFraudPersonOrgFindOrCreate(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	id1, created, err := d.FindOrCreateFraudPerson(ctx, "Jane Q Doe", []string{"Janey Doe"})
	if err != nil || !created {
		t.Fatalf("create person: %v created=%v", err, created)
	}
	// Exact name, case-insensitive.
	id2, created, err := d.FindOrCreateFraudPerson(ctx, "jane q doe", nil)
	if err != nil || created || id2 != id1 {
		t.Fatalf("dedupe by name: id=%d/%d created=%v err=%v", id1, id2, created, err)
	}
	// Alias match.
	id3, created, err := d.FindOrCreateFraudPerson(ctx, "Janey Doe", nil)
	if err != nil || created || id3 != id1 {
		t.Fatalf("dedupe by alias: id=%d/%d created=%v err=%v", id1, id3, created, err)
	}

	o1, created, err := d.FindOrCreateFraudOrganisation(ctx, "Acme Trading Ltd")
	if err != nil || !created {
		t.Fatalf("create org: %v", err)
	}
	o2, created, err := d.FindOrCreateFraudOrganisation(ctx, "ACME TRADING LTD")
	if err != nil || created || o2 != o1 {
		t.Fatalf("dedupe org: %d/%d created=%v err=%v", o1, o2, created, err)
	}
}

func TestFraudClaimLifecycle(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	claim := seedFraudClaim(t, ctx, d, "sfo-charges-director")
	if claim.ReviewState != domain.ReviewPending {
		t.Fatalf("new claim state = %q", claim.ReviewState)
	}

	// Dedupe: same publisher+headline+month collides, returns existing.
	dup, created, err := d.CreateFraudClaim(ctx, domain.FraudClaim{
		Headline: "sfo-charges-director", SourceType: domain.FraudSourceAgencyRelease,
		ContentHash: "different-hash", PublishedDate: datePtr("2026-05-20"),
	}, nil, "seed@test")
	if err != nil || created || dup.ID != claim.ID {
		t.Fatalf("dedupe: created=%v id=%d/%d err=%v", created, dup.ID, claim.ID, err)
	}

	// Content hash is mandatory provenance.
	if _, _, err := d.CreateFraudClaim(ctx, domain.FraudClaim{
		Headline: "no-hash", SourceType: domain.FraudSourceManual,
	}, nil, "seed@test"); err == nil {
		t.Error("claim without content_hash must be rejected")
	}

	rid := seedReviewer(t, ctx, d, "fraud-rev1@test")

	// Illegal edge: pending -> verified directly.
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewVerified, ReviewerID: &rid, Attestation: true,
	}); !errors.Is(err, fraudtracker.ErrIllegalTransition) {
		t.Errorf("pending->verified: got %v, want ErrIllegalTransition", err)
	}

	// pending -> in_review.
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewInReview, ReviewerID: &rid, ReviewerLabel: "rev1",
	}); err != nil {
		t.Fatalf("start review: %v", err)
	}

	// Verify blocked while the case attachment is undecided.
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewVerified, ReviewerID: &rid, Attestation: true,
	}); !errors.Is(err, fraudtracker.ErrMatchUndecided) {
		t.Errorf("verify undecided: got %v, want ErrMatchUndecided", err)
	}

	// Reviewer decides: no existing case (will promote as new).
	if err := d.SetFraudClaimNoMatch(ctx, claim.ID, &rid, "rev1"); err != nil {
		t.Fatalf("set no_match: %v", err)
	}

	// Verify still requires attestation.
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewVerified, ReviewerID: &rid,
	}); !errors.Is(err, fraudtracker.ErrNoAttestation) {
		t.Errorf("verify no attestation: got %v, want ErrNoAttestation", err)
	}

	verified, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewVerified, ReviewerID: &rid,
		ReviewerLabel: "rev1", Note: "checked", Attestation: true,
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.VerifiedBy == nil || *verified.VerifiedBy != rid {
		t.Error("verify must stamp the reviewer")
	}

	// Promote as a new case.
	c, err := d.PromoteFraudClaim(ctx, claim.ID, &store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Promoted Case", TypologyRaw: "fraud"},
		Events: []domain.FraudCaseEvent{{
			EventType: domain.EventChargesLaid, EventDate: datePtr("2026-05-01"),
		}},
		Sources: []domain.FraudCaseSource{{
			URL: "https://example.test/r/sfo-charges-director", ContentHash: "claimhash-sfo-charges-director",
			SourceType: domain.FraudSourceAgencyRelease,
		}},
	}, nil, nil, &rid, "rev1")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if c.Status != domain.CaseChargesLaid {
		t.Errorf("promoted case status = %q", c.Status)
	}

	// The claim now carries the case id and a full audit trail.
	got, _, reviews, err := d.GetFraudClaim(ctx, claim.ID)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.CaseID == nil || *got.CaseID != c.ID {
		t.Error("claim must be stamped with the promoted case id")
	}
	var actions []string
	for _, r := range reviews {
		actions = append(actions, r.Action)
	}
	want := []string{"created", "start_review", "match_reject", "verify", "promote"}
	if len(actions) != len(want) {
		t.Fatalf("audit trail %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("audit trail %v, want %v", actions, want)
		}
	}
}

func TestFraudClaimAttachToExistingCase(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	existing := seedFraudCase(t, ctx, d, "Existing")
	claim := seedFraudClaim(t, ctx, d, "appeal-outcome-existing")
	rid := seedReviewer(t, ctx, d, "fraud-rev2@test")

	// Case matching: shared participant name suggests the existing case.
	n, err := d.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: claim.ID, PersonNames: []string{"Alex Existing"}, OrgNames: []string{"Existing Holdings Ltd"},
	})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if n != 1 {
		t.Fatalf("suggestions = %d, want 1 (person + org hits collapse to the best per case)", n)
	}
	cands, err := d.ListFraudCaseMatchCandidates(ctx, claim.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %d err=%v, want 1", len(cands), err)
	}
	if cands[0].MatchScore < 0.89 {
		t.Errorf("best hit should be the exact person match (0.9), got %.2f via %s", cands[0].MatchScore, cands[0].MatchMethod)
	}

	if err := d.DecideFraudCaseMatch(ctx, claim.ID, cands[0].ID, true, &rid, "rev2"); err != nil {
		t.Fatalf("confirm match: %v", err)
	}

	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewInReview, ReviewerID: &rid,
	}); err != nil {
		t.Fatalf("start review: %v", err)
	}
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewVerified, ReviewerID: &rid, Attestation: true,
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Promote attaches an appeal event to the EXISTING case (the mechanism
	// that delivers "track them through the courts").
	c, err := d.PromoteFraudClaim(ctx, claim.ID, nil, &domain.FraudCaseEvent{
		EventType: domain.EventAppealOutcome, EventDate: datePtr("2026-06-01"),
		OutcomeDetail: "appeal dismissed",
	}, &domain.FraudCaseSource{
		URL: "https://example.test/r/appeal-outcome-existing", SourceType: domain.FraudSourceAgencyRelease,
		ContentHash: "claimhash-appeal-outcome-existing",
	}, &rid, "rev2")
	if err != nil {
		t.Fatalf("promote onto existing: %v", err)
	}
	if c.ID != existing.ID {
		t.Errorf("promoted onto case %d, want existing %d", c.ID, existing.ID)
	}
	if c.Status != domain.CaseUpheld {
		t.Errorf("status = %q, want upheld", c.Status)
	}
	cases, err := d.ListFraudCases(ctx, store.FraudCaseFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cases) != 1 {
		t.Errorf("cases = %d, want 1 (no duplicate case created)", len(cases))
	}
}

func TestFraudQuarantineTwoReviewerRule(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	claim := seedFraudClaim(t, ctx, d, "suppressed-matter")
	r1 := seedReviewer(t, ctx, d, "fq-rev1@test")
	r2 := seedReviewer(t, ctx, d, "fq-rev2@test")

	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewQuarantined, ReviewerID: &r1,
	}); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	// Exit without any vote: blocked.
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewInReview, ReviewerID: &r1, Note: "release",
	}); !errors.Is(err, fraudtracker.ErrSecondReviewer) {
		t.Errorf("no-vote exit: got %v, want ErrSecondReviewer", err)
	}

	// Self-vote then self-release: blocked as same reviewer.
	if err := d.RecordFraudReview(ctx, domain.FraudClaimReview{
		ClaimID: claim.ID, Reviewer: &r1, Action: "quarantine_release_vote",
		ToState: domain.ReviewQuarantined,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewInReview, ReviewerID: &r1, Note: "release",
	}); !errors.Is(err, fraudtracker.ErrSameReviewer) {
		t.Errorf("self-vote exit: got %v, want ErrSameReviewer", err)
	}

	// Distinct second reviewer releases.
	if _, err := d.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: claim.ID, To: domain.ReviewInReview, ReviewerID: &r2, Note: "release ok",
	}); err != nil {
		t.Fatalf("two-reviewer release: %v", err)
	}
}

func TestFraudClaimReviewsAppendOnly(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	claim := seedFraudClaim(t, ctx, d, "immutable-audit")
	if _, err := d.Pool.Exec(ctx, `UPDATE fraud_claim_reviews SET note='tampered' WHERE claim_id=$1`, claim.ID); err == nil {
		t.Fatal("UPDATE on fraud_claim_reviews must be blocked by trigger")
	}
	if _, err := d.Pool.Exec(ctx, `DELETE FROM fraud_claim_reviews WHERE claim_id=$1`, claim.ID); err == nil {
		t.Fatal("DELETE on fraud_claim_reviews must be blocked by trigger")
	}
}

// Matching v2: fuzzy participant names (pg_trgm) and amount-tolerance hits
// find cases that exact matching misses; confirmation stays human.
func TestSuggestFraudCaseMatchesFuzzyAndAmount(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	// Case with an aka-style participant name and a distinctive amount.
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName: "Fuzzy Target", TypologyRaw: "covid fraud", TotalAmountNZD: f64(1_234_567),
		},
		Participants: []store.FraudParticipantInput{{
			PersonName:  "Lucien Marcel Vantreux (aka Remy Calloway)",
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: "fz-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fuzzy: the follow-up article uses only the alias-ish short name.
	claimA := seedFraudClaim(t, ctx, d, "fuzzy-followup")
	n, err := d.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: claimA.ID, PersonNames: []string{"Remy Calloway"},
	})
	if err != nil || n != 1 {
		t.Fatalf("fuzzy name: n=%d err=%v", n, err)
	}
	cands, _ := d.ListFraudCaseMatchCandidates(ctx, claimA.ID)
	if len(cands) != 1 || cands[0].CaseID != c.ID || cands[0].MatchScore >= 0.9 {
		t.Fatalf("fuzzy candidate wrong: %+v", cands)
	}

	// Amount: appeal report with no shared names but the same distinctive sum.
	claimB := seedFraudClaim(t, ctx, d, "amount-followup")
	n, err = d.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: claimB.ID, AmountNZD: 1_240_000, // within 2%
	})
	if err != nil || n != 1 {
		t.Fatalf("amount match: n=%d err=%v", n, err)
	}
	cands, _ = d.ListFraudCaseMatchCandidates(ctx, claimB.ID)
	if len(cands) != 1 || cands[0].MatchMethod != "amount_region" {
		t.Fatalf("amount candidate wrong: %+v", cands)
	}

	// Small round sums never amount-match (too coincidental).
	claimC := seedFraudClaim(t, ctx, d, "small-amount")
	n, err = d.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{ClaimID: claimC.ID, AmountNZD: 5000})
	if err != nil || n != 0 {
		t.Fatalf("small amounts must not match: n=%d err=%v", n, err)
	}
}

// Court identifiers: stored on create, merged on append, and the strongest
// matcher signal.
func TestFraudCourtIdentifiers(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName: "CourtID Target", CourtIdentifiers: []string{"CRI-2024-004-1234"},
		},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: "cid-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.CourtIdentifiers) != 1 || c.CourtIdentifiers[0] != "CRI-2024-004-1234" {
		t.Fatalf("identifiers on create: %v", c.CourtIdentifiers)
	}

	// Merge keeps the set deduplicated.
	if err := d.AddFraudCaseCourtIdentifiers(ctx, c.ID, []string{"CA123/2025", "CRI-2024-004-1234"}); err != nil {
		t.Fatal(err)
	}
	got, _, _, _, err := d.GetFraudCase(ctx, c.ID)
	if err != nil || len(got.CourtIdentifiers) != 2 {
		t.Fatalf("merged identifiers: %v err=%v", got.CourtIdentifiers, err)
	}

	// An appeal report sharing only the CA number finds the case at 0.97.
	claim := seedFraudClaim(t, ctx, d, "courtid-followup")
	n, err := d.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: claim.ID, CourtIDs: []string{"CA123/2025"},
	})
	if err != nil || n != 1 {
		t.Fatalf("court id match: n=%d err=%v", n, err)
	}
	cands, _ := d.ListFraudCaseMatchCandidates(ctx, claim.ID)
	if len(cands) != 1 || cands[0].MatchMethod != "court_id" || cands[0].MatchScore < 0.96 {
		t.Fatalf("court id candidate wrong: %+v", cands)
	}
}
