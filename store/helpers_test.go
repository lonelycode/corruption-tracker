// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"testing"

	"github.com/lonelycode/corruption-tracker/store"
)

func strptr(s string) *string { return &s }

// seedReviewer registers an actor to act as a reviewer: reviewer columns
// reference the tracker's own actor directory.
func seedReviewer(t *testing.T, ctx context.Context, d *store.Store, email string) int64 {
	t.Helper()
	id, err := d.EnsureActor(ctx, store.Actor{ExternalRef: "test:" + email, Label: email, Email: email})
	if err != nil {
		t.Fatalf("seed reviewer %s: %v", email, err)
	}
	return id
}
