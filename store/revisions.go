// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CaseRevision is one case-edit audit record for fraud_case_revisions
// (append-only). Case edits write it in the same transaction as the edit.
type CaseRevision struct {
	CaseID      int64
	FieldName   string // "" for whole-case edits
	BeforeJSON  []byte // nil => '{}'
	AfterJSON   []byte // nil => '{}'
	ActorID     *int64 // tracker_actors.id; nil when not attributable
	ActorKind   string // "admin" | "ingester" | "system"
	ActorSource string // connector name when ActorKind == "ingester"
	ChangeKind  string // "update" (default) | "create" | "delete" | ...
	Note        string
}

// CaseRevisionRow is one row of a case's history, decoded for display. Field
// names match the shared admin history partial.
type CaseRevisionRow struct {
	ID          int64
	CaseID      int64
	FieldName   string
	Before      any
	After       any
	ActorID     *int64
	ActorLabel  string
	ActorKind   string
	ActorSource string
	ChangeKind  string
	Note        string
	CreatedAt   time.Time
}

func (d *Store) writeCaseRevisionTx(ctx context.Context, tx pgx.Tx, r CaseRevision) error {
	if r.CaseID == 0 {
		return errors.New("case revision: case id is required")
	}
	if r.ActorKind == "" {
		return errors.New("case revision: actor kind is required")
	}
	if r.ChangeKind == "" {
		r.ChangeKind = "update"
	}
	before, after := r.BeforeJSON, r.AfterJSON
	if len(before) == 0 {
		before = []byte("{}")
	}
	if len(after) == 0 {
		after = []byte("{}")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fraud_case_revisions
		  (case_id, field_name, before_json, after_json, actor_id, actor_kind,
		   actor_source, change_kind, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		r.CaseID, r.FieldName, before, after, r.ActorID, r.ActorKind,
		r.ActorSource, r.ChangeKind, r.Note)
	if err != nil {
		return fmt.Errorf("insert case revision: %w", err)
	}
	return nil
}

// RecordCaseRevision appends one audit row on its own (for changes made
// outside an edit transaction, e.g. verification and naming toggles).
func (d *Store) RecordCaseRevision(ctx context.Context, r CaseRevision) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := d.writeCaseRevisionTx(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListCaseRevisions returns a case's history, newest first, capped to limit
// (default 200, hard cap 1000).
func (d *Store) ListCaseRevisions(ctx context.Context, caseID int64, limit int) ([]CaseRevisionRow, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT r.id, r.case_id, r.field_name, r.before_json, r.after_json,
		       r.actor_id, COALESCE(a.display_label, ''), r.actor_kind, r.actor_source,
		       r.change_kind, r.note, r.created_at
		  FROM fraud_case_revisions r
		  LEFT JOIN tracker_actors a ON a.id = r.actor_id
		 WHERE r.case_id = $1
		 ORDER BY r.created_at DESC, r.id DESC
		 LIMIT $2`, caseID, limit)
	if err != nil {
		return nil, fmt.Errorf("list case revisions: %w", err)
	}
	defer rows.Close()
	var out []CaseRevisionRow
	for rows.Next() {
		var rev CaseRevisionRow
		var before, after []byte
		if err := rows.Scan(&rev.ID, &rev.CaseID, &rev.FieldName, &before, &after,
			&rev.ActorID, &rev.ActorLabel, &rev.ActorKind, &rev.ActorSource,
			&rev.ChangeKind, &rev.Note, &rev.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan case revision: %w", err)
		}
		if len(before) > 0 {
			if err := json.Unmarshal(before, &rev.Before); err != nil {
				return nil, fmt.Errorf("unmarshal before: %w", err)
			}
		}
		if len(after) > 0 {
			if err := json.Unmarshal(after, &rev.After); err != nil {
				return nil, fmt.Errorf("unmarshal after: %w", err)
			}
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}
