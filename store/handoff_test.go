// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func isReadOnly(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "25006"
}

// While frozen the database refuses every dataset write, from any code
// path; reads and operational tables keep working; abort restores writes.
func TestFreezeGuardsTheDatasetInTheDatabase(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()
	if _, err := d.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "Before freeze", TypologyRaw: "fraud"}}); err != nil {
		t.Fatal(err)
	}
	state, err := d.Freeze(ctx, "test", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != store.ModeFrozen || state.FreezeID == "" || state.FrozenAt == nil || state.FrozenBy != "test" {
		t.Fatalf("state after freeze = %+v", state)
	}
	if _, err := d.Freeze(ctx, "again", time.Second); !errors.Is(err, store.ErrFrozen) {
		t.Fatalf("second freeze = %v, want ErrFrozen", err)
	}

	_, err = d.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "During freeze", TypologyRaw: "fraud"}})
	if !isReadOnly(err) {
		t.Fatalf("insert while frozen = %v, want read_only_sql_transaction", err)
	}
	for _, q := range []string{
		`UPDATE fraud_cases SET case_name = 'x'`,
		`DELETE FROM fraud_case_events`,
		`TRUNCATE fraud_persons CASCADE`,
		`INSERT INTO tracker_actors (external_ref, display_label) VALUES ('psql:op', 'op')`,
	} {
		if _, err := d.Pool.Exec(ctx, q); !isReadOnly(err) {
			t.Errorf("%s while frozen = %v, want read_only_sql_transaction", q, err)
		}
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_cases`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read while frozen = %d %v", n, err)
	}
	// Operational tables stay writable.
	for _, q := range []string{
		`INSERT INTO tracker_job_runs (job, started_at, status) VALUES ('j', now(), 'skipped_paused')`,
		`INSERT INTO tracker_access_log (surface) VALUES ('test')`,
		`UPDATE tracker_instance SET ingestion_paused = ingestion_paused`,
	} {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Errorf("%s while frozen: %v", q, err)
		}
	}
	// The importer's bypass.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL corruption_tracker.allow_frozen_write = 'on'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fraud_persons (full_name) VALUES ('Bypass Probe')`); err != nil {
		t.Errorf("bypassed insert: %v", err)
	}
	_ = tx.Rollback(ctx)

	if err := d.AbortHandoff(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "After abort", TypologyRaw: "fraud"}}); err != nil {
		t.Fatalf("write after abort: %v", err)
	}
	if st, _ := d.GetInstanceState(ctx); st.Mode != store.ModeNormal || st.FreezeID != "" {
		t.Errorf("state after abort = %+v", st)
	}
}

// Every dataset table carries the guard; operational tables don't.
func TestEveryDatasetTableIsGuarded(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()
	for _, name := range bundle.TableSet {
		var exists, guarded bool
		if err := d.Pool.QueryRow(ctx, `
			SELECT to_regclass($1) IS NOT NULL,
			       EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass($1) AND tgname = $1 || '_write_guard')`,
			name).Scan(&exists, &guarded); err != nil {
			t.Fatal(err)
		}
		if !exists {
			continue
		}
		if want := name != "tracker_job_runs"; guarded != want {
			t.Errorf("%s: write guard = %v, want %v", name, guarded, want)
		}
	}
}

func TestCompleteHandoffRules(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()
	if err := d.CompleteHandoff(ctx, "x", "https://next.example"); err == nil {
		t.Fatal("complete without a freeze succeeded")
	}
	if _, err := d.Freeze(ctx, "test", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteHandoff(ctx, "00000000-0000-4000-8000-000000000001", "https://next.example"); err == nil ||
		!strings.Contains(err.Error(), "export --handoff") {
		t.Fatalf("complete without a handoff export = %v", err)
	}
	id := bundle.NewID()
	if err := d.RecordHandoffExport(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteHandoff(ctx, bundle.NewID(), "https://next.example"); err == nil {
		t.Fatal("complete with the wrong export id succeeded")
	}
	if err := d.CompleteHandoff(ctx, id, "http://next.example"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("complete with an http successor = %v", err)
	}
	if st, _ := d.GetInstanceState(ctx); st.Mode != store.ModeFrozen {
		t.Fatalf("mode after a refused successor = %s, want frozen", st.Mode)
	}
	if err := d.CompleteHandoff(ctx, id, "https://next.example"); err != nil {
		t.Fatal(err)
	}
	st, _ := d.GetInstanceState(ctx)
	if st.Mode != store.ModeHandedOff || st.SuccessorURL != "https://next.example" {
		t.Fatalf("state after complete = %+v", st)
	}
	if err := d.AbortHandoff(ctx); err == nil {
		t.Fatal("abort of a handed-off instance succeeded")
	}
	if _, err := d.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "x", TypologyRaw: "fraud"}}); !isReadOnly(err) {
		t.Fatalf("write after handoff = %v, want read-only", err)
	}
	if paused, err := d.SchedulerPaused(ctx); err != nil || !paused {
		t.Errorf("scheduler paused after handoff = %v %v", paused, err)
	}
}

// ResumeAfterImport stamps the resume, so PendingResume is false after it
// and a plain unpause stays plain from then on.
func TestResumeAfterImportStampsResume(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()
	if _, err := d.EnsureInstance(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE tracker_instance SET ingestion_paused = true,
		imported_from = '{"instance_id":"src","export_id":"exp","freeze_id":null}'`); err != nil {
		t.Fatal(err)
	}
	st, _ := d.GetInstanceState(ctx)
	if !st.PendingResume() {
		t.Fatal("not pending before resume")
	}
	if err := d.ResumeAfterImport(ctx, []string{"agency:x"}); err != nil {
		t.Fatal(err)
	}
	st, _ = d.GetInstanceState(ctx)
	if st.PendingResume() || st.ImportedFrom == nil || st.ImportedFrom.ResumedAt == nil || st.IngestionPaused ||
		st.ImportedFrom.ExportID != "exp" {
		t.Fatalf("after resume: %+v from %+v", st, st.ImportedFrom)
	}
	// No freeze time in this import (not a handoff bundle): nothing armed.
	if cu, _ := d.GetConnectorCatchUp(ctx, "agency:x"); cu.CatchUp {
		t.Error("catch-up armed without a freeze time")
	}
	if fresh, _ := storetest.Open(t).GetInstanceState(ctx); fresh.PendingResume() {
		t.Error("a never-imported instance reports a pending resume")
	}
}

func TestCheckSuccessorURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://next.example.org":              true,
		"https://next.example.org/tracker?x=1":  true,
		"HTTPS://next.example.org":              true,
		"https://localhost:8443":                true,
		"http://next.example.org":               false,
		"https://":                              false,
		"https:next.example.org":                false,
		"https://operator@tracker.invalid":      false,
		"//next.example.org":                    false,
		"next.example.org":                      false,
		"javascript:alert(1)":                   false,
		"":                                      false,
		"https://next.example.org/\n\tsmuggled": false,
	} {
		if err := store.CheckSuccessorURL(raw); (err == nil) != ok {
			t.Errorf("CheckSuccessorURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}
