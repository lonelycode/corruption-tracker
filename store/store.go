// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package store is the corruption & fraud tracker's persistence layer: every
// read and write of the tracker's tables goes through it. It depends only on
// a pgx pool and the tracker's own packages.
package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrRowMissing is returned when a requested row does not exist (or is not
// visible to the caller).
var ErrRowMissing = errors.New("row not found")

// Store wraps the pool the tracker tables live in.
type Store struct {
	Pool *pgxpool.Pool
}

// New returns a Store over pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

// nullableTime returns nil for a zero time so COALESCE in SQL falls back to NOW().
func nullableTime(t interface{ IsZero() bool }) any {
	if t.IsZero() {
		return nil
	}
	return t
}
