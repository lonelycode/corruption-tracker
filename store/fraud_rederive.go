// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

// StatusRederive is one case whose stored status differs from the status
// its events derive to today.
type StatusRederive struct {
	CaseID   int64
	From, To domain.FraudCaseStatus
}

// RederiveStats summarises a RederiveFraudCaseStatuses run.
type RederiveStats struct {
	Checked    int // non-overridden cases examined
	Overridden int // cases skipped for a manual status override
	Changed    []StatusRederive
	Applied    int // changes written (0 on a dry run)
}

// RederiveFraudCaseStatuses re-runs status derivation over every case and
// reports the ones whose stored status is stale, e.g. after a derivation
// fix. Stored status is otherwise only recomputed when a
// case gets an event write.
//
// Cases with a manual status override are skipped. With apply, each change
// is written in its own transaction with the case row locked, together with
// a status revision (actor "system") through the case-edit audit path, so
// every flip is attributable. Without apply nothing is written.
func (d *Store) RederiveFraudCaseStatuses(ctx context.Context, apply bool, note string) (RederiveStats, error) {
	var st RederiveStats
	rows, err := d.Pool.Query(ctx, `SELECT id FROM fraud_cases ORDER BY id`)
	if err != nil {
		return st, fmt.Errorf("list cases: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return st, fmt.Errorf("scan case id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("list cases: %w", err)
	}

	for _, id := range ids {
		ch, overridden, err := d.rederiveCaseStatus(ctx, id, apply, note)
		if err != nil {
			return st, fmt.Errorf("case %d: %w", id, err)
		}
		if overridden {
			st.Overridden++
			continue
		}
		st.Checked++
		if ch == nil {
			continue
		}
		st.Changed = append(st.Changed, *ch)
		if apply {
			st.Applied++
		}
	}
	return st, nil
}

// rederiveCaseStatus derives one case's status under a row lock and, when
// it differs and apply is set, writes it with its audit revision. A nil
// change means the stored status is current.
func (d *Store) rederiveCaseStatus(ctx context.Context, id int64, apply bool, note string) (*StatusRederive, bool, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var stored string
	var overridden bool
	if err := tx.QueryRow(ctx, `SELECT status, status_overridden FROM fraud_cases WHERE id=$1 FOR UPDATE`, id).
		Scan(&stored, &overridden); err != nil {
		return nil, false, fmt.Errorf("lock case: %w", err)
	}
	if overridden {
		return nil, true, nil
	}
	events, err := listFraudCaseEventsTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	derived := fraudtracker.DeriveCaseStatus(events)
	if string(derived) == stored {
		return nil, false, nil
	}
	ch := &StatusRederive{CaseID: id, From: domain.FraudCaseStatus(stored), To: derived}
	if !apply {
		return ch, false, nil
	}

	if _, err := tx.Exec(ctx, `UPDATE fraud_cases SET status=$2, updated_at=now() WHERE id=$1`, id, string(derived)); err != nil {
		return nil, false, fmt.Errorf("update status: %w", err)
	}
	before, _ := json.Marshal(map[string]any{"status": stored, "status_overridden": false})
	after, _ := json.Marshal(map[string]any{"status": string(derived), "status_overridden": false})
	if err := d.writeCaseRevisionTx(ctx, tx, CaseRevision{
		CaseID:     id,
		FieldName:  "status",
		BeforeJSON: before,
		AfterJSON:  after,
		ActorKind:  "system",
		ChangeKind: "update",
		Note:       note,
	}); err != nil {
		return nil, false, fmt.Errorf("revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return ch, false, nil
}
