// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"fmt"
	"time"
)

// Instance is the tracker_instance singleton (design
// §11.1).
type Instance struct {
	ID              string
	Mode            string // normal | frozen | handed_off
	IngestionPaused bool
	UpdatedAt       time.Time
}

// EnsureInstance returns the instance row, creating it when missing (a
// fresh migration creates it; test harnesses may truncate it).
func (d *Store) EnsureInstance(ctx context.Context) (Instance, error) {
	var in Instance
	if _, err := d.Pool.Exec(ctx, `INSERT INTO tracker_instance DEFAULT VALUES ON CONFLICT DO NOTHING`); err != nil {
		return in, fmt.Errorf("ensure instance: %w", err)
	}
	err := d.Pool.QueryRow(ctx, `
		SELECT id::text, mode, ingestion_paused, updated_at FROM tracker_instance LIMIT 1`).
		Scan(&in.ID, &in.Mode, &in.IngestionPaused, &in.UpdatedAt)
	if err != nil {
		return in, fmt.Errorf("read instance: %w", err)
	}
	return in, nil
}

// IngestionPaused reports the global ingestion pause (§9.3). A missing
// instance row reads as not paused.
func (d *Store) IngestionPaused(ctx context.Context) (bool, error) {
	var paused bool
	err := d.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT ingestion_paused FROM tracker_instance LIMIT 1), false)`).Scan(&paused)
	if err != nil {
		return false, fmt.Errorf("read ingestion pause: %w", err)
	}
	return paused, nil
}

// SetIngestionPaused sets the global ingestion pause. Pause stops only the
// scheduler: manual CLI runs and UI writes keep working (§9.3).
func (d *Store) SetIngestionPaused(ctx context.Context, paused bool) error {
	if _, err := d.EnsureInstance(ctx); err != nil {
		return err
	}
	if _, err := d.Pool.Exec(ctx, `
		UPDATE tracker_instance SET ingestion_paused = $1, updated_at = now()`, paused); err != nil {
		return fmt.Errorf("set ingestion pause: %w", err)
	}
	return nil
}

// ConnectorState is one tracker_connector_state row.
type ConnectorState struct {
	Connector     string
	Enabled       bool
	LastRunAt     *time.Time
	LastSuccessAt *time.Time
}

// ConnectorStates returns every recorded connector, by name. Connectors
// without a row are enabled.
func (d *Store) ConnectorStates(ctx context.Context) (map[string]ConnectorState, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT connector, enabled, last_run_at, last_success_at FROM tracker_connector_state`)
	if err != nil {
		return nil, fmt.Errorf("list connector state: %w", err)
	}
	defer rows.Close()
	out := map[string]ConnectorState{}
	for rows.Next() {
		var c ConnectorState
		if err := rows.Scan(&c.Connector, &c.Enabled, &c.LastRunAt, &c.LastSuccessAt); err != nil {
			return nil, fmt.Errorf("scan connector state: %w", err)
		}
		out[c.Connector] = c
	}
	return out, rows.Err()
}

// ConnectorEnabled reports whether connector may run (no row: enabled).
func (d *Store) ConnectorEnabled(ctx context.Context, connector string) (bool, error) {
	var enabled bool
	err := d.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM tracker_connector_state WHERE connector = $1), true)`,
		connector).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("read connector %s: %w", connector, err)
	}
	return enabled, nil
}

// SetConnectorEnabled enables or disables one connector.
func (d *Store) SetConnectorEnabled(ctx context.Context, connector string, enabled bool) error {
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO tracker_connector_state (connector, enabled) VALUES ($1, $2)
		ON CONFLICT (connector) DO UPDATE SET enabled = EXCLUDED.enabled`, connector, enabled); err != nil {
		return fmt.Errorf("set connector %s: %w", connector, err)
	}
	return nil
}
