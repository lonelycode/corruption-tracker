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

// Fraud report snapshot reads — the report page reads ONLY these, never the
// live fraud_* tables. Writes happen in internal/fraudreport (INSERT-only).

// FraudReportPeriod is one selectable issue in the report picker.
type FraudReportPeriod struct {
	Period      string // YYYY | YYYY-Qn | YYYY-Www
	Version     int    // latest version
	GeneratedAt time.Time
	Revised     bool
}

// Kind classifies the period for grouping in pickers.
func (p FraudReportPeriod) Kind() string {
	switch {
	case len(p.Period) == 4:
		return "year"
	case len(p.Period) == 7 && p.Period[5] == 'Q':
		return "quarter"
	default:
		return "week"
	}
}

// ListFraudReportPeriods returns available issues, newest first. Dev builds
// (include_unverified) are only visible when includeDev is set.
func (d *Store) ListFraudReportPeriods(ctx context.Context, includeDev bool) ([]FraudReportPeriod, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT DISTINCT ON (period) period, version, generated_at, version > 1
		  FROM fraud_report_snapshots
		 WHERE ($1 OR NOT include_unverified)
		 ORDER BY period DESC, version DESC`, includeDev)
	if err != nil {
		return nil, fmt.Errorf("list report periods: %w", err)
	}
	defer rows.Close()
	var out []FraudReportPeriod
	for rows.Next() {
		var w FraudReportPeriod
		if err := rows.Scan(&w.Period, &w.Version, &w.GeneratedAt, &w.Revised); err != nil {
			return nil, fmt.Errorf("scan report period: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FraudReportSnapshot is one stored snapshot version.
type FraudReportSnapshot struct {
	Period            string
	Version           int
	Payload           json.RawMessage
	RevisionNote      string
	IncludeUnverified bool
	GeneratedAt       time.Time
}

// GetFraudReportSnapshot returns the latest version for a period. Returns
// ErrRowMissing when the period has no (visible) snapshot.
func (d *Store) GetFraudReportSnapshot(ctx context.Context, period string, includeDev bool) (FraudReportSnapshot, error) {
	var s FraudReportSnapshot
	err := d.Pool.QueryRow(ctx, `
		SELECT period, version, payload, revision_note, include_unverified, generated_at
		  FROM fraud_report_snapshots
		 WHERE period = $1 AND ($2 OR NOT include_unverified)
		 ORDER BY version DESC LIMIT 1`, period, includeDev).
		Scan(&s.Period, &s.Version, &s.Payload, &s.RevisionNote, &s.IncludeUnverified, &s.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrRowMissing
	}
	if err != nil {
		return s, fmt.Errorf("get report snapshot: %w", err)
	}
	return s, nil
}
