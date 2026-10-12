// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package storetest_test

import (
	"context"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func TestOpenGivesIsolatedMigratedSchemas(t *testing.T) {
	ctx := context.Background()
	a := storetest.Open(t)
	b := storetest.Open(t)

	var schemaA, schemaB string
	if err := a.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schemaA); err != nil {
		t.Fatal(err)
	}
	if err := b.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schemaB); err != nil {
		t.Fatal(err)
	}
	if schemaA == schemaB || schemaA == "public" {
		t.Fatalf("schemas not isolated: %q / %q", schemaA, schemaB)
	}

	// Fully migrated: seeds present, latest tracker tables present.
	var typologies int
	if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_typologies`).Scan(&typologies); err != nil || typologies == 0 {
		t.Fatalf("seeded typologies = %d, err %v", typologies, err)
	}
	if _, err := a.EnsureActor(ctx, storeActor("iso@test")); err != nil {
		t.Fatalf("tracker_actors missing: %v", err)
	}

	// Writes in one schema are invisible in the other.
	if _, err := a.Pool.Exec(ctx, `INSERT INTO fraud_persons (full_name) VALUES ('Only In A')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_persons`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("schema B sees %d persons (err %v), want 0", n, err)
	}

	// TruncateAll empties data tables but keeps the seeded taxonomies.
	if err := storetest.TruncateAll(ctx, a.Pool); err != nil {
		t.Fatal(err)
	}
	if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_persons`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after TruncateAll persons = %d (err %v), want 0", n, err)
	}
	var after int
	if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_typologies`).Scan(&after); err != nil || after != typologies {
		t.Fatalf("TruncateAll touched seeds: %d -> %d (err %v)", typologies, after, err)
	}
}

// TruncateAll works on a frozen instance (the freeze guard refuses a plain
// TRUNCATE) and leaves it in normal mode.
func TestTruncateAllWhileFrozen(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	if _, err := st.Pool.Exec(ctx, `INSERT INTO fraud_persons (full_name) VALUES ('Frozen Probe')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Freeze(ctx, "storetest", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `TRUNCATE fraud_persons CASCADE`); err == nil {
		t.Fatal("plain TRUNCATE succeeded while frozen; this test no longer proves anything")
	}
	if err := storetest.TruncateAll(ctx, st.Pool); err != nil {
		t.Fatalf("TruncateAll while frozen: %v", err)
	}
	state, err := st.GetInstanceState(ctx)
	if err != nil || !state.Writable() {
		t.Fatalf("after TruncateAll: %+v %v, want normal", state, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_persons`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fraud_persons after TruncateAll = %d %v", n, err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO fraud_persons (full_name) VALUES ('After')`); err != nil {
		t.Fatalf("write after TruncateAll: %v", err)
	}
}
