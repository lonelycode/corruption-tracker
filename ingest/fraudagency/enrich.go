// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
)

// Imported-case enrichment backfill. The spreadsheet rows carried a single
// conviction date + penalty, so the 200-odd imported cases have one-event
// timelines and no offending periods. Each linked source article (the row's
// original Link) is re-fetched and run through the standard extractor; the
// result enters the review queue as a claim whose case attachment is
// PRE-CONFIRMED — the URL was bound to its case at import time, so the
// binding is provenance identity, not a fuzzy match. Content verification
// (and the promote that attaches richer events / offending periods) remains
// a reviewer decision, and the queue's bulk-approve handles these.
//
// Per-domain robots.txt is checked once per run and respected; a disallowed
// or unfetchable page is skipped and counted, never retried in-run.

// EnrichStats summarises a backfill run.
type EnrichStats struct {
	Targets       int
	SkippedKnown  int // URL already has a claim
	RobotsBlocked int
	FetchErrors   int
	NotFraudCase  int // extractor disagreed (page changed/paywalled stub)
	ClaimsNew     int
	Errors        int
}

// EnrichImported runs the backfill over targets.
func (d *Deps) EnrichImported(ctx context.Context, targets []store.FraudEnrichTarget) (EnrichStats, error) {
	var st EnrichStats
	st.Targets = len(targets)
	if d.LLM == nil {
		return st, errors.New("fraud enrich-imported requires an LLM completer")
	}
	if d.Fetcher == nil {
		d.Fetcher = NewFetcher()
	}
	if d.Delay == 0 {
		d.Delay = 2 * time.Second
	}

	robotsCache := map[string]string{} // host -> robots body ("" = none/404)
	for _, t := range targets {
		key := "url:" + t.URL
		exists, err := d.claimKeyExists(ctx, key)
		if err != nil {
			return st, err
		}
		if exists {
			st.SkippedKnown++
			continue
		}

		allowed, err := d.enrichRobotsAllowed(ctx, robotsCache, t.URL)
		if err != nil || !allowed {
			if err != nil {
				d.Log.Warn("enrich robots check failed", "url", t.URL, "err", err)
			}
			st.RobotsBlocked++
			continue
		}

		if err := d.enrichOne(ctx, t, key, &st); err != nil {
			st.Errors++
			d.Log.Warn("enrich target failed", "case", t.CaseID, "url", t.URL, "err", err)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(d.Delay):
		}
	}
	return st, nil
}

func (d *Deps) enrichRobotsAllowed(ctx context.Context, cache map[string]string, rawURL string) (bool, error) {
	return RobotsAllowedForURL(ctx, d.Fetcher, cache, rawURL)
}

// RobotsAllowedForURL checks an arbitrary URL against its host's robots.txt
// (per-host body cached in cache; "" = none/404 = default-allow). Shared by
// the enrichment and recode passes, which fetch case-bound article URLs
// across many news domains.
func RobotsAllowedForURL(ctx context.Context, f *Fetcher, cache map[string]string, rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false, fmt.Errorf("bad url %q", rawURL)
	}
	body, seen := cache[u.Host]
	if !seen {
		fetched, status, err := f.get(ctx, u.Scheme+"://"+u.Host+"/robots.txt")
		switch {
		case err != nil:
			return false, err
		case status == 404 || status == 410:
			fetched = "" // no robots = default-allow
		case status != 200:
			return false, fmt.Errorf("robots status %d for %s", status, u.Host)
		}
		body = fetched
		cache[u.Host] = body
	}
	if body == "" {
		return true, nil
	}
	return !robotsDisallows(body, u.Path, f.Crawler.Token()), nil
}

func (d *Deps) enrichOne(ctx context.Context, t store.FraudEnrichTarget, dedupeKey string, st *EnrichStats) error {
	html, err := d.Fetcher.Get(ctx, t.URL)
	if err != nil {
		st.FetchErrors++ // dead link / paywall / 404 — counted, not retried
		d.Log.Debug("enrich fetch failed", "url", t.URL, "err", err)
		return nil
	}
	text := HTMLToText(html)
	title := pageTitle(html)
	if title == "" {
		title = t.URL
	}

	ex, raw, err := runExtraction(ctx, d.LLM, text)
	if err != nil {
		return err
	}
	if !ex.IsFraudCase {
		// Page moved, paywalled stub, or generic landing — nothing usable.
		st.NotFraudCase++
		return nil
	}

	sum := sha256.Sum256([]byte(text))
	hash := "enrich:" + hex.EncodeToString(sum[:])
	host := ""
	if u, err := url.Parse(t.URL); err == nil {
		host = u.Host
	}
	caseID := t.CaseID
	conf := ex.Confidence
	claim := domain.FraudClaim{
		// Provenance identity: this URL was bound to the case at import, so
		// the attachment is pre-confirmed; reviewers verify content only.
		CaseID:          &caseID,
		MatchStatus:     domain.MatchConfirmed,
		Payload:         raw,
		Headline:        fmt.Sprintf("[enrich case #%d] %s", t.CaseID, title),
		ConfidenceScore: &conf,
		SourceType:      domain.FraudSourceNews,
		SourceURL:       t.URL,
		SourcePublisher: strings.TrimPrefix(host, "www."),
		LicenceBasis:    newsLicenceBasis,
		PublishedDate:   parseISODate(ex.EventDate),
		Excerpt:         ex.ExactExcerpt,
		ContentHash:     hash,
		DedupeKey:       &dedupeKey,
	}
	claim.SuppressionFlag, _ = suppressionScreen(ex, text)

	if d.DryRun {
		d.Log.Info("dry-run: would create enrichment claim", "case", t.CaseID,
			"title", title, "offending_start", ex.OffendingPeriod.Start)
		st.ClaimsNew++
		return nil
	}
	_, wasNew, err := d.DB.CreateFraudClaim(ctx, claim, nil, "fraudenrich")
	if err != nil {
		return err
	}
	if wasNew {
		st.ClaimsNew++
	}
	return nil
}
