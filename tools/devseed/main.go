// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Command devseed seeds a LOCAL DEV tracker database with a realistic spread
// of review states for UX testing: claims worked through the review cycle
// (with genuine audit trails — everything goes through the store's guards,
// nothing is SQL-faked), promoted cases, a verified-case register,
// naming-approved examples, and verified snapshots including a "Revised"
// re-issue. Every decision is attributed to a synthetic dev actor
// ("devseed:reviewer"), never a real user.
//
// Run AFTER `fraud import` and at least one `fraud poll` / `classify-news`
// so there is material to work with:
//
//	DATABASE_URL=postgres://user:pass@localhost/dbname go run ./corruption-tracker/tools/devseed
//	go run ./corruption-tracker/tools/devseed sheet-only   # just clear sheet problem rows
//
// It refuses any database that isn't on localhost. Safe-ish to re-run: claim
// and case state changes skip already-processed rows; snapshot re-runs add
// versions. Host-side extras (alitheia's flagged team login for the
// org-gated report view) live in the host: scripts/fraudpoc in alitheia.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/store"
)

// reviewerActor is the synthetic actor every seeded decision is attributed to.
var reviewerActor = store.Actor{ExternalRef: "devseed:reviewer", Label: "Dev Seed Reviewer", Kind: "system"}

const reviewerLabel = "devseed"

func main() {
	if err := run(context.Background(), os.Getenv("DATABASE_URL"), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string, args []string) error {
	if err := requireLocal(dsn); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)
	log := slog.Default()

	if len(args) > 0 && args[0] == "sheet-only" {
		reviewer, err := st.EnsureActor(ctx, reviewerActor)
		if err != nil {
			return err
		}
		return approveSheetClaims(ctx, st, reviewer, log)
	}
	if err := seed(ctx, st, log); err != nil {
		return err
	}
	fmt.Println(`
Dev data seeded. Things to look at:
  /admin/fraud                       dashboard
  /admin/fraud/claims?state=all      queue across every state
  /admin/fraud/cases                 verified register + unverified sign-off tail
  /admin/fraud/appeals               HITL worklist
  /tracker/corruption                report page; weeks incl. a Revised issue`)
	return nil
}

// requireLocal refuses anything but a localhost database: devseed
// fabricates review decisions and must never touch staging or prod.
func requireLocal(dsn string) error {
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to seed a non-local database (host %q)", host)
}

func seed(ctx context.Context, st *store.Store, log *slog.Logger) error {
	reviewer, err := st.EnsureActor(ctx, reviewerActor)
	if err != nil {
		return err
	}
	log.Info("using reviewer", "actor", reviewer)
	if err := workClaims(ctx, st, reviewer, log); err != nil {
		return err
	}
	if err := approveSheetClaims(ctx, st, reviewer, log); err != nil {
		return err
	}
	if err := verifyCases(ctx, st, reviewer, log); err != nil {
		return err
	}
	if err := approveNaming(ctx, st, log); err != nil {
		return err
	}
	return buildSnapshots(ctx, st, log)
}

// workClaims drives pending extracted claims through the review cycle:
// most are verified+promoted, a couple are left mid-flight for state
// variety in the queue UI.
func workClaims(ctx context.Context, st *store.Store, reviewer int64, log *slog.Logger) error {
	claims, err := st.ListFraudClaims(ctx, store.FraudClaimFilter{State: "pending", Limit: 200})
	if err != nil {
		return err
	}
	res, err := fraudagency.LoadResolvers(ctx, st)
	if err != nil {
		return err
	}

	promoted, midFlight := 0, 0
	for _, c := range claims {
		if c.NeedsManualExtraction || len(c.Payload) == 0 {
			continue // spreadsheet problem rows stay pending: that's realistic queue UX
		}
		var ex fraudagency.Extraction
		if json.Unmarshal(c.Payload, &ex) != nil || !ex.IsFraudCase {
			continue
		}

		// Leave a few mid-flight: 2 in_review, 1 needs_more_info.
		if midFlight < 3 {
			if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
				ClaimID: c.ID, To: domain.ReviewInReview, ReviewerID: &reviewer,
				ReviewerLabel: reviewerLabel, Note: "devseed: picked up for review",
			}); err != nil {
				return fmt.Errorf("claim %d start review: %w", c.ID, err)
			}
			if midFlight == 2 {
				if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
					ClaimID: c.ID, To: domain.ReviewNeedsMoreInfo, ReviewerID: &reviewer,
					ReviewerLabel: reviewerLabel, Note: "devseed: waiting on court list confirmation",
				}); err != nil {
					return fmt.Errorf("claim %d needs info: %w", c.ID, err)
				}
			}
			midFlight++
			continue
		}
		if promoted >= 10 {
			break
		}

		// Attachment decision: confirm the top suggestion when one exists,
		// otherwise it becomes a new case.
		cands, err := st.ListFraudCaseMatchCandidates(ctx, c.ID)
		if err != nil {
			return err
		}
		confirmed := false
		for _, cand := range cands {
			if cand.Decision == domain.DecisionPending {
				if err := st.DecideFraudCaseMatch(ctx, c.ID, cand.ID, true, &reviewer, reviewerLabel); err != nil {
					return fmt.Errorf("claim %d confirm match: %w", c.ID, err)
				}
				confirmed = true
				break
			}
		}
		if !confirmed {
			if err := st.SetFraudClaimNoMatch(ctx, c.ID, &reviewer, reviewerLabel); err != nil {
				return fmt.Errorf("claim %d no-match: %w", c.ID, err)
			}
		}

		if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: c.ID, To: domain.ReviewInReview, ReviewerID: &reviewer, ReviewerLabel: reviewerLabel,
		}); err != nil {
			return fmt.Errorf("claim %d start review: %w", c.ID, err)
		}
		if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: c.ID, To: domain.ReviewVerified, ReviewerID: &reviewer,
			ReviewerLabel: reviewerLabel, Note: "devseed: content checked against source", Attestation: true,
		}); err != nil {
			return fmt.Errorf("claim %d verify: %w", c.ID, err)
		}

		fresh, _, _, err := st.GetFraudClaim(ctx, c.ID)
		if err != nil {
			return err
		}
		if fresh.MatchStatus == domain.MatchConfirmed && fresh.CaseID != nil {
			ev, src := fraudagency.EventFromExtraction(&ex, fresh)
			if _, err := st.PromoteFraudClaim(ctx, c.ID, nil, &ev, &src, &reviewer, reviewerLabel); err != nil {
				return fmt.Errorf("claim %d promote(event): %w", c.ID, err)
			}
		} else {
			in := fraudagency.CaseInputFromExtraction(&ex, fresh, res)
			if _, err := st.PromoteFraudClaim(ctx, c.ID, &in, nil, nil, &reviewer, reviewerLabel); err != nil {
				return fmt.Errorf("claim %d promote(new): %w", c.ID, err)
			}
		}
		promoted++
	}
	log.Info("claims worked", "promoted", promoted, "left_mid_flight", midFlight)
	return nil
}

// approveSheetClaims clears the spreadsheet problem-row claims out of the
// pending queue: their parseable siblings were imported as cases directly,
// so these rows are verified (approved) with a note rather than left to
// drown fresh connector items. They are NOT promoted — there is nothing
// machine-extractable to promote; the original row JSON stays on the claim
// for anyone who wants to hand-build a case from it later.
func approveSheetClaims(ctx context.Context, st *store.Store, reviewer int64, log *slog.Logger) error {
	rows, err := st.Pool.Query(ctx, `
		SELECT id FROM fraud_claims
		 WHERE deleted_at IS NULL AND review_state='pending'
		   AND source_publisher='Fraud/Bribery/Corruption spreadsheet'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := st.SetFraudClaimNoMatch(ctx, id, &reviewer, reviewerLabel); err != nil {
			return fmt.Errorf("sheet claim %d no-match: %w", id, err)
		}
		if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: id, To: domain.ReviewInReview, ReviewerID: &reviewer, ReviewerLabel: reviewerLabel,
		}); err != nil {
			return fmt.Errorf("sheet claim %d review: %w", id, err)
		}
		if _, err := st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: id, To: domain.ReviewVerified, ReviewerID: &reviewer, ReviewerLabel: reviewerLabel,
			Note:        "spreadsheet problem row: underlying dataset already imported as cases; approved to clear the queue (row JSON retained for manual case-building)",
			Attestation: true,
		}); err != nil {
			return fmt.Errorf("sheet claim %d verify: %w", id, err)
		}
	}
	log.Info("sheet claims approved out of pending", "count", len(ids))
	return nil
}

// verifyCases signs off most cases (so verified-only snapshots have data)
// while leaving a tail unverified so the sign-off queue UX is testable.
func verifyCases(ctx context.Context, st *store.Store, reviewer int64, log *slog.Logger) error {
	cases, err := st.ListFraudCases(ctx, store.FraudCaseFilter{ReviewState: "unverified", Limit: 500})
	if err != nil {
		return err
	}
	verified := 0
	for _, c := range cases {
		if c.ID%9 == 0 { // keep ~11% unverified for the sign-off page
			continue
		}
		if _, err := st.VerifyFraudCase(ctx, c.ID, reviewer); err != nil {
			return fmt.Errorf("verify case %d: %w", c.ID, err)
		}
		verified++
	}
	log.Info("cases verified", "verified", verified, "left_unverified", len(cases)-verified)
	return nil
}

// approveNaming flips naming on a handful of verified, decided, unflagged
// cases so the report shows the named-case rendering path too.
func approveNaming(ctx context.Context, st *store.Store, log *slog.Logger) error {
	rows, err := st.Pool.Query(ctx, `
		SELECT c.id FROM fraud_cases c
		 WHERE c.review_state='verified' AND c.status IN ('convicted','upheld')
		   AND c.typology_id IS NOT NULL AND NOT c.naming_approved
		   AND NOT EXISTS (SELECT 1 FROM fraud_case_participants p
		                    WHERE p.case_id=c.id AND (p.suppression_flag OR p.clean_slate_possible))
		 ORDER BY c.total_amount_nzd DESC NULLS LAST LIMIT 4`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := st.SetFraudCaseNamingApproved(ctx, id, true); err != nil {
			return err
		}
	}
	log.Info("naming approved", "cases", ids)
	return nil
}

// snapshotPeriods are the verified-only issues devseed builds; the last
// week is then re-issued so the Revised marker renders.
var snapshotPeriods = []string{"2025-W31", "2025-W36", "2025-W44", "2026-W18", "2026-W23", "2025", "2026-Q1"}

func buildSnapshots(ctx context.Context, st *store.Store, log *slog.Logger) error {
	b := &fraudreport.Builder{DB: st, Log: log, IncludeUnverified: false}
	for _, p := range snapshotPeriods {
		if _, _, err := b.Build(ctx, p, "data correction: late-arriving event attached to an existing case"); err != nil {
			return fmt.Errorf("snapshot %s: %w", p, err)
		}
	}
	_, v, err := b.Build(ctx, "2026-W23", "revised: appeal outcome recorded against an existing case after first issue")
	if err != nil {
		return fmt.Errorf("snapshot revision: %w", err)
	}
	log.Info("snapshots built", "periods", snapshotPeriods, "revised_2026_W23_version", v)
	return nil
}
