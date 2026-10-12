// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Actor is an entry in the tracker's actor directory: whoever reviewed,
// verified, decided or edited something. Hosts identify their principals by
// ExternalRef ("alitheia:user:42", "local:7", "system:recode").
type Actor struct {
	ExternalRef string
	Label       string
	Email       string // optional
	Kind        string // "user" (default) | "system" | "connector"
}

// EnsureActor returns the actor id for a.ExternalRef, creating the actor on
// first sight and refreshing its label/email otherwise. It is the only way
// actor ids are minted; callers never assume a host id equals an actor id.
func (d *Store) EnsureActor(ctx context.Context, a Actor) (int64, error) {
	if strings.TrimSpace(a.ExternalRef) == "" {
		return 0, errors.New("ensure actor: external ref is required")
	}
	kind := a.Kind
	if kind == "" {
		kind = "user"
	}
	var email any
	if a.Email != "" {
		email = a.Email
	}
	var id int64
	err := d.Pool.QueryRow(ctx, `
		INSERT INTO tracker_actors (external_ref, display_label, email, kind)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (external_ref) DO UPDATE
		   SET display_label = EXCLUDED.display_label,
		       email         = EXCLUDED.email,
		       updated_at    = now()
		 WHERE (tracker_actors.display_label, tracker_actors.email)
		       IS DISTINCT FROM (EXCLUDED.display_label, EXCLUDED.email)
		RETURNING id`, a.ExternalRef, a.Label, email, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The conflict update was skipped (nothing changed): read the id.
		err = d.Pool.QueryRow(ctx, `SELECT id FROM tracker_actors WHERE external_ref = $1`, a.ExternalRef).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("ensure actor %s: %w", a.ExternalRef, err)
	}
	return id, nil
}
