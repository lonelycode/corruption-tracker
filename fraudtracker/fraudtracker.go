// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package fraudtracker holds the legal-safety core of the corruption & fraud
// case tracker: the claim review state machine, the case-status derivation
// from lifecycle events, and the taxonomy alias resolver. Persistence lives
// in internal/storage/db (no-ORM convention); this package is the pure logic
// the DB layer, importer, and admin handlers call into.
//
// The review lifecycle is deliberately identical to the conviction-claims
// pipeline (internal/convictions) — same states, same edges, same
// two-distinct-reviewer quarantine rule — but kept as its own package: the
// two pipelines must be able to evolve independently and nothing about the
// fraud tracker may couple to the political-roster domain.
package fraudtracker

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
)

// Guard errors returned by the DB-layer transition/promote functions when a
// legal precondition is not met.
var (
	ErrIllegalTransition = errors.New("fraudtracker: illegal state transition")
	ErrNoAttestation     = errors.New("fraudtracker: verify requires reviewer attestation")
	ErrMatchUndecided    = errors.New("fraudtracker: verify requires the case attachment to be decided (confirmed existing case or no_match for a new one)")
	ErrSecondReviewer    = errors.New("fraudtracker: quarantine exit requires a second distinct reviewer")
	ErrSameReviewer      = errors.New("fraudtracker: second quarantine vote must be a distinct reviewer")
	ErrNoteRequired      = errors.New("fraudtracker: a note is required for this action")
	ErrNotVerified       = errors.New("fraudtracker: promote requires a verified claim")
)

// allowed[from][to] = audit action label. Edges not present are illegal.
// 'verified' and 'rejected' are terminal; rejected claims are retained so
// re-ingestion dedupes against them.
var allowed = map[domain.ReviewState]map[domain.ReviewState]string{
	domain.ReviewPending: {
		domain.ReviewInReview:    "start_review",
		domain.ReviewQuarantined: "quarantine",
		domain.ReviewRejected:    "reject",
	},
	domain.ReviewInReview: {
		domain.ReviewVerified:      "verify",
		domain.ReviewRejected:      "reject",
		domain.ReviewNeedsMoreInfo: "request_info",
		domain.ReviewQuarantined:   "quarantine",
	},
	domain.ReviewNeedsMoreInfo: {
		domain.ReviewInReview:    "start_review",
		domain.ReviewRejected:    "reject",
		domain.ReviewQuarantined: "quarantine",
	},
	domain.ReviewQuarantined: {
		domain.ReviewInReview: "release", // two-reviewer gated
		domain.ReviewRejected: "reject",  // two-reviewer gated
	},
}

// Action returns the audit label for an edge and whether the edge is legal.
func Action(from, to domain.ReviewState) (string, bool) {
	tos, ok := allowed[from]
	if !ok {
		return "", false
	}
	label, ok := tos[to]
	return label, ok
}

// IsQuarantineExit reports whether the two-distinct-reviewer rule applies.
func IsQuarantineExit(from, to domain.ReviewState) bool {
	return from == domain.ReviewQuarantined &&
		(to == domain.ReviewInReview || to == domain.ReviewRejected)
}

// NoteRequired reports whether an action must carry a non-empty note.
func NoteRequired(action string) bool {
	switch action {
	case "reject", "release":
		return true
	default:
		return false
	}
}

// TransitionInput is the data a reviewer action carries into the DB-layer
// transition function.
type TransitionInput struct {
	ClaimID       int64
	To            domain.ReviewState
	ReviewerID    *int64
	ReviewerLabel string
	Note          string
	// Attestation must be true for the verify edge and quarantine release —
	// the "I checked suppression + naming + attachment" checkbox.
	Attestation bool
}

// ───────────────────────── case-status derivation ─────────────────────────

// DeriveCaseStatus computes a case's lifecycle status from its events. The
// latest material event wins; events are ordered by eventOrderKey (event_date,
// with undated events placed as described there) then id. The model tolerates
// gaps — a case may jump straight from convicted to an appeal outcome with no
// appeal_filed.
//
// An empty event list yields charges_laid (the entry state: a case only
// exists once charges are laid).
func DeriveCaseStatus(events []domain.FraudCaseEvent) domain.FraudCaseStatus {
	if len(events) == 0 {
		return domain.CaseChargesLaid
	}
	type keyed struct {
		e   domain.FraudCaseEvent
		key orderKey
	}
	sorted := make([]keyed, len(events))
	for i, e := range events {
		sorted[i] = keyed{e, eventOrderKey(e, events)}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		ki, kj := sorted[i].key, sorted[j].key
		if !ki.date.Equal(kj.date) {
			return ki.date.Before(kj.date)
		}
		if ki.floored != kj.floored {
			return !ki.floored
		}
		return sorted[i].e.ID < sorted[j].e.ID
	})
	status := domain.CaseChargesLaid
	for _, k := range sorted {
		if s, material := eventStatus(k.e); material {
			status = s
		}
	}
	return status
}

// orderKey places one event for status derivation.
type orderKey struct {
	date time.Time
	// floored marks an undated event pinned to date: it sorts after every
	// event that really is dated on that day.
	floored bool
}

// eventOrderKey orders events for status derivation. UNDATED events sort
// before all dated ones: an event with no date cannot claim recency, and a
// late-recorded historical event (e.g. an enrichment backfill of an old
// first appearance) must never outrank a dated verdict. Among undated
// events, insertion order (the stable sort's ID tiebreak) holds.
//
// The exception is an undated appeal event. An appeal can only follow the
// conviction it challenges, so it is floored just after the latest dated
// event it must follow (appealFollows). Without the floor, dating the
// conviction later (recode's date fill, a reviewer edit) moved a dismissed
// appeal in front of it and flipped the case from upheld back to convicted.
func eventOrderKey(e domain.FraudCaseEvent, all []domain.FraudCaseEvent) orderKey {
	if e.EventDate != nil {
		return orderKey{date: *e.EventDate}
	}
	follows := appealFollows[e.EventType]
	if follows == nil {
		return orderKey{}
	}
	var floor *time.Time
	for _, o := range all {
		if o.EventDate != nil && follows[o.EventType] && (floor == nil || o.EventDate.After(*floor)) {
			floor = o.EventDate
		}
	}
	if floor == nil {
		return orderKey{}
	}
	return orderKey{date: *floor, floored: true}
}

// appealFollows lists, per appeal event type, the event types it necessarily
// comes after. Earlier stages (charges, appearances, retrials) are left out
// on purpose: a retrial can follow an appeal, so a dated retrial never pushes
// an undated appeal past it.
var appealFollows = map[domain.FraudEventType]map[domain.FraudEventType]bool{
	domain.EventAppealFiled: {domain.EventVerdict: true, domain.EventSentencing: true},
	domain.EventAppealHeard: {domain.EventVerdict: true, domain.EventSentencing: true, domain.EventAppealFiled: true},
	domain.EventAppealOutcome: {domain.EventVerdict: true, domain.EventSentencing: true,
		domain.EventAppealFiled: true, domain.EventAppealHeard: true},
}

// eventStatus maps one event to the case status it implies. Verdict and
// appeal-outcome events are disambiguated from outcome_detail keywords; a
// reviewer who needs a different reading records a corrected event rather
// than editing status (which is always derived).
func eventStatus(e domain.FraudCaseEvent) (domain.FraudCaseStatus, bool) {
	switch e.EventType {
	case domain.EventChargesLaid:
		return domain.CaseChargesLaid, true
	case domain.EventFirstAppearance, domain.EventPlea, domain.EventTrialStart, domain.EventRetrial:
		return domain.CaseBeforeCourt, true
	case domain.EventVerdict:
		if containsAny(e.OutcomeDetail, "acquit", "not guilty") {
			return domain.CaseAcquitted, true
		}
		return domain.CaseConvicted, true
	case domain.EventSentencing:
		return domain.CaseConvicted, true
	case domain.EventAppealFiled, domain.EventAppealHeard:
		return domain.CaseAppealPending, true
	case domain.EventAppealOutcome:
		if containsAny(e.OutcomeDetail, "overturn", "quash", "set aside", "allowed", "acquit") {
			return domain.CaseOverturned, true
		}
		return domain.CaseUpheld, true
	case domain.EventDiscontinued:
		return domain.CaseDiscontinued, true
	}
	return "", false
}

func containsAny(s string, subs ...string) bool {
	l := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// ───────────────────────── taxonomy resolution ─────────────────────────

// TermResolver maps free-text input onto taxonomy terms via slug, label, and
// alias equality after normalisation. Resolution is deterministic and
// auditable: no fuzzy matching — unmapped input stays unmapped (the raw text
// is preserved on the case for later alias promotion).
type TermResolver struct {
	byKey map[string]domain.FraudTaxonomyTerm
}

// NewTermResolver indexes terms by normalised slug, label, and each alias.
func NewTermResolver(terms []domain.FraudTaxonomyTerm) *TermResolver {
	r := &TermResolver{byKey: make(map[string]domain.FraudTaxonomyTerm)}
	for _, t := range terms {
		if !t.Active {
			continue
		}
		r.add(t.Slug, t)
		r.add(t.Label, t)
		for _, a := range t.Aliases {
			r.add(a, t)
		}
	}
	return r
}

func (r *TermResolver) add(key string, t domain.FraudTaxonomyTerm) {
	k := Normalize(key)
	if k == "" {
		return
	}
	if _, exists := r.byKey[k]; !exists {
		r.byKey[k] = t
	}
}

// Resolve returns the term for a free-text input, if any.
func (r *TermResolver) Resolve(raw string) (domain.FraudTaxonomyTerm, bool) {
	t, ok := r.byKey[Normalize(raw)]
	return t, ok
}

// Normalize lowercases, trims, collapses whitespace, and strips trailing
// punctuation — the canonical form for alias keys.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, ".,;:")
}

// ───────────────────────── court identifiers ─────────────────────────
//
// NZ court identifiers as they appear in public documents: CRI numbers
// (CRI-2024-004-1234), appeal registry numbers (CA123/2025, SC 45/2025),
// and neutral citations ([2025] NZHC 123, [2025] NZDC 4567). There is no
// queryable registry behind them — they are matching glue and provenance.

var courtIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bCRI[- ]\d{4}[- ]\d{3}[- ]\d{1,6}\b`),
	regexp.MustCompile(`(?i)\b(?:CA|SC)\s?\d{1,4}/\d{4}\b`),
	regexp.MustCompile(`(?i)\[\d{4}\]\s?NZ(?:HC|DC|CA|SC|EmpC|ERA)\s?\d{1,5}\b`),
}

// DetectCourtIdentifiers scans free text for court identifiers and returns
// them normalised and deduplicated.
func DetectCourtIdentifiers(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, re := range courtIDPatterns {
		for _, m := range re.FindAllString(text, -1) {
			id := NormalizeCourtID(m)
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// NormalizeCourtID canonicalises an identifier: uppercase, single spaces,
// "CRI 2024 004 1234" -> "CRI-2024-004-1234". Unrecognisable input yields ""
// so junk the extractor invents never lands in the array.
func NormalizeCourtID(s string) string {
	s = strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "CRI") {
		s = strings.ReplaceAll(s, " ", "-")
		s = strings.ReplaceAll(s, "--", "-")
	} else if strings.HasPrefix(s, "[") {
		// Neutral citation: "[2025]NZHC 123" -> "[2025] NZHC 123".
		s = regexp.MustCompile(`\]\s*NZ`).ReplaceAllString(s, "] NZ")
		s = regexp.MustCompile(`([A-Z])\s*(\d+)$`).ReplaceAllString(s, "$1 $2")
	} else {
		// Appeal numbers: "CA 123/2025" -> "CA123/2025".
		s = strings.ReplaceAll(s, " ", "")
	}
	for _, re := range courtIDPatterns {
		if re.MatchString(s) {
			return s
		}
	}
	return ""
}
