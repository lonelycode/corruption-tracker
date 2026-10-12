// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package accesslog is the standalone server's host.AccessAuditor: the
// Privacy-Act record of person-level views and exports goes to
// tracker_access_log, with the same surface and detail strings an embedding
// host receives (design §5.2).
package accesslog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/host"
)

// Auditor writes to tracker_access_log.
type Auditor struct{ Pool *pgxpool.Pool }

var _ host.AccessAuditor = Auditor{}

// LogAccess records one access. The principal's actor is looked up by
// external ref (local users always have one); an unknown ref is still
// logged, by label, rather than dropped.
func (a Auditor) LogAccess(ctx context.Context, p host.Principal, entitlement, surface, detail string) error {
	var actorID *int64
	var id int64
	err := a.Pool.QueryRow(ctx, `SELECT id FROM tracker_actors WHERE external_ref = $1`, p.ExternalRef).Scan(&id)
	switch {
	case err == nil:
		actorID = &id
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("access log actor: %w", err)
	}
	if _, err := a.Pool.Exec(ctx, `INSERT INTO tracker_access_log (actor_id, actor_label, entitlement, surface, detail)
		VALUES ($1, $2, $3, $4, $5)`, actorID, p.Label, entitlement, surface, detail); err != nil {
		return fmt.Errorf("access log: %w", err)
	}
	return nil
}
