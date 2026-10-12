// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package domain holds the corruption & fraud case tracker's entity types
// and lifecycle vocabulary. It has no dependency on the embedding app.
package domain

import (
	"encoding/json"
	"time"
)

// Corruption & fraud case tracker entities. Cases are the unit of analysis;
// persons are participants, never the unit. Everything is review-gated and
// org-flag-gated — no public surface reads these tables. The fraud_* schema
// is HARD-SEPARATED from the political people/organisations tables by
// design (see migration 0116 header and the original tracker design notes).
//
// ReviewState / MatchStatus / MatchDecision (review.go) are the tracker's
// own copies of alitheia's conviction-pipeline enums: the review lifecycle
// semantics and the stored string values are identical.

// FraudCaseStatus is the court-lifecycle position of a case, DERIVED from
// the latest material fraud_case_events row (fraudtracker.DeriveCaseStatus)
// — never hand-edited. Mirrors fraud_cases.status.
type FraudCaseStatus string

const (
	CaseChargesLaid   FraudCaseStatus = "charges_laid"
	CaseBeforeCourt   FraudCaseStatus = "before_court"
	CaseConvicted     FraudCaseStatus = "convicted"
	CaseAcquitted     FraudCaseStatus = "acquitted"
	CaseAppealPending FraudCaseStatus = "appeal_pending"
	CaseOverturned    FraudCaseStatus = "overturned"
	CaseUpheld        FraudCaseStatus = "upheld"
	CaseDiscontinued  FraudCaseStatus = "discontinued"
)

// PreVerdict reports whether a status is before any finding — such cases
// render with the ALLEGED badge and default to unnamed in all report output.
func (s FraudCaseStatus) PreVerdict() bool {
	switch s {
	case CaseChargesLaid, CaseBeforeCourt:
		return true
	}
	return false
}

// FraudEventType mirrors fraud_case_events.event_type.
type FraudEventType string

const (
	EventChargesLaid     FraudEventType = "charges_laid"
	EventFirstAppearance FraudEventType = "first_appearance"
	EventPlea            FraudEventType = "plea"
	EventTrialStart      FraudEventType = "trial_start"
	EventVerdict         FraudEventType = "verdict"
	EventSentencing      FraudEventType = "sentencing"
	EventAppealFiled     FraudEventType = "appeal_filed"
	EventAppealHeard     FraudEventType = "appeal_heard"
	EventAppealOutcome   FraudEventType = "appeal_outcome"
	EventRetrial         FraudEventType = "retrial"
	EventDiscontinued    FraudEventType = "discontinued"
)

// FraudSourceType mirrors fraud_sources.source_type / fraud_claims.source_type.
type FraudSourceType string

const (
	FraudSourceAgencyRelease FraudSourceType = "agency_release"
	FraudSourceNews          FraudSourceType = "news"
	FraudSourceJudgment      FraudSourceType = "judgment"
	FraudSourceManual        FraudSourceType = "manual"
)

// FraudTaxonomyTerm is one row of any of the five fraud taxonomies
// (typologies, agencies, regions, profession_categories, sectors). Aliases
// are lowercased free-text variants; resolution is deterministic.
type FraudTaxonomyTerm struct {
	ID      int64
	Slug    string
	Label   string
	Aliases []string
	Active  bool
	Sort    int
}

// FraudPerson carries minimal PII by design: name + aliases only. No DOB,
// no address, no contact data, no link to the political roster.
type FraudPerson struct {
	ID        int64
	FullName  string
	Aliases   []string
	Notes     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FraudOrganisation is a company/agency involved in a case.
type FraudOrganisation struct {
	ID        int64
	Name      string
	NZBN      *string
	Notes     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FraudCase is the unit of analysis.
type FraudCase struct {
	ID       int64
	CaseName string // internal label, never published
	Status   FraudCaseStatus

	TypologyID          *int64
	TypologyRaw         string
	SectorID            *int64
	RegionID            *int64
	RegionRaw           string
	ProsecutingAgencyID *int64
	AgencyRaw           string

	TotalAmountNZD *float64
	AmountBasis    *string // alleged | proven | reparations_ordered
	AmountNote     string

	OffendingPeriodStart     *time.Time
	OffendingPeriodEnd       *time.Time
	OffendingPeriodPrecision *string // day | month | year

	CourtName  string
	CourtLevel string
	// CourtIdentifiers are normalised CRI/CA/SC numbers and neutral
	// citations as stated by sources — matching glue and provenance, not a
	// link into any external registry (none is publicly queryable). On
	// org-facing surfaces they follow the naming rule: a citation locates
	// the judgment and thereby the name.
	CourtIdentifiers []string

	VictimClass  *string
	VictimDetail string
	VictimCount  *int

	CovidRelated bool
	Summary      string

	// NamingApproved: a reviewer has confirmed no suppression order applies
	// and approved naming. Pre-verdict cases additionally render unnamed
	// regardless unless this is set (template-enforced).
	NamingApproved bool

	ReviewState string // unverified | verified
	VerifiedBy  *int64
	VerifiedAt  *time.Time

	// StatusOverridden marks a case whose status was set by hand rather
	// than derived from its events. When true, recomputeFraudCaseStatusTx
	// leaves the stored status alone (a later event edit / recode can't
	// clobber the human value). Cleared by the "revert to auto-derived"
	// action, which re-enables derivation.
	StatusOverridden bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// FraudCaseParticipant is a person's role in a case.
type FraudCaseParticipant struct {
	ID       int64
	CaseID   int64
	PersonID int64
	Role     string // defendant | co_accused | named_associate

	ProfessionRaw        string
	ProfessionCategoryID *int64
	Gender               string

	IndividualCharges string
	// Only set when distinguishable from the case total; aggregates use the
	// case-level amount to avoid co-accused multi-counting.
	IndividualAmountNZD *float64

	Outcome        *string // convicted | acquitted | charges_withdrawn | conviction_overturned
	SentenceType   *string
	SentenceDetail string
	SentenceMonths *int

	SuppressionFlag    bool
	CleanSlatePossible bool

	CreatedAt time.Time
	UpdatedAt time.Time

	// PersonName is display-only, joined from fraud_persons.
	PersonName string
}

// FraudCaseEvent is one row of the lifecycle spine.
type FraudCaseEvent struct {
	ID                 int64
	CaseID             int64
	EventType          FraudEventType
	EventDate          *time.Time
	EventDatePrecision *string
	Court              string
	OutcomeDetail      string
	SourceID           *int64
	CreatedAt          time.Time
}

// FraudCaseSource is provenance attached at case level. Excerpt is private
// and never exposed on any surface.
type FraudCaseSource struct {
	ID            int64
	CaseID        int64
	URL           string
	Publisher     string
	SourceType    FraudSourceType
	LicenceBasis  string
	PublishedDate *time.Time
	RetrievedAt   time.Time
	Excerpt       string // private; never republished
	ContentHash   string
	CreatedAt     time.Time
}

// FraudClaim is one unverified assertion from a connector or manual
// submission, awaiting human review. Provenance is inline (one capture per
// claim). Payload holds the full structured extraction; reviewer edits are
// applied at promote time and the original payload is never rewritten.
type FraudClaim struct {
	ID          int64
	CaseID      *int64 // set when attachment to an existing case is confirmed, or on promote
	MatchStatus MatchStatus

	Payload         json.RawMessage
	Headline        string
	ConfidenceScore *float64

	ReviewState           ReviewState
	SuppressionFlag       bool
	NeedsManualExtraction bool

	VerifiedBy      *int64
	VerifiedAt      *time.Time
	AttestationNote *string

	SourceURL       string
	SourcePublisher string
	SourceType      FraudSourceType
	LicenceBasis    string
	PublishedDate   *time.Time
	RetrievedAt     time.Time
	Excerpt         string // private; never republished
	ContentHash     string

	DedupeKey *string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// FraudClaimReview is one append-only audit row for a fraud claim (trigger
// in migration 0117 blocks UPDATE/DELETE).
type FraudClaimReview struct {
	ID            int64
	ClaimID       int64
	Reviewer      *int64
	ReviewerLabel string
	Action        string
	FromState     *ReviewState
	ToState       ReviewState
	Note          string
	CreatedAt     time.Time
}

// FraudCaseMatchCandidate is an existing case a claim might attach to (so
// an appeal outcome becomes an event on the original case rather than a
// duplicate case). Never auto-confirmed.
type FraudCaseMatchCandidate struct {
	ID          int64
	ClaimID     int64
	CaseID      int64
	CaseName    string // joined for display
	MatchScore  float64
	MatchMethod string // person_name | org_name | amount_region | typology_window | manual
	ContextNote string
	Decision    MatchDecision
	DecidedBy   *int64
	DecidedAt   *time.Time
	CreatedAt   time.Time
}
