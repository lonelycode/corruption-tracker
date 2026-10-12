// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"fmt"
	"time"
)

// Admin-surface queries for the fraud tracker: queue/dashboard counts and
// the HITL appeal-check worklist.

// CountFraudClaimsByState returns live-claim counts keyed by review_state.
func (d *Store) CountFraudClaimsByState(ctx context.Context) (map[string]int, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT review_state, count(*) FROM fraud_claims
		 WHERE deleted_at IS NULL GROUP BY review_state`)
	if err != nil {
		return nil, fmt.Errorf("count claims: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// CountFraudCasesByStatus returns case counts keyed by status, split by
// review_state ("verified"/"unverified" sub-keys as "status|review_state").
func (d *Store) CountFraudCasesByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT status || '|' || review_state, count(*) FROM fraud_cases GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("count cases: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// FraudAppealCheck is one row of the HITL appeal worklist: a case whose
// appeal status should be checked manually (in the reviewer's browser —
// never server-side; JDO/NZLII forbid automated access).
type FraudAppealCheck struct {
	CaseID           int64
	CaseName         string
	Status           string
	Typology         string
	LastEventDate    *time.Time
	LastChecked      *time.Time // most recent recorded check, any outcome
	ParticipantNames []string   // admin-only; access must be logged by caller
}

// FraudAppealWorklist returns cases in appeal_pending, plus convicted cases
// whose latest verdict/sentencing event falls within windowDays (the appeal
// window heuristic) — EXCLUDING cases checked clear within coolDays, so the
// queue shrinks as diligence happens. Ordered never-checked first, then
// stalest check first.
func (d *Store) FraudAppealWorklist(ctx context.Context, windowDays, coolDays int) ([]FraudAppealCheck, error) {
	rows, err := d.Pool.Query(ctx, `
		WITH latest AS (
			SELECT case_id, MAX(event_date) AS last_event
			  FROM fraud_case_events
			 WHERE event_type IN ('verdict','sentencing') AND event_date IS NOT NULL
			 GROUP BY case_id
		), checks AS (
			SELECT case_id, MAX(checked_at) AS last_checked
			  FROM fraud_appeal_checks
			 GROUP BY case_id
		)
		SELECT c.id, c.case_name, c.status, COALESCE(t.label, c.typology_raw), l.last_event,
		       ch.last_checked,
		       COALESCE(array_agg(p.full_name ORDER BY cp.id) FILTER (WHERE p.id IS NOT NULL), ARRAY[]::text[])
		  FROM fraud_cases c
		  LEFT JOIN latest l ON l.case_id = c.id
		  LEFT JOIN checks ch ON ch.case_id = c.id
		  LEFT JOIN fraud_typologies t ON t.id = c.typology_id
		  LEFT JOIN fraud_case_participants cp ON cp.case_id = c.id
		  LEFT JOIN fraud_persons p ON p.id = cp.person_id
		 WHERE (c.status = 'appeal_pending'
		        OR (c.status = 'convicted' AND l.last_event >= now()::date - $1::int))
		   AND (ch.last_checked IS NULL OR ch.last_checked < now() - make_interval(days => $2::int))
		 GROUP BY c.id, t.label, l.last_event, ch.last_checked
		 ORDER BY ch.last_checked NULLS FIRST, l.last_event NULLS FIRST, c.id
		 LIMIT 200`, windowDays, coolDays)
	if err != nil {
		return nil, fmt.Errorf("appeal worklist: %w", err)
	}
	defer rows.Close()
	var out []FraudAppealCheck
	for rows.Next() {
		var a FraudAppealCheck
		if err := rows.Scan(&a.CaseID, &a.CaseName, &a.Status, &a.Typology, &a.LastEventDate, &a.LastChecked, &a.ParticipantNames); err != nil {
			return nil, fmt.Errorf("scan appeal check: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordFraudAppealCheck logs one manual JDO/NZLII check (the diligence
// trail) — outcome 'no_appeal_found' starts the worklist cooling window;
// 'finding_recorded' marks that a manual submission was filed.
func (d *Store) RecordFraudAppealCheck(ctx context.Context, caseID int64, checkedBy *int64, label, outcome, note string) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO fraud_appeal_checks (case_id, checked_by, checked_by_label, outcome, note)
		VALUES ($1,$2,$3,$4,$5)`, caseID, checkedBy, label, outcome, note)
	if err != nil {
		return fmt.Errorf("record appeal check: %w", err)
	}
	return nil
}
