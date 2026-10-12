// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Duplicate-case detection and merge for the corruption & fraud tracker.
//
// The same real-world matter reported by multiple outlets (tvnz / stuff / rnz)
// or polled from more than one agency feed produces separate fraud_cases rows
// that the claim-dedupe never collapsed. They surface as repeated entries in
// the explorer and its CSV export. This file finds near-certain duplicates and
// merges a group down to one winner, re-pointing every child row and salvaging
// any factual field the winner was missing.
//
// Precision over recall by design: a group is only offered for merge when the
// cases share an IDENTICAL, non-empty set of participant persons AND either
// agree on a concrete amount or literally cite the same source URL. "Same
// person, no amount, no shared source" is left for a human — a prolific
// offender genuinely can have two distinct matters.

// FraudDupCase is one case inside a duplicate group, with the signals the
// winner-selection and audit log need.
type FraudDupCase struct {
	ID             int64
	CaseName       string
	ReviewState    string
	NamingApproved bool
	AmountNZD      *float64
	Participants   int
	Sources        int
	Events         int
	Typologies     int
	SourceURLs     []string
}

// score ranks a case for winner selection: verified beats unverified, an
// explicit naming decision is worth keeping, then richer records win, oldest
// (lowest id) breaks ties.
func (c FraudDupCase) score() int {
	s := 0
	if c.ReviewState == "verified" {
		s += 1_000_000
	}
	if c.NamingApproved {
		s += 100_000
	}
	s += 1000 * (c.Sources + c.Events + c.Typologies + c.Participants)
	return s
}

// FraudDuplicateGroup is a confirmed-mergeable set: one winner, one or more
// losers, plus the reason it qualified.
type FraudDuplicateGroup struct {
	PersonIDs []int64
	WinnerID  int64
	LoserIDs  []int64
	Cases     []FraudDupCase
	Reason    string
}

// FindDuplicateFraudCaseGroups returns the high-precision duplicate groups
// described above, winner already chosen. Read-only.
func (d *Store) FindDuplicateFraudCaseGroups(ctx context.Context) ([]FraudDuplicateGroup, error) {
	rows, err := d.Pool.Query(ctx, `
		WITH pp AS (
			SELECT case_id, array_agg(DISTINCT person_id ORDER BY person_id) AS pset
			  FROM fraud_case_participants
			 GROUP BY case_id
		), grp AS (
			SELECT pset, array_agg(case_id ORDER BY case_id) AS cases
			  FROM pp
			 WHERE array_length(pset, 1) >= 1
			 GROUP BY pset
			HAVING count(*) > 1
		)
		SELECT pset, cases FROM grp ORDER BY cases`)
	if err != nil {
		return nil, fmt.Errorf("find duplicate groups: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		persons []int64
		cases   []int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.persons, &c.cases); err != nil {
			return nil, fmt.Errorf("scan duplicate group: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []FraudDuplicateGroup
	for _, c := range candidates {
		cases, err := d.loadDupCases(ctx, c.cases)
		if err != nil {
			return nil, err
		}
		reason, ok := mergeableReason(cases)
		if !ok {
			continue
		}
		winner := cases[0]
		for _, cc := range cases[1:] {
			if cc.score() > winner.score() ||
				(cc.score() == winner.score() && cc.ID < winner.ID) {
				winner = cc
			}
		}
		var losers []int64
		for _, cc := range cases {
			if cc.ID != winner.ID {
				losers = append(losers, cc.ID)
			}
		}
		out = append(out, FraudDuplicateGroup{
			PersonIDs: c.persons, WinnerID: winner.ID, LoserIDs: losers,
			Cases: cases, Reason: reason,
		})
	}
	return out, nil
}

// mergeableReason reports whether a same-person group is safe to auto-merge,
// and why: a shared amount or a shared source URL.
func mergeableReason(cases []FraudDupCase) (string, bool) {
	var amount *float64
	concrete := false
	conflict := false
	for _, c := range cases {
		if c.AmountNZD == nil {
			continue
		}
		concrete = true
		if amount == nil {
			amount = c.AmountNZD
		} else if *amount != *c.AmountNZD {
			conflict = true
		}
	}
	if concrete && !conflict {
		return fmt.Sprintf("shared amount %.0f NZD", *amount), true
	}

	seen := map[string]bool{}
	for _, c := range cases {
		for _, u := range c.SourceURLs {
			if u == "" {
				continue
			}
			if seen[u] {
				return "shared source URL", true
			}
			seen[u] = true
		}
	}
	return "", false
}

func (d *Store) loadDupCases(ctx context.Context, ids []int64) ([]FraudDupCase, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT c.id, c.case_name, c.review_state, c.naming_approved, c.total_amount_nzd,
		       (SELECT count(*) FROM fraud_case_participants p WHERE p.case_id = c.id),
		       (SELECT count(*) FROM fraud_sources s WHERE s.case_id = c.id),
		       (SELECT count(*) FROM fraud_case_events e WHERE e.case_id = c.id),
		       (SELECT count(*) FROM fraud_case_typologies ct WHERE ct.case_id = c.id),
		       COALESCE((SELECT array_agg(s.url) FROM fraud_sources s
		                  WHERE s.case_id = c.id AND s.url <> ''), '{}')
		  FROM fraud_cases c
		 WHERE c.id = ANY($1)
		 ORDER BY c.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("load dup cases: %w", err)
	}
	defer rows.Close()
	var out []FraudDupCase
	for rows.Next() {
		var c FraudDupCase
		if err := rows.Scan(&c.ID, &c.CaseName, &c.ReviewState, &c.NamingApproved, &c.AmountNZD,
			&c.Participants, &c.Sources, &c.Events, &c.Typologies, &c.SourceURLs); err != nil {
			return nil, fmt.Errorf("scan dup case: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FraudMergeResult summarises one merge.
type FraudMergeResult struct {
	WinnerID            int64
	LosersMerged        int
	Participants        int // rows re-pointed onto the winner
	Sources             int
	Events              int
	Typologies          int
	Organisations       int
	Agencies            int
	Claims              int
	MatchCandidates     int
	AppealChecks        int
	CollisionsDiscarded int // duplicate child rows dropped instead of re-pointed
}

// MergeFraudCases folds loserIDs into winnerID in one transaction: every child
// row is re-pointed (case-scoped unique keys deduped, not violated), missing
// factual scalars are salvaged from the losers, the losers are deleted, and the
// winner's derived status is recomputed from its now-merged event set.
func (d *Store) MergeFraudCases(ctx context.Context, winnerID int64, loserIDs []int64) (FraudMergeResult, error) {
	res := FraudMergeResult{WinnerID: winnerID}
	if len(loserIDs) == 0 {
		return res, nil
	}
	if slices.Contains(loserIDs, winnerID) {
		return res, fmt.Errorf("merge: winner %d listed as a loser", winnerID)
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin merge: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock both rows so a concurrent edit can't race the re-point.
	for _, id := range append([]int64{winnerID}, loserIDs...) {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM fraud_cases WHERE id=$1 FOR UPDATE`, id).Scan(&exists); err != nil {
			if err == pgx.ErrNoRows {
				return res, fmt.Errorf("merge: case %d not found", id)
			}
			return res, fmt.Errorf("lock case %d: %w", id, err)
		}
	}

	for _, loser := range loserIDs {
		// Re-point child rows. For tables with a case-scoped unique key, drop
		// the loser's colliding rows first, then move the survivors.
		links := []struct {
			table   string
			keyCols []string
			counter *int
		}{
			{"fraud_case_participants", []string{"person_id"}, &res.Participants},
			{"fraud_case_agencies", []string{"agency_id"}, &res.Agencies},
			{"fraud_case_organisations", []string{"organisation_id", "role"}, &res.Organisations},
			{"fraud_case_typologies", []string{"typology_id"}, &res.Typologies},
			{"fraud_sources", []string{"content_hash"}, &res.Sources},
			{"fraud_case_match_candidates", []string{"claim_id"}, &res.MatchCandidates},
		}
		for _, ln := range links {
			moved, discarded, err := repointScopedTx(ctx, tx, ln.table, ln.keyCols, winnerID, loser)
			if err != nil {
				return res, err
			}
			*ln.counter += moved
			res.CollisionsDiscarded += discarded
		}
		// Tables keyed only by their own id: re-point everything.
		for _, t := range []struct {
			table   string
			counter *int
		}{
			{"fraud_case_events", &res.Events},
			{"fraud_claims", &res.Claims},
			{"fraud_appeal_checks", &res.AppealChecks},
		} {
			ct, err := tx.Exec(ctx,
				`UPDATE `+t.table+` SET case_id=$1 WHERE case_id=$2`, winnerID, loser)
			if err != nil {
				return res, fmt.Errorf("repoint %s: %w", t.table, err)
			}
			*t.counter += int(ct.RowsAffected())
		}

		// Salvage factual scalars the winner is missing. Coding fields
		// (typology/sector/region/agency) are left to the recode pass.
		if _, err := tx.Exec(ctx, `
			UPDATE fraud_cases w SET
			    total_amount_nzd       = COALESCE(w.total_amount_nzd, l.total_amount_nzd),
			    amount_basis           = COALESCE(w.amount_basis, l.amount_basis),
			    offending_period_start = COALESCE(w.offending_period_start, l.offending_period_start),
			    offending_period_end   = COALESCE(w.offending_period_end, l.offending_period_end),
			    offending_period_precision = COALESCE(w.offending_period_precision, l.offending_period_precision),
			    victim_class           = COALESCE(w.victim_class, l.victim_class),
			    victim_count           = COALESCE(w.victim_count, l.victim_count),
			    court_name   = CASE WHEN w.court_name   = '' THEN l.court_name   ELSE w.court_name   END,
			    victim_detail= CASE WHEN w.victim_detail= '' THEN l.victim_detail ELSE w.victim_detail END,
			    summary      = CASE WHEN w.summary      = '' THEN l.summary      ELSE w.summary      END,
			    covid_related = w.covid_related OR l.covid_related,
			    updated_at = now()
			  FROM fraud_cases l
			 WHERE w.id=$1 AND l.id=$2`, winnerID, loser); err != nil {
			return res, fmt.Errorf("salvage scalars: %w", err)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM fraud_cases WHERE id=$1`, loser); err != nil {
			return res, fmt.Errorf("delete loser %d: %w", loser, err)
		}
		res.LosersMerged++
	}

	if _, err := recomputeFraudCaseStatusTx(ctx, tx, winnerID); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit merge: %w", err)
	}
	return res, nil
}

// repointScopedTx moves a link table's loser rows onto the winner, first
// dropping any loser rows that would collide on the case-scoped unique key
// (keyCols). Returns (moved, discarded). The table names and key columns are
// fixed internal constants — never request input — so the fmt.Sprintf is safe.
func repointScopedTx(ctx context.Context, tx pgx.Tx, table string, keyCols []string, winner, loser int64) (moved, discarded int, err error) {
	conds := make([]string, len(keyCols))
	for i, c := range keyCols {
		conds[i] = fmt.Sprintf("w.%s IS NOT DISTINCT FROM l.%s", c, c)
	}
	join := strings.Join(conds, " AND ")
	del, err := tx.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %[1]s l
		 WHERE l.case_id=$2
		   AND EXISTS (SELECT 1 FROM %[1]s w WHERE w.case_id=$1 AND %[2]s)`,
		table, join), winner, loser)
	if err != nil {
		return 0, 0, fmt.Errorf("dedup %s: %w", table, err)
	}
	upd, err := tx.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET case_id=$1 WHERE case_id=$2`, table), winner, loser)
	if err != nil {
		return 0, 0, fmt.Errorf("repoint %s: %w", table, err)
	}
	return int(upd.RowsAffected()), int(del.RowsAffected()), nil
}
