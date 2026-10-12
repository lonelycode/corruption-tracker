// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/scheduler"
	"github.com/lonelycode/corruption-tracker/store"
)

// Frozen: write routes answer 503 and scheduled jobs skip, reads keep
// working, and the handoff page shows the checklist. Abort restores normal
// operation.
func TestFreezeBlocksWritesAndJobsUntilAbort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c, err := h.s.Store.CreateFraudCase(ctx, store.FraudCaseInput{Case: domain.FraudCase{CaseName: "Frozen case", TypologyRaw: "fraud"}})
	if err != nil {
		t.Fatal(err)
	}

	resp, _ := h.b.post("/admin/handoff/", "/admin/handoff/begin", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("freeze = %d", resp.StatusCode)
	}
	if st, _ := h.s.Store.GetInstanceState(ctx); st.Mode != store.ModeFrozen {
		t.Fatalf("mode after freeze = %s", st.Mode)
	}

	// Reads work, including the case page and the status pages.
	for _, path := range []string{"/tracker/corruption", "/admin/fraud/cases/" + itoa(c.ID), "/admin/ingestion/"} {
		if resp, _ := h.b.get(path); resp.StatusCode != 200 {
			t.Errorf("GET %s while frozen = %d", path, resp.StatusCode)
		}
	}
	// The checklist is on the handoff page.
	_, page := h.b.get("/admin/handoff/")
	for _, want := range []string{"Handoff checklist", "data-transfer agreement", "Abort handoff", "Complete handoff"} {
		if !strings.Contains(page, want) {
			t.Errorf("handoff page while frozen lacks %q", want)
		}
	}
	// Writes are refused with 503 before reaching the handlers.
	for _, path := range []string{"/admin/fraud/cases/" + itoa(c.ID) + "/verify", "/admin/ingestion/pause"} {
		resp, body := h.b.post("/admin/handoff/", path, url.Values{"paused": {"on"}})
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "frozen") {
			t.Errorf("POST %s while frozen = %d %q", path, resp.StatusCode, body)
		}
	}
	// Jobs skip.
	h.tick(t)
	if h.ran("recode")+h.ran("mail") != 0 {
		t.Fatalf("jobs ran while frozen: %v", h.runs)
	}
	var last string
	_ = h.s.Pool.QueryRow(ctx, `SELECT status FROM tracker_job_runs ORDER BY id DESC LIMIT 1`).Scan(&last)
	if last != scheduler.StatusSkippedPaused {
		t.Errorf("last job status while frozen = %s", last)
	}
	// An export taken now is the handoff bundle.
	_, page = h.b.get("/admin/ingestion/")
	m := csrfField.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token")
	}
	eresp, err := h.b.c.PostForm(h.b.base+"/admin/ingestion/export", url.Values{"csrf_token": {m[1]}})
	if err != nil {
		t.Fatal(err)
	}
	eresp.Body.Close()
	if eresp.StatusCode != 200 {
		t.Fatalf("export while frozen = %d", eresp.StatusCode)
	}
	if st, _ := h.s.Store.GetInstanceState(ctx); st.HandoffExportID == "" {
		t.Error("export while frozen did not record the handoff export")
	}

	// Completing is irreversible, so a successor that isn't https is refused
	// and the tracker stays frozen.
	st, _ := h.s.Store.GetInstanceState(ctx)
	resp, _ = h.b.post("/admin/handoff/", "/admin/handoff/complete", url.Values{
		"export_id": {st.HandoffExportID}, "successor_url": {"http://next.example.org"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("complete with an http successor = %d", resp.StatusCode)
	}
	if st, _ := h.s.Store.GetInstanceState(ctx); st.Mode != store.ModeFrozen {
		t.Fatalf("mode after an http successor = %s, want frozen", st.Mode)
	}
	if _, page := h.b.get("/admin/handoff/"); !strings.Contains(page, "must be an https URL") {
		t.Error("the handoff page does not say why the http successor was refused")
	}

	// Abort: writes and jobs are back.
	if resp, _ := h.b.post("/admin/handoff/", "/admin/handoff/abort", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("abort = %d", resp.StatusCode)
	}
	if resp, _ := h.b.post("/admin/ingestion/", "/admin/ingestion/pause", url.Values{"paused": {"off"}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("POST after abort = %d", resp.StatusCode)
	}
	h.clock.add(time.Minute)
	h.tick(t)
	if h.ran("recode") == 0 {
		t.Error("jobs did not resume after abort")
	}
	var audits int
	_ = h.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_access_log WHERE surface IN ('handoff_begin', 'handoff_abort')`).Scan(&audits)
	if audits != 2 {
		t.Errorf("audited handoff steps = %d, want 2", audits)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// On a freshly imported instance the plain "Resume ingestion" on the
// Ingestion page arms the handoff catch-up too: resuming
// there must not silently skip items published around the cutoff. Later
// pause/resume cycles don't re-arm it.
func TestIngestionResumeAfterImportArmsCatchUp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	frozenAt := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	if _, err := h.s.Pool.Exec(ctx, `
		UPDATE tracker_instance SET ingestion_paused = true,
		       imported_from = jsonb_build_object('instance_id', 'src', 'export_id', 'exp', 'freeze_id', 'frz',
		                                          'frozen_at', $1::timestamptz)`, frozenAt); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.s.Store.GetInstanceState(ctx); !st.PendingResume() {
		t.Fatalf("imported instance not pending resume: %+v", st)
	}

	if resp, _ := h.b.post("/admin/ingestion/", "/admin/ingestion/pause", url.Values{"paused": {"off"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("resume = %d", resp.StatusCode)
	}
	st, err := h.s.Store.GetInstanceState(ctx)
	if err != nil || st.IngestionPaused || st.PendingResume() {
		t.Fatalf("after resume: %+v %v", st, err)
	}
	want := frozenAt.Add(-24 * time.Hour)
	for _, c := range cli.Connectors() {
		cu, err := h.s.Store.GetConnectorCatchUp(ctx, c)
		if err != nil || !cu.CatchUp || cu.ResumeFrom == nil || !cu.ResumeFrom.Equal(want) {
			t.Errorf("%s after Ingestion resume: %+v %v, want catch-up from %s", c, cu, err, want)
		}
	}
	// The Handoff page no longer offers the import resume.
	if _, page := h.b.get("/admin/handoff/"); strings.Contains(page, "This instance was imported") {
		t.Error("handoff page still offers the post-import resume")
	}

	// A connector finishes its catch-up; a later pause/resume leaves it so.
	first := cli.Connectors()[0]
	if err := h.s.Store.ConnectorRan(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	h.b.post("/admin/ingestion/", "/admin/ingestion/pause", url.Values{"paused": {"on"}})
	h.b.post("/admin/ingestion/", "/admin/ingestion/pause", url.Values{"paused": {"off"}})
	if cu, _ := h.s.Store.GetConnectorCatchUp(ctx, first); cu.CatchUp {
		t.Error("a later resume re-armed the catch-up")
	}
	if st, _ := h.s.Store.GetInstanceState(ctx); st.IngestionPaused {
		t.Error("later resume left ingestion paused")
	}
}
