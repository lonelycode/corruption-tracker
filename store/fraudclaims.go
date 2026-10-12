// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

// Fraud-claims review queue persistence. Every connector output and manual
// submission lands here as an UNVERIFIED claim; nothing reaches fraud_cases
// without a reviewer verifying the claim and promoting it (new case) or
// attaching it (event on an existing case). Mirrors convictions.go: guards
// run inside one transaction, every state change writes one append-only
// fraud_claim_reviews row (mutation blocked by the migration 0117 trigger).

const fraudClaimCols = `id, case_id, match_status, payload, headline, confidence_score,
	review_state, suppression_flag, needs_manual_extraction,
	verified_by, verified_at, attestation_note,
	source_url, source_publisher, source_type, licence_basis, published_date,
	retrieved_at, excerpt, content_hash, dedupe_key,
	created_at, updated_at, deleted_at`

func scanFraudClaim(row pgx.Row) (domain.FraudClaim, error) {
	var c domain.FraudClaim
	var matchStatus, reviewState, sourceType string
	err := row.Scan(&c.ID, &c.CaseID, &matchStatus, &c.Payload, &c.Headline, &c.ConfidenceScore,
		&reviewState, &c.SuppressionFlag, &c.NeedsManualExtraction,
		&c.VerifiedBy, &c.VerifiedAt, &c.AttestationNote,
		&c.SourceURL, &c.SourcePublisher, &sourceType, &c.LicenceBasis, &c.PublishedDate,
		&c.RetrievedAt, &c.Excerpt, &c.ContentHash, &c.DedupeKey,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt)
	if err != nil {
		return c, err
	}
	c.MatchStatus = domain.MatchStatus(matchStatus)
	c.ReviewState = domain.ReviewState(reviewState)
	c.SourceType = domain.FraudSourceType(sourceType)
	return c, nil
}

// ComputeFraudDedupeKey derives the (publisher-or-agency, headline-words,
// published-month) key the fraud_claims_dedupe_uq partial-unique index uses,
// so a re-poll of the same release collides instead of duplicating. Connectors
// may pre-set DedupeKey with something stronger (e.g. the release URL).
func ComputeFraudDedupeKey(publisher, headline string, published *time.Time) string {
	pub := strings.ToLower(strings.Join(strings.Fields(publisher), " "))
	head := strings.ToLower(strings.Join(strings.Fields(headline), " "))
	month := "0000-00"
	if published != nil {
		month = published.Format("2006-01")
	}
	return pub + "|" + head + "|" + month
}

// CreateFraudClaim inserts a pending claim with its inline provenance. If a
// live claim (including a rejected one) already carries the dedupe key, the
// existing claim is returned with created=false — re-ingestion never
// duplicates or resurfaces a matter. New claims get a genesis audit row.
func (d *Store) CreateFraudClaim(ctx context.Context, c domain.FraudClaim, reviewer *int64, reviewerLabel string) (claim domain.FraudClaim, created bool, err error) {
	if strings.TrimSpace(c.ContentHash) == "" {
		return claim, false, errors.New("fraud claim requires a content_hash (provenance)")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return claim, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	key := c.DedupeKey
	if key == nil || *key == "" {
		k := ComputeFraudDedupeKey(c.SourcePublisher, c.Headline, c.PublishedDate)
		key = &k
	}

	existing, err := scanFraudClaim(tx.QueryRow(ctx, `
		SELECT `+fraudClaimCols+`
		  FROM fraud_claims
		 WHERE dedupe_key = $1 AND deleted_at IS NULL
		 LIMIT 1`, *key))
	switch {
	case err == nil:
		if cerr := tx.Commit(ctx); cerr != nil {
			return claim, false, fmt.Errorf("commit: %w", cerr)
		}
		return existing, false, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to insert
	default:
		return claim, false, fmt.Errorf("dedupe lookup: %w", err)
	}

	payload := c.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	claim, err = scanFraudClaim(tx.QueryRow(ctx, `
		INSERT INTO fraud_claims
			(case_id, match_status, payload, headline, confidence_score,
			 review_state, suppression_flag, needs_manual_extraction,
			 source_url, source_publisher, source_type, licence_basis, published_date,
			 retrieved_at, excerpt, content_hash, dedupe_key)
		VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$8,$9,$10,$11,$12,COALESCE($13, now()),$14,$15,$16)
		RETURNING `+fraudClaimCols,
		c.CaseID, string(orDefaultFraudMatch(c.MatchStatus)), payload, c.Headline, c.ConfidenceScore,
		c.SuppressionFlag, c.NeedsManualExtraction,
		c.SourceURL, c.SourcePublisher, string(c.SourceType), c.LicenceBasis, c.PublishedDate,
		nullableTime(c.RetrievedAt), c.Excerpt, c.ContentHash, *key))
	if err != nil {
		return claim, false, fmt.Errorf("insert fraud claim: %w", err)
	}

	if err = insertFraudReviewTx(ctx, tx, domain.FraudClaimReview{
		ClaimID: claim.ID, Reviewer: reviewer, ReviewerLabel: reviewerLabel,
		Action: "created", ToState: domain.ReviewPending,
	}); err != nil {
		return claim, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return claim, false, fmt.Errorf("commit: %w", err)
	}
	return claim, true, nil
}

// insertFraudReviewTx appends one audit row — the ONLY write path to
// fraud_claim_reviews.
func insertFraudReviewTx(ctx context.Context, tx pgx.Tx, r domain.FraudClaimReview) error {
	var from any
	if r.FromState != nil {
		from = string(*r.FromState)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fraud_claim_reviews
			(claim_id, reviewer, reviewer_label, action, from_state, to_state, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		r.ClaimID, r.Reviewer, r.ReviewerLabel, r.Action, from, string(r.ToState), r.Note)
	if err != nil {
		return fmt.Errorf("insert fraud_claim_review: %w", err)
	}
	return nil
}

// ApplyFraudClaimExtraction writes a successful (re-)extraction onto a
// claim that was queued for manual extraction: payload, confidence, excerpt
// (the existing excerpt is kept when the extractor returned none) and the
// suppression flag (OR-merged, never cleared), and drops
// needs_manual_extraction so the bulk-approve path can process the claim.
// Review state is deliberately untouched — extraction fills in the data,
// verification stays a human decision. One audit row records the rewrite.
func (d *Store) ApplyFraudClaimExtraction(ctx context.Context, claimID int64, payload []byte, confidence float64, excerpt string, suppression bool, reviewerLabel, note string) error {
	if len(payload) == 0 {
		return errors.New("apply extraction: empty payload")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	err = tx.QueryRow(ctx, `
		UPDATE fraud_claims
		   SET payload=$2, confidence_score=$3,
		       excerpt=COALESCE(NULLIF($4,''), excerpt),
		       suppression_flag=(suppression_flag OR $5),
		       needs_manual_extraction=false, updated_at=now()
		 WHERE id=$1 AND deleted_at IS NULL
		 RETURNING review_state`,
		claimID, payload, confidence, excerpt, suppression).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRowMissing
		}
		return fmt.Errorf("apply extraction: %w", err)
	}
	if err := insertFraudReviewTx(ctx, tx, domain.FraudClaimReview{
		ClaimID: claimID, ReviewerLabel: reviewerLabel,
		Action: "extraction_applied", ToState: domain.ReviewState(state),
		Note: note,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListFraudClaimsForExtraction returns pending claims queued for manual
// extraction, oldest first — the extract-pending work list.
func (d *Store) ListFraudClaimsForExtraction(ctx context.Context, limit int) ([]domain.FraudClaim, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT `+fraudClaimCols+`
		  FROM fraud_claims
		 WHERE review_state='pending' AND needs_manual_extraction
		   AND deleted_at IS NULL
		 ORDER BY id
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list claims for extraction: %w", err)
	}
	defer rows.Close()
	var out []domain.FraudClaim
	for rows.Next() {
		c, err := scanFraudClaim(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claim for extraction: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordFraudReview appends an audit row without a state change (quarantine
// votes, edit-field notes).
func (d *Store) RecordFraudReview(ctx context.Context, r domain.FraudClaimReview) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertFraudReviewTx(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TransitionFraudClaim validates a review-state edge, runs the guards in one
// transaction, updates the claim, and appends one audit row atomically.
// Guards: legal edge, note-where-required, verify requires attestation AND a
// decided case attachment (confirmed existing case or no_match for new), and
// the two-distinct-reviewer quarantine-exit rule.
func (d *Store) TransitionFraudClaim(ctx context.Context, in fraudtracker.TransitionInput) (domain.FraudClaim, error) {
	var out domain.FraudClaim
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var from, matchStatus string
	if err := tx.QueryRow(ctx, `
		SELECT review_state, match_status
		  FROM fraud_claims
		 WHERE id=$1 AND deleted_at IS NULL
		 FOR UPDATE`, in.ClaimID).Scan(&from, &matchStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrRowMissing
		}
		return out, fmt.Errorf("lock fraud claim: %w", err)
	}
	fromState := domain.ReviewState(from)

	action, ok := fraudtracker.Action(fromState, in.To)
	if !ok {
		return out, fraudtracker.ErrIllegalTransition
	}
	if fraudtracker.NoteRequired(action) && strings.TrimSpace(in.Note) == "" {
		return out, fraudtracker.ErrNoteRequired
	}

	if in.To == domain.ReviewVerified {
		ms := domain.MatchStatus(matchStatus)
		if ms != domain.MatchConfirmed && ms != domain.MatchNoMatch {
			return out, fraudtracker.ErrMatchUndecided
		}
		if !in.Attestation {
			return out, fraudtracker.ErrNoAttestation
		}
	}

	if fraudtracker.IsQuarantineExit(fromState, in.To) {
		if err := requireSecondFraudReviewerTx(ctx, tx, in.ClaimID, in.ReviewerID); err != nil {
			return out, err
		}
	}

	if in.To == domain.ReviewVerified {
		out, err = scanFraudClaim(tx.QueryRow(ctx, `
			UPDATE fraud_claims
			   SET review_state=$2, verified_by=$3, verified_at=now(),
			       attestation_note=$4, updated_at=now()
			 WHERE id=$1 RETURNING `+fraudClaimCols,
			in.ClaimID, string(in.To), in.ReviewerID, in.Note))
	} else {
		out, err = scanFraudClaim(tx.QueryRow(ctx, `
			UPDATE fraud_claims
			   SET review_state=$2, updated_at=now()
			 WHERE id=$1 RETURNING `+fraudClaimCols,
			in.ClaimID, string(in.To)))
	}
	if err != nil {
		return out, fmt.Errorf("update fraud claim state: %w", err)
	}

	if err := insertFraudReviewTx(ctx, tx, domain.FraudClaimReview{
		ClaimID: in.ClaimID, Reviewer: in.ReviewerID, ReviewerLabel: in.ReviewerLabel,
		Action: action, FromState: &fromState, ToState: in.To, Note: in.Note,
	}); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

func requireSecondFraudReviewerTx(ctx context.Context, tx pgx.Tx, claimID int64, actor *int64) error {
	if actor == nil {
		return fraudtracker.ErrSecondReviewer
	}
	var others int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM fraud_claim_reviews
		 WHERE claim_id=$1 AND action='quarantine_release_vote'
		   AND reviewer IS NOT NULL AND reviewer <> $2`, claimID, *actor).Scan(&others); err != nil {
		return fmt.Errorf("count release votes: %w", err)
	}
	if others == 0 {
		var self int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM fraud_claim_reviews
			 WHERE claim_id=$1 AND action='quarantine_release_vote' AND reviewer=$2`,
			claimID, *actor).Scan(&self); err != nil {
			return fmt.Errorf("count self votes: %w", err)
		}
		if self > 0 {
			return fraudtracker.ErrSameReviewer
		}
		return fraudtracker.ErrSecondReviewer
	}
	return nil
}

// GetFraudClaim returns a claim with its case-match candidates and audit trail.
func (d *Store) GetFraudClaim(ctx context.Context, id int64) (domain.FraudClaim, []domain.FraudCaseMatchCandidate, []domain.FraudClaimReview, error) {
	claim, err := scanFraudClaim(d.Pool.QueryRow(ctx, `
		SELECT `+fraudClaimCols+` FROM fraud_claims WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return claim, nil, nil, ErrRowMissing
	}
	if err != nil {
		return claim, nil, nil, fmt.Errorf("get fraud claim: %w", err)
	}
	cands, err := d.ListFraudCaseMatchCandidates(ctx, id)
	if err != nil {
		return claim, nil, nil, err
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT id, claim_id, reviewer, reviewer_label, action, from_state, to_state, note, created_at
		  FROM fraud_claim_reviews WHERE claim_id=$1 ORDER BY created_at, id`, id)
	if err != nil {
		return claim, nil, nil, fmt.Errorf("list fraud reviews: %w", err)
	}
	defer rows.Close()
	var reviews []domain.FraudClaimReview
	for rows.Next() {
		var r domain.FraudClaimReview
		var fromState *string
		var toState string
		if err := rows.Scan(&r.ID, &r.ClaimID, &r.Reviewer, &r.ReviewerLabel, &r.Action,
			&fromState, &toState, &r.Note, &r.CreatedAt); err != nil {
			return claim, nil, nil, fmt.Errorf("scan fraud review: %w", err)
		}
		if fromState != nil {
			fs := domain.ReviewState(*fromState)
			r.FromState = &fs
		}
		r.ToState = domain.ReviewState(toState)
		reviews = append(reviews, r)
	}
	return claim, cands, reviews, rows.Err()
}

// FraudClaimFilter narrows ListFraudClaims.
type FraudClaimFilter struct {
	State         string
	Suppression   bool // only suppression-flagged
	ManualExtract bool // only needs_manual_extraction
	// Ready selects pending claims the queue's bulk-approve can fully
	// process: extracted payload present, no manual-extraction need, and no
	// pending match decision. Sorted confidence-first so the reviewer's
	// daily pass is tick-all -> approve, then triage the remainder.
	Ready  bool
	Limit  int
	Offset int
}

// ListFraudClaims returns live claims for the review queue, oldest-first
// within state so the queue drains fairly (Ready view sorts by confidence).
func (d *Store) ListFraudClaims(ctx context.Context, f FraudClaimFilter) ([]domain.FraudClaim, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	order := `created_at, id`
	if f.Ready {
		f.State = string(domain.ReviewPending)
		order = `confidence_score DESC NULLS LAST, created_at, id`
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT `+fraudClaimCols+`
		  FROM fraud_claims
		 WHERE deleted_at IS NULL
		   AND ($1 = '' OR review_state = $1)
		   AND (NOT $2 OR suppression_flag)
		   AND (NOT $3 OR needs_manual_extraction)
		   AND (NOT $6 OR (NOT needs_manual_extraction
		                   AND payload <> '{}'::jsonb
		                   AND match_status <> 'candidates_pending'))
		 ORDER BY `+order+`
		 LIMIT $4 OFFSET $5`,
		f.State, f.Suppression, f.ManualExtract, f.Limit, f.Offset, f.Ready)
	if err != nil {
		return nil, fmt.Errorf("list fraud claims: %w", err)
	}
	defer rows.Close()
	var out []domain.FraudClaim
	for rows.Next() {
		c, err := scanFraudClaim(rows)
		if err != nil {
			return nil, fmt.Errorf("scan fraud claim: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddFraudCaseMatchCandidate suggests an existing case a claim may attach to.
// Idempotent on (claim_id, case_id). Sets the claim's match_status to
// candidates_pending when it was unmatched.
func (d *Store) AddFraudCaseMatchCandidate(ctx context.Context, cand domain.FraudCaseMatchCandidate) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO fraud_case_match_candidates
			(claim_id, case_id, match_score, match_method, context_note)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (claim_id, case_id) DO NOTHING`,
		cand.ClaimID, cand.CaseID, cand.MatchScore, cand.MatchMethod, cand.ContextNote); err != nil {
		return fmt.Errorf("insert match candidate: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fraud_claims SET match_status='candidates_pending', updated_at=now()
		 WHERE id=$1 AND match_status='unmatched' AND deleted_at IS NULL`, cand.ClaimID); err != nil {
		return fmt.Errorf("bump match status: %w", err)
	}
	return tx.Commit(ctx)
}

// ListFraudCaseMatchCandidates returns a claim's candidates with case names.
func (d *Store) ListFraudCaseMatchCandidates(ctx context.Context, claimID int64) ([]domain.FraudCaseMatchCandidate, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT mc.id, mc.claim_id, mc.case_id, c.case_name, mc.match_score, mc.match_method,
		       mc.context_note, mc.decision, mc.decided_by, mc.decided_at, mc.created_at
		  FROM fraud_case_match_candidates mc
		  JOIN fraud_cases c ON c.id = mc.case_id
		 WHERE mc.claim_id=$1
		 ORDER BY mc.match_score DESC, mc.id`, claimID)
	if err != nil {
		return nil, fmt.Errorf("list match candidates: %w", err)
	}
	defer rows.Close()
	var out []domain.FraudCaseMatchCandidate
	for rows.Next() {
		var c domain.FraudCaseMatchCandidate
		var decision string
		if err := rows.Scan(&c.ID, &c.ClaimID, &c.CaseID, &c.CaseName, &c.MatchScore, &c.MatchMethod,
			&c.ContextNote, &decision, &c.DecidedBy, &c.DecidedAt, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan match candidate: %w", err)
		}
		c.Decision = domain.MatchDecision(decision)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DecideFraudCaseMatch records a reviewer verdict on a candidate. Confirming
// sets the claim's case_id + match_status=confirmed (one confirmed candidate
// per claim, DB-enforced); rejecting the last pending candidate returns the
// claim to unmatched. Both write an audit row.
func (d *Store) DecideFraudCaseMatch(ctx context.Context, claimID, candidateID int64, confirm bool, reviewerID *int64, reviewerLabel string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	if err := tx.QueryRow(ctx, `
		SELECT review_state FROM fraud_claims
		 WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, claimID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRowMissing
		}
		return fmt.Errorf("lock claim: %w", err)
	}

	decision, action := "rejected", "match_reject"
	if confirm {
		decision, action = "confirmed", "match_confirm"
	}
	var caseID int64
	if err := tx.QueryRow(ctx, `
		UPDATE fraud_case_match_candidates
		   SET decision=$3, decided_by=$4, decided_at=now()
		 WHERE id=$1 AND claim_id=$2
		 RETURNING case_id`, candidateID, claimID, decision, reviewerID).Scan(&caseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRowMissing
		}
		return fmt.Errorf("decide candidate: %w", err)
	}

	if confirm {
		if _, err := tx.Exec(ctx, `
			UPDATE fraud_claims SET case_id=$2, match_status='confirmed', updated_at=now()
			 WHERE id=$1`, claimID, caseID); err != nil {
			return fmt.Errorf("confirm attachment: %w", err)
		}
	} else {
		// If no pending candidates remain and none confirmed, fall back to
		// unmatched so the reviewer can mark no_match (new case) explicitly.
		if _, err := tx.Exec(ctx, `
			UPDATE fraud_claims SET match_status='unmatched', updated_at=now()
			 WHERE id=$1 AND match_status='candidates_pending'
			   AND NOT EXISTS (SELECT 1 FROM fraud_case_match_candidates
			                    WHERE claim_id=$1 AND decision IN ('pending','confirmed'))`, claimID); err != nil {
			return fmt.Errorf("reset match status: %w", err)
		}
	}

	if err := insertFraudReviewTx(ctx, tx, domain.FraudClaimReview{
		ClaimID: claimID, Reviewer: reviewerID, ReviewerLabel: reviewerLabel,
		Action: action, ToState: domain.ReviewState(state),
		Note: fmt.Sprintf("candidate case #%d %s", caseID, decision),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetFraudClaimNoMatch records the reviewer's explicit decision that no
// existing case matches — the claim will become a NEW case on promotion.
func (d *Store) SetFraudClaimNoMatch(ctx context.Context, claimID int64, reviewerID *int64, reviewerLabel string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT review_state FROM fraud_claims
		 WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, claimID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRowMissing
		}
		return fmt.Errorf("lock claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fraud_claims SET match_status='no_match', case_id=NULL, updated_at=now()
		 WHERE id=$1`, claimID); err != nil {
		return fmt.Errorf("set no_match: %w", err)
	}
	if err := insertFraudReviewTx(ctx, tx, domain.FraudClaimReview{
		ClaimID: claimID, Reviewer: reviewerID, ReviewerLabel: reviewerLabel,
		Action: "match_reject", ToState: domain.ReviewState(state),
		Note: "reviewer decided no existing case matches (will promote as new case)",
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PromoteFraudClaim turns a VERIFIED claim into case data. With a confirmed
// attachment (match_status=confirmed, case_id set) it appends the given
// event + source to that case; with no_match it creates a new case from the
// supplied input. Either way the claim is stamped with the case id and a
// 'promote' audit row is written. The caller (admin handler / importer)
// builds the FraudCaseInput / event from the reviewed payload — reviewer
// edits live there, the claim payload itself is never rewritten.
func (d *Store) PromoteFraudClaim(ctx context.Context, claimID int64, newCase *FraudCaseInput, event *domain.FraudCaseEvent, src *domain.FraudCaseSource, reviewerID *int64, reviewerLabel string) (domain.FraudCase, error) {
	var out domain.FraudCase
	claim, _, _, err := d.GetFraudClaim(ctx, claimID)
	if err != nil {
		return out, err
	}
	if claim.ReviewState != domain.ReviewVerified {
		return out, fraudtracker.ErrNotVerified
	}

	switch {
	case claim.MatchStatus == domain.MatchConfirmed && claim.CaseID != nil:
		if event == nil {
			return out, errors.New("promote onto existing case requires an event")
		}
		out, err = d.AddFraudCaseEvent(ctx, *claim.CaseID, *event, src)
		if err != nil {
			return out, err
		}
	case claim.MatchStatus == domain.MatchNoMatch:
		if newCase == nil {
			return out, errors.New("promote as new case requires case input")
		}
		out, err = d.CreateFraudCase(ctx, *newCase)
		if err != nil {
			return out, err
		}
	default:
		return out, fraudtracker.ErrMatchUndecided
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE fraud_claims SET case_id=$2, updated_at=now() WHERE id=$1`, claimID, out.ID); err != nil {
		return out, fmt.Errorf("stamp claim case: %w", err)
	}
	if err := insertFraudReviewTx(ctx, tx, domain.FraudClaimReview{
		ClaimID: claimID, Reviewer: reviewerID, ReviewerLabel: reviewerLabel,
		Action: "promote", ToState: domain.ReviewVerified,
		Note: fmt.Sprintf("promoted to case #%d", out.ID),
	}); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// FraudMatchInput carries the extracted signals used to find existing cases
// a claim plausibly belongs to.
type FraudMatchInput struct {
	ClaimID     int64
	PersonNames []string
	OrgNames    []string
	AmountNZD   float64  // 0 = unknown
	CourtIDs    []string // normalised CRI/CA/SC numbers + neutral citations
}

// SuggestFraudCaseMatches finds existing cases a claim plausibly belongs to,
// in decreasing strength:
//   - exact participant name / alias (0.9)
//   - fuzzy participant name via pg_trgm word similarity, catching variants
//     like "Remy Calloway" vs "Lucien Vantreux (aka Remy Calloway)" (≥0.5
//     similarity, scored sim*0.85 so exact always outranks fuzzy)
//   - exact organisation name (0.7), fuzzy organisation (≥0.6, sim*0.65)
//   - same amount within 2% for sums ≥ $10k (0.55) — distinctive sums are
//     how an appeal report with no shared names finds its original case
//
// Candidates are idempotent (first suggestion per case wins) and NEVER
// auto-confirmed — attachment is always a reviewer decision; these exist so
// that decision is one click instead of a search.
func (d *Store) SuggestFraudCaseMatches(ctx context.Context, in FraudMatchInput) (int, error) {
	type hit struct {
		caseID int64
		score  float64
		method string
		note   string
	}
	var hits []hit

	// Court identifiers first: an exact CRI/CA/SC/citation overlap is the
	// strongest signal there is (0.97) — identifiers don't collide.
	if len(in.CourtIDs) > 0 {
		rows, err := d.Pool.Query(ctx, `
			SELECT c.id, x.id_hit FROM fraud_cases c,
			       LATERAL (SELECT unnest(c.court_identifiers) AS id_hit) x
			 WHERE x.id_hit = ANY($1::text[])`, in.CourtIDs)
		if err != nil {
			return 0, fmt.Errorf("suggest by court id: %w", err)
		}
		for rows.Next() {
			var h hit
			var id string
			if err := rows.Scan(&h.caseID, &id); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan court id hit: %w", err)
			}
			h.method = "court_id"
			h.score = 0.97
			h.note = "court identifier match: " + id
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}

	for _, n := range in.PersonNames {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		rows, err := d.Pool.Query(ctx, `
			SELECT DISTINCT ON (c.id) c.id,
			       (lower(p.full_name) = lower($1)
			        OR p.aliases @> to_jsonb(ARRAY[lower($1)])) AS exact,
			       word_similarity(lower($1), lower(p.full_name)) AS sim,
			       p.full_name
			  FROM fraud_cases c
			  JOIN fraud_case_participants cp ON cp.case_id = c.id
			  JOIN fraud_persons p ON p.id = cp.person_id
			 WHERE lower(p.full_name) = lower($1)
			    OR p.aliases @> to_jsonb(ARRAY[lower($1)])
			    OR word_similarity(lower($1), lower(p.full_name)) >= 0.5
			 ORDER BY c.id, exact DESC, sim DESC`, n)
		if err != nil {
			return 0, fmt.Errorf("suggest by person: %w", err)
		}
		for rows.Next() {
			var h hit
			var exact bool
			var sim float64
			var existing string
			if err := rows.Scan(&h.caseID, &exact, &sim, &existing); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan person hit: %w", err)
			}
			h.method = "person_name"
			if exact {
				h.score = 0.9
				h.note = "participant name match: " + n
			} else {
				h.score = sim * 0.85
				h.note = fmt.Sprintf("fuzzy name match (%.2f): %q ~ existing %q", sim, n, existing)
			}
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}

	for _, n := range in.OrgNames {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		rows, err := d.Pool.Query(ctx, `
			SELECT DISTINCT ON (c.id) c.id,
			       (lower(o.name) = lower($1)) AS exact,
			       word_similarity(lower($1), lower(o.name)) AS sim,
			       o.name
			  FROM fraud_cases c
			  JOIN fraud_case_organisations co ON co.case_id = c.id
			  JOIN fraud_organisations o ON o.id = co.organisation_id
			 WHERE lower(o.name) = lower($1)
			    OR word_similarity(lower($1), lower(o.name)) >= 0.6
			 ORDER BY c.id, exact DESC, sim DESC`, n)
		if err != nil {
			return 0, fmt.Errorf("suggest by org: %w", err)
		}
		for rows.Next() {
			var h hit
			var exact bool
			var sim float64
			var existing string
			if err := rows.Scan(&h.caseID, &exact, &sim, &existing); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan org hit: %w", err)
			}
			h.method = "org_name"
			if exact {
				h.score = 0.7
				h.note = "organisation name match: " + n
			} else {
				h.score = sim * 0.65
				h.note = fmt.Sprintf("fuzzy org match (%.2f): %q ~ existing %q", sim, n, existing)
			}
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}

	if in.AmountNZD >= 10000 {
		rows, err := d.Pool.Query(ctx, `
			SELECT c.id, c.total_amount_nzd FROM fraud_cases c
			 WHERE c.total_amount_nzd IS NOT NULL
			   AND abs(c.total_amount_nzd - $1) <= $1 * 0.02`, in.AmountNZD)
		if err != nil {
			return 0, fmt.Errorf("suggest by amount: %w", err)
		}
		for rows.Next() {
			var h hit
			var amt float64
			if err := rows.Scan(&h.caseID, &amt); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan amount hit: %w", err)
			}
			h.method = "amount_region"
			h.score = 0.55
			h.note = fmt.Sprintf("sum involved matches within 2%%: claim $%.0f ~ case $%.0f", in.AmountNZD, amt)
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}

	// Best hit per case wins; insertion is idempotent on (claim, case).
	best := map[int64]hit{}
	for _, h := range hits {
		if cur, ok := best[h.caseID]; !ok || h.score > cur.score {
			best[h.caseID] = h
		}
	}
	added := 0
	for _, h := range best {
		if err := d.AddFraudCaseMatchCandidate(ctx, domain.FraudCaseMatchCandidate{
			ClaimID: in.ClaimID, CaseID: h.caseID, MatchScore: h.score,
			MatchMethod: h.method, ContextNote: h.note,
		}); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// orDefaultFraudMatch is the fraud-claim twin of orDefaultMatch (the tracker
// has its own MatchStatus type): an unset status inserts as unmatched.
func orDefaultFraudMatch(m domain.MatchStatus) domain.MatchStatus {
	if m == "" {
		return domain.MatchUnmatched
	}
	return m
}
