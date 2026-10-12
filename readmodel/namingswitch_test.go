// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// The instance naming switch (standalone, off until an admin turns it on)
// withholds names on every reader surface (case page,
// explorer and its search, digest, report snapshot) for a case the per-case
// rule would name. A nil switch (alitheia) leaves the per-case rule alone.
func TestNamingSwitchWithholdsEverywhere(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	actor, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "local:1", Label: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	c, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Switch case", TypologyRaw: "theft by employee",
			Summary: "Riley Switchfixture took client funds."},
		Participants: []store.FraudParticipantInput{{PersonName: "Riley Switchfixture",
			Participant: domain.FraudCaseParticipant{Role: "defendant"}}},
		Events: []domain.FraudCaseEvent{{EventType: domain.EventSentencing, EventDate: &when}},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceAgencyRelease, Publisher: "Agency",
			URL: "https://agency.example.org/release", ContentHash: "switch-seed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE fraud_cases SET status = 'convicted' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.VerifyFraudCase(ctx, c.ID, actor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&fraudreport.Builder{DB: st}).Build(ctx, "2026", ""); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/tracker/corruption", nil)
	auth := fakeAuth{signedIn: true, access: host.Access{Admin: true}}
	type seen struct{ casePage, explorer, search, digest, snapshot bool }
	look := func(enabled func(context.Context) bool) seen {
		t.Helper()
		v, err := New(Options{Store: st, Auth: auth, NamingEnabled: enabled}).For(r)
		if err != nil {
			t.Fatal(err)
		}
		var s seen
		cs, err := v.Case(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		s.casePage = len(cs.Names) > 0 || cs.Summary != ""
		page, err := v.Explorer(ctx, ExplorerFilter{})
		if err != nil || len(page.Rows) != 1 {
			t.Fatalf("explorer = %+v, %v", page, err)
		}
		s.explorer = len(page.Rows[0].Names) > 0 || page.Rows[0].Summary != ""
		hits, err := v.Explorer(ctx, ExplorerFilter{Query: "switchfixture"})
		if err != nil {
			t.Fatal(err)
		}
		s.search = hits.Total > 0
		rows, err := v.Digest(ctx, "2026", "csv")
		if err != nil || len(rows) != 1 {
			t.Fatalf("digest = %+v, %v", rows, err)
		}
		s.digest = len(rows[0].Names) > 0 || rows[0].Summary != ""
		snap, err := v.Snapshot(ctx, "2026")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range snap.Report.NewInPeriod.Convictions {
			if len(e.Names) > 0 || strings.Contains(e.Summary, "Switchfixture") {
				s.snapshot = true
			}
		}
		return s
	}

	all := seen{true, true, true, true, true}
	if got := look(nil); got != all {
		t.Errorf("no switch (alitheia): names on %+v, want every surface", got)
	}
	if got := look(func(context.Context) bool { return true }); got != all {
		t.Errorf("switch on: names on %+v, want every surface", got)
	}
	if got := look(func(context.Context) bool { return false }); got != (seen{}) {
		t.Errorf("switch off: names on %+v, want none", got)
	}
	// The reason shown is the switch, not suppression.
	for _, tc := range []struct {
		enabled func(context.Context) bool
		off     bool
	}{{nil, false}, {func(context.Context) bool { return true }, false}, {func(context.Context) bool { return false }, true}} {
		v, _ := New(Options{Store: st, Auth: auth, NamingEnabled: tc.enabled}).For(r)
		cs, _ := v.Case(ctx, c.ID)
		page, _ := v.Explorer(ctx, ExplorerFilter{})
		if cs.NamingOff != tc.off || len(page.Rows) != 1 || page.Rows[0].NamingOff != tc.off {
			t.Errorf("NamingOff case=%v explorer=%+v, want %v", cs.NamingOff, page.Rows, tc.off)
		}
	}
	// The explorer still finds the case by what isn't identifying.
	v, _ := New(Options{Store: st, Auth: auth, NamingEnabled: func(context.Context) bool { return false }}).For(r)
	if hits, err := v.Explorer(ctx, ExplorerFilter{Query: "theft by employee"}); err != nil || hits.Total != 1 {
		t.Errorf("non-identifying search with naming off = %d, %v; want 1", hits.Total, err)
	}
}
