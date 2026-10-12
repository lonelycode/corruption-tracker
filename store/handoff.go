// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// Instance modes (tracker_instance.mode, design
// §11.1).
const (
	ModeNormal    = "normal"
	ModeFrozen    = "frozen"
	ModeHandedOff = "handed_off"
)

// advisoryClass is the tracker's advisory-lock class id ("FTKR");
// objid 1 is the freeze write barrier (§11.2).
const (
	advisoryClass   = 0x46544b52
	advisoryBarrier = 1
)

// ErrFrozen is returned by Freeze when the instance is not in normal mode,
// and by checks that refuse work while frozen.
var ErrFrozen = errors.New("corruption-tracker is not in normal mode")

// InstanceState is the handoff state of this instance.
type InstanceState struct {
	ID              string
	Mode            string
	FreezeID        string
	FrozenAt        *time.Time
	FrozenBy        string
	HandoffExportID string
	SuccessorURL    string
	IngestionPaused bool
	// ImportedFrom is set on an instance created from a bundle.
	ImportedFrom *ImportedFrom
}

// ImportedFrom records the bundle an instance was imported from.
type ImportedFrom struct {
	InstanceID string     `json:"instance_id"`
	ExportID   string     `json:"export_id"`
	FreezeID   *string    `json:"freeze_id"`
	FrozenAt   *time.Time `json:"frozen_at,omitempty"`
	// ResumedAt is when ingestion was first resumed after the import (the
	// catch-up was armed then).
	ResumedAt *time.Time `json:"resumed_at,omitempty"`
}

// Writable reports whether dataset writes are allowed.
func (s InstanceState) Writable() bool { return s.Mode == ModeNormal }

// PendingResume reports an instance imported from a bundle whose ingestion
// has never been resumed: the first resume must arm the post-handoff
// catch-up (ResumeAfterImport), whichever page or command starts it.
func (s InstanceState) PendingResume() bool {
	return s.ImportedFrom != nil && s.ImportedFrom.ResumedAt == nil
}

// GetInstanceState reads the instance's handoff state. A missing row (a
// truncated test DB) or a schema without tracker_instance reads as normal.
func (d *Store) GetInstanceState(ctx context.Context) (InstanceState, error) {
	st := InstanceState{Mode: ModeNormal}
	var exists bool
	if err := d.Pool.QueryRow(ctx, `SELECT to_regclass('tracker_instance') IS NOT NULL`).Scan(&exists); err != nil {
		return st, err
	}
	if !exists {
		return st, nil
	}
	var freezeID, frozenBy, exportID, successor *string
	var imported []byte
	err := d.Pool.QueryRow(ctx, `
		SELECT id::text, mode, freeze_id::text, frozen_at, frozen_by, handoff_export_id::text, successor_url,
		       ingestion_paused, imported_from
		  FROM tracker_instance LIMIT 1`).
		Scan(&st.ID, &st.Mode, &freezeID, &st.FrozenAt, &frozenBy, &exportID, &successor, &st.IngestionPaused, &imported)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceState{Mode: ModeNormal}, nil
	}
	if err != nil {
		return st, fmt.Errorf("read instance state: %w", err)
	}
	st.FreezeID, st.FrozenBy, st.HandoffExportID, st.SuccessorURL = deref(freezeID), deref(frozenBy), deref(exportID), deref(successor)
	if len(imported) > 0 {
		var from ImportedFrom
		if err := json.Unmarshal(imported, &from); err == nil {
			st.ImportedFrom = &from
		}
	}
	return st, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Freeze puts the instance into read-only handoff mode (§11.2). It takes
// the write barrier exclusively, so it waits (up to timeout) for every
// in-flight writer transaction to commit; writers that arrive later fail
// at the database guard. by is recorded as frozen_by.
func (d *Store) Freeze(ctx context.Context, by string, timeout time.Duration) (InstanceState, error) {
	if _, err := d.EnsureInstance(ctx); err != nil {
		return InstanceState{}, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return InstanceState{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, timeout.Milliseconds())); err != nil {
		return InstanceState{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, int32(advisoryClass), int32(advisoryBarrier)); err != nil {
		return InstanceState{}, fmt.Errorf("freeze barrier (in-flight writers did not finish within %s): %w", timeout, err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tracker_instance
		   SET mode = 'frozen', freeze_id = gen_random_uuid(), frozen_at = now(), frozen_by = $1,
		       handoff_export_id = NULL, updated_at = now()
		 WHERE mode = 'normal'`, by)
	if err != nil {
		return InstanceState{}, fmt.Errorf("freeze: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return InstanceState{}, ErrFrozen
	}
	if err := tx.Commit(ctx); err != nil {
		return InstanceState{}, err
	}
	return d.GetInstanceState(ctx)
}

// RunningJobs lists jobs still recorded as running or still holding a
// scheduler job lock (§11.4 step 1 drains them). Stale 'running' rows from
// a crashed process show up here too; an operator clears them by aborting
// or by marking them.
func (d *Store) RunningJobs(ctx context.Context) ([]string, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT job FROM tracker_job_runs WHERE status = 'running'
		UNION
		SELECT 'advisory-lock:' || objid::text FROM pg_locks
		 WHERE locktype = 'advisory' AND classid = $1::oid AND objid >= 1001 AND granted
		ORDER BY 1`, int64(advisoryClass))
	if err != nil {
		return nil, fmt.Errorf("running jobs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RecordHandoffExport remembers the bundle taken under the current freeze;
// handoff complete must name it.
func (d *Store) RecordHandoffExport(ctx context.Context, exportID string) error {
	tag, err := d.Pool.Exec(ctx, `
		UPDATE tracker_instance SET handoff_export_id = $1::uuid, updated_at = now() WHERE mode = 'frozen'`, exportID)
	if err != nil {
		return fmt.Errorf("record handoff export: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("the instance is not frozen")
	}
	return nil
}

// CompleteHandoff marks a frozen instance as handed off for good: it stays
// read-only, and every page points to the successor.
func (d *Store) CompleteHandoff(ctx context.Context, exportID, successorURL string) error {
	if err := CheckSuccessorURL(successorURL); err != nil {
		return err
	}
	st, err := d.GetInstanceState(ctx)
	if err != nil {
		return err
	}
	if st.Mode != ModeFrozen {
		return fmt.Errorf("handoff complete needs a frozen instance (mode is %s)", st.Mode)
	}
	if st.HandoffExportID == "" {
		return errors.New("no handoff export was taken under this freeze (export --handoff first)")
	}
	if exportID != st.HandoffExportID {
		return fmt.Errorf("export %s is not the handoff export %s of this freeze", exportID, st.HandoffExportID)
	}
	if _, err := d.Pool.Exec(ctx, `
		UPDATE tracker_instance SET mode = 'handed_off', successor_url = $1, updated_at = now()
		 WHERE mode = 'frozen'`, successorURL); err != nil {
		return fmt.Errorf("complete handoff: %w", err)
	}
	return nil
}

// CheckSuccessorURL accepts only an absolute https URL with a host and no
// credentials. Completing a handoff can't be undone, and every page of the
// handed-off tracker sends readers to this address.
func CheckSuccessorURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" {
		return fmt.Errorf("the successor URL must be an https URL such as https://tracker.example.org (got %q)", raw)
	}
	if u.User != nil {
		return errors.New("the successor URL must not carry a username or password")
	}
	return nil
}

// AbortHandoff returns a frozen instance to normal operation. A bundle
// taken under the aborted freeze is stale.
func (d *Store) AbortHandoff(ctx context.Context) error {
	tag, err := d.Pool.Exec(ctx, `
		UPDATE tracker_instance
		   SET mode = 'normal', freeze_id = NULL, frozen_at = NULL, frozen_by = NULL, handoff_export_id = NULL,
		       updated_at = now()
		 WHERE mode = 'frozen'`)
	if err != nil {
		return fmt.Errorf("abort handoff: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("handoff abort needs a frozen instance (a handed-off instance stays read-only)")
	}
	return nil
}

// ResumeAfterImport starts ingestion on an instance imported from a
// handoff bundle (§11.4 step 4): every connector catches up from 24 h
// before the source froze, so items published around the cutoff are
// fetched (the bundle's dedupe keys stop duplicates), then ingestion is
// unpaused. Without a recorded freeze time the connectors resume normally.
func (d *Store) ResumeAfterImport(ctx context.Context, connectors []string) error {
	st, err := d.GetInstanceState(ctx)
	if err != nil {
		return err
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if st.ImportedFrom != nil && st.ImportedFrom.FrozenAt != nil {
		from := st.ImportedFrom.FrozenAt.Add(-24 * time.Hour)
		for _, c := range connectors {
			if _, err := tx.Exec(ctx, `
				INSERT INTO tracker_connector_state (connector, resume_from, catch_up) VALUES ($1, $2, true)
				ON CONFLICT (connector) DO UPDATE SET resume_from = EXCLUDED.resume_from, catch_up = true`,
				c, from); err != nil {
				return fmt.Errorf("connector %s: %w", c, err)
			}
		}
	}
	// Stamp the resume so later pause/resume cycles don't re-arm catch-up.
	if _, err := tx.Exec(ctx, `
		UPDATE tracker_instance
		   SET ingestion_paused = false, updated_at = now(),
		       imported_from = CASE WHEN imported_from IS NULL THEN NULL
		                            ELSE jsonb_set(imported_from, '{resumed_at}', to_jsonb(now())) END`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ConnectorCatchUp is a connector's pending post-handoff catch-up.
type ConnectorCatchUp struct {
	CatchUp    bool
	ResumeFrom *time.Time
}

// GetConnectorCatchUp reads a connector's catch-up state (none: no row).
func (d *Store) GetConnectorCatchUp(ctx context.Context, connector string) (ConnectorCatchUp, error) {
	var c ConnectorCatchUp
	err := d.Pool.QueryRow(ctx, `
		SELECT catch_up, resume_from FROM tracker_connector_state WHERE connector = $1`, connector).
		Scan(&c.CatchUp, &c.ResumeFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectorCatchUp{}, nil
	}
	if err != nil {
		return c, fmt.Errorf("read connector %s: %w", connector, err)
	}
	return c, nil
}

// ConnectorRan records a connector run; a successful one clears its
// catch-up.
func (d *Store) ConnectorRan(ctx context.Context, connector string, ok bool) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO tracker_connector_state (connector, last_run_at, last_success_at, catch_up)
		VALUES ($1, now(), CASE WHEN $2 THEN now() END, false)
		ON CONFLICT (connector) DO UPDATE SET
		       last_run_at = now(),
		       last_success_at = CASE WHEN $2 THEN now() ELSE tracker_connector_state.last_success_at END,
		       catch_up = CASE WHEN $2 THEN false ELSE tracker_connector_state.catch_up END`, connector, ok)
	if err != nil {
		return fmt.Errorf("record connector %s run: %w", connector, err)
	}
	return nil
}

// SchedulerPaused reports whether scheduled ingestion must not start: the
// operator's pause, or a freeze/handoff (§9.3, §11.3).
func (d *Store) SchedulerPaused(ctx context.Context) (bool, error) {
	paused, err := d.IngestionPaused(ctx)
	if err != nil || paused {
		return paused, err
	}
	st, err := d.GetInstanceState(ctx)
	if err != nil {
		return false, err
	}
	return !st.Writable(), nil
}
