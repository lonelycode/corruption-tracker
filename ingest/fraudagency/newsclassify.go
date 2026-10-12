// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/host"
)

// News-corpus classifier: scans the already-ingested news_articles /
// commentary_items (read-only, via the host.NewsItemSource port) for
// fraud/corruption enforcement reporting and routes hits into the
// fraud-claims review queue. Unlike the convictions classifier there is no
// political-roster gate — the fraud tracker covers whoever the courts do.
//
// Cost control mirrors the agency pollers: vocabulary pre-filter before any
// LLM spend, URL-keyed dedupe, and LLM-rejected items recorded as
// auto-rejected claims so a re-scan never re-extracts them. Vocab-rejected
// corpus items are NOT recorded (the corpus is large and re-checking the
// vocabulary locally is free — recording them would bloat the queue table).

const newsLicenceBasis = "news-fair-dealing (internal review only; excerpt never republished)"

// ClassifyStats summarises a corpus classification run.
type ClassifyStats struct {
	Scanned      int
	VocabHits    int
	SkippedKnown int // URL already in the queue
	NotFraudCase int
	ClaimsNew    int
	Quarantined  int
	Suggestions  int
	Errors       int
}

// ClassifyCorpus runs the classifier over corpus items. Requires an LLM:
// without extraction every court story would land in the manual queue,
// which helps nobody.
func (d *Deps) ClassifyCorpus(ctx context.Context, items []host.NewsItem) (ClassifyStats, error) {
	var st ClassifyStats
	if d.LLM == nil {
		return st, errors.New("fraud classify-news requires an LLM completer")
	}
	for _, it := range items {
		st.Scanned++
		text := strings.TrimSpace(strings.Join([]string{it.Title, it.Summary, it.Body}, "\n\n"))
		if text == "" || it.URL == "" {
			continue
		}
		if !HasFraudVocabulary(text) {
			continue
		}
		st.VocabHits++

		key := "url:" + it.URL
		exists, err := d.claimKeyExists(ctx, key)
		if err != nil {
			return st, err
		}
		if exists {
			st.SkippedKnown++
			continue
		}

		if err := d.classifyItem(ctx, it, text, key, &st); err != nil {
			st.Errors++
			d.Log.Warn("fraud classify item failed", "url", it.URL, "err", err)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		default:
		}
	}
	return st, nil
}

func (d *Deps) classifyItem(ctx context.Context, it host.NewsItem, text, dedupeKey string, st *ClassifyStats) error {
	sum := sha256.Sum256([]byte(text))
	hash := "newscorpus:" + hex.EncodeToString(sum[:])

	base := domain.FraudClaim{
		Headline:        it.Title,
		SourceType:      domain.FraudSourceNews,
		SourceURL:       it.URL,
		SourcePublisher: it.Source,
		LicenceBasis:    newsLicenceBasis,
		ContentHash:     hash,
		DedupeKey:       &dedupeKey,
	}
	if !it.PublishedAt.IsZero() {
		pd := it.PublishedAt
		base.PublishedDate = &pd
	}

	ex, raw, err := runExtraction(ctx, d.LLM, text)
	if err != nil {
		// Corpus items don't queue for manual extraction on LLM failure —
		// the item stays unrecorded and the next scan retries it.
		return err
	}
	if !ex.IsFraudCase {
		st.NotFraudCase++
		if d.DryRun {
			return nil
		}
		base.Payload = raw
		conf := ex.Confidence
		base.ConfidenceScore = &conf
		base.Excerpt = firstChars(text, 400)
		rejected, wasNew, err := d.DB.CreateFraudClaim(ctx, base, nil, "fraudnews")
		if err != nil || !wasNew {
			return err
		}
		_, err = d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: rejected.ID, To: domain.ReviewRejected,
			ReviewerLabel: "fraudnews",
			Note: fmt.Sprintf("auto-rejected: extractor classified as not an enforcement matter (confidence=%.2f)",
				ex.Confidence),
		})
		return err
	}

	base.Payload = raw
	conf := ex.Confidence
	base.ConfidenceScore = &conf
	base.Excerpt = ex.ExactExcerpt
	suppressed, suppressionNote := suppressionScreen(ex, text)
	base.SuppressionFlag = suppressed
	if t := parseISODate(ex.EventDate); t != nil && base.PublishedDate == nil {
		base.PublishedDate = t
	}

	personNames := namedPersons(ex)
	var orgNames []string
	for _, o := range ex.Organisations {
		if strings.TrimSpace(o.Name) != "" {
			orgNames = append(orgNames, o.Name)
		}
	}

	if d.DryRun {
		d.Log.Info("dry-run: would create news claim", "title", it.Title,
			"source", it.Source, "persons", len(personNames), "orgs", len(orgNames),
			"suppression", base.SuppressionFlag)
		st.ClaimsNew++
		return nil
	}

	created, wasNew, err := d.DB.CreateFraudClaim(ctx, base, nil, "fraudnews")
	if err != nil {
		return err
	}
	if !wasNew {
		return nil
	}
	st.ClaimsNew++

	if base.SuppressionFlag {
		if _, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: created.ID, To: domain.ReviewQuarantined,
			ReviewerLabel: "fraudnews",
			Note:          "auto-quarantined: " + suppressionNote,
		}); err != nil {
			d.Log.Warn("auto-quarantine failed", "claim", created.ID, "err", err)
		} else {
			st.Quarantined++
		}
	}

	n, err := d.DB.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: created.ID, PersonNames: personNames, OrgNames: orgNames,
		AmountNZD: ex.TotalAmountNZD, CourtIDs: courtIDs(ex, text),
	})
	if err != nil {
		d.Log.Warn("case match suggestion failed", "claim", created.ID, "err", err)
	}
	st.Suggestions += n
	return nil
}
