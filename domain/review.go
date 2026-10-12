// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package domain

// Review and match lifecycle vocabulary. These are the tracker's own copies
// of the enums alitheia's conviction pipeline uses; the string values are
// stored in fraud_claims / fraud_case_match_candidates and must not change.

// ReviewState is the human-review lifecycle of a claim. Values mirror the
// CHECK constraint on fraud_claims.review_state.
type ReviewState string

const (
	ReviewPending       ReviewState = "pending"
	ReviewInReview      ReviewState = "in_review"
	ReviewVerified      ReviewState = "verified"
	ReviewRejected      ReviewState = "rejected"
	ReviewNeedsMoreInfo ReviewState = "needs_more_info"
	ReviewQuarantined   ReviewState = "quarantined_suppression"
)

// MatchStatus tracks the case-link lifecycle of a claim, distinct from the
// review workflow. Mirrors fraud_claims.match_status.
type MatchStatus string

const (
	MatchUnmatched         MatchStatus = "unmatched"
	MatchCandidatesPending MatchStatus = "candidates_pending"
	MatchConfirmed         MatchStatus = "confirmed"
	MatchNoMatch           MatchStatus = "no_match"
)

// MatchDecision is a reviewer's verdict on a match candidate. Mirrors
// fraud_case_match_candidates.decision.
type MatchDecision string

const (
	DecisionPending   MatchDecision = "pending"
	DecisionConfirmed MatchDecision = "confirmed"
	DecisionRejected  MatchDecision = "rejected"
)
