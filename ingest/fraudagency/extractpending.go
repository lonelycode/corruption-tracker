// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

// Pending-extraction backfill. Claims land in the queue with
// needs_manual_extraction for reasons that have nothing to do with the
// underlying matter — the LLM endpoint was down mid-poll, a spreadsheet
// problem row carried its data as a raw row instead of prose, a mail alert
// was too terse to spend a call on. Each one then costs the reviewer a full
// detail-page pass (new case, suppression tick, field entry) that the
// extractor performs in one call for every other claim.
//
// ExtractPending retries extraction over that backlog: refetch the source
// URL when there is one (robots-respecting, same rules as enrichment),
// otherwise run on what the claim already holds (headline + excerpt + any
// non-extraction payload, e.g. the raw sheet row). A successful extraction
// is written onto the claim IN PLACE — payload, confidence, suppression
// screen, match suggestions — and the claim stays exactly where it was in
// the review queue: verification and promotion remain the reviewer's
// bulk-approve decision. Extraction here only recovers the structured data
// (typology, stage, amounts, dates) that makes the resulting case worth
// having in aggregates; it never decides anything.
//
// Idempotency: success clears needs_manual_extraction (leaves the work
// list); a not-an-enforcement-matter verdict auto-rejects, the established
// dedupe memory (same as poll/classify/mail). Only transient failures
// (fetch or LLM errors) stay queued for the next run.

// ReextractStats summarises one ExtractPending run.
type ReextractStats struct {
	Claims        int // work list size
	Extracted     int // payload applied, claim now bulk-approvable
	NotFraudCase  int // extractor disagreed -> auto-rejected
	TooThin       int // below the text floor, no LLM call spent
	FetchErrors   int // source URL unfetchable (fell back to stored text)
	RobotsBlocked int // source URL disallowed (fell back to stored text)
	Quarantined   int // suppression signal found during re-extraction
	Suggestions   int // case-match candidates created
	Errors        int
}

// reextractMinChars is the floor below which a claim has nothing to extract
// from — e.g. a sheet row whose entire content is "Fraud, Bribery". Those
// stay manual: a human can reject them faster than an LLM can hallucinate
// structure into them.
const reextractMinChars = 80

// ExtractPending retries extraction over pending manual-extraction claims.
func (d *Deps) ExtractPending(ctx context.Context, limit int) (ReextractStats, error) {
	var st ReextractStats
	if d.LLM == nil {
		return st, errors.New("fraud extract-pending requires an LLM completer")
	}
	if d.Fetcher == nil {
		d.Fetcher = NewFetcher()
	}
	if d.Delay == 0 {
		d.Delay = 2 * time.Second
	}

	claims, err := d.DB.ListFraudClaimsForExtraction(ctx, limit)
	if err != nil {
		return st, err
	}
	st.Claims = len(claims)

	robotsCache := map[string]string{}
	for _, claim := range claims {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if err := d.extractOne(ctx, claim, robotsCache, &st); err != nil {
			st.Errors++
			d.Log.Warn("extract-pending claim failed", "claim", claim.ID, "err", err)
		}
	}
	return st, nil
}

// claimStoredText assembles the extractable text a claim already carries:
// headline, excerpt, and any non-extraction payload (the sheet importer
// stores the raw problem row there — names, year, amount, outcome).
func claimStoredText(c domain.FraudClaim) string {
	parts := []string{strings.TrimSpace(c.Headline), strings.TrimSpace(c.Excerpt)}
	if p := strings.TrimSpace(string(c.Payload)); p != "" && p != "{}" {
		parts = append(parts, "Structured source data:\n"+p)
	}
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

func (d *Deps) extractOne(ctx context.Context, claim domain.FraudClaim, robotsCache map[string]string, st *ReextractStats) error {
	text := claimStoredText(claim)
	if claim.SourceURL != "" {
		switch allowed, err := d.enrichRobotsAllowed(ctx, robotsCache, claim.SourceURL); {
		case err != nil || !allowed:
			st.RobotsBlocked++
		default:
			if html, err := d.Fetcher.Get(ctx, claim.SourceURL); err != nil {
				st.FetchErrors++ // dead link — stored text still gets a shot
				d.Log.Debug("extract-pending fetch failed", "claim", claim.ID, "url", claim.SourceURL, "err", err)
			} else {
				// Page first (the substance), stored text after (headline and
				// sheet-row fields disambiguate listing-style pages).
				text = HTMLToText(html) + "\n\n" + text
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d.Delay):
			}
		}
	}
	if len(text) < reextractMinChars {
		st.TooThin++
		return nil
	}

	ex, raw, err := runExtraction(ctx, d.LLM, text)
	if err != nil {
		return err // transient: stays manual, retried next run
	}
	if !ex.IsFraudCase {
		st.NotFraudCase++
		if d.DryRun {
			return nil
		}
		_, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: claim.ID, To: domain.ReviewRejected,
			ReviewerLabel: "fraudreextract",
			Note: fmt.Sprintf("auto-rejected: re-extraction classified as not an enforcement matter (confidence=%.2f)",
				ex.Confidence),
		})
		return err
	}

	suppressed, suppressNote := suppressionScreen(ex, text)
	if d.DryRun {
		d.Log.Info("dry-run: would apply extraction", "claim", claim.ID,
			"stage", ex.CaseStage, "suppression", suppressed)
		st.Extracted++
		return nil
	}
	if err := d.DB.ApplyFraudClaimExtraction(ctx, claim.ID, raw, ex.Confidence, ex.ExactExcerpt,
		suppressed, "fraudreextract",
		fmt.Sprintf("extraction recovered (stage=%s, confidence=%.2f)", ex.CaseStage, ex.Confidence)); err != nil {
		return err
	}
	st.Extracted++

	if suppressed && claim.ReviewState == domain.ReviewPending {
		if _, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: claim.ID, To: domain.ReviewQuarantined,
			ReviewerLabel: "fraudreextract",
			Note:          "auto-quarantined: suppression signal found on re-extraction: " + firstChars(suppressNote, 120),
		}); err != nil {
			d.Log.Warn("extract-pending quarantine failed", "claim", claim.ID, "err", err)
		} else {
			st.Quarantined++
		}
	}

	var orgNames []string
	for _, o := range ex.Organisations {
		if strings.TrimSpace(o.Name) != "" {
			orgNames = append(orgNames, o.Name)
		}
	}
	n, err := d.DB.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID:     claim.ID,
		PersonNames: namedPersons(ex),
		OrgNames:    orgNames,
		AmountNZD:   ex.TotalAmountNZD,
		CourtIDs:    courtIDs(ex, text),
	})
	if err != nil {
		d.Log.Warn("extract-pending match suggestion failed", "claim", claim.ID, "err", err)
		return nil
	}
	st.Suggestions += n
	return nil
}
