// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
)

func TestTierFor(t *testing.T) {
	cases := []struct {
		in   host.Access
		want Tier
	}{
		{host.Access{}, TierNone},
		{host.Access{View: true}, TierViewer},
		{host.Access{Export: true}, TierExport}, // export implies view
		{host.Access{View: true, Export: true}, TierExport},
		{host.Access{Operate: true}, TierOperator}, // operators imply export
		{host.Access{Admin: true}, TierOperator},
	}
	for _, tc := range cases {
		if got := TierFor(tc.in); got != tc.want {
			t.Errorf("TierFor(%+v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// fakeAuth lets a test pick the principal and entitlements.
type fakeAuth struct {
	signedIn bool
	access   host.Access
}

func (a fakeAuth) Principal(*http.Request) (host.Principal, bool) {
	return host.Principal{ExternalRef: "test:1", Label: "t@x"}, a.signedIn
}
func (a fakeAuth) Access(context.Context, *http.Request) (host.Access, error) { return a.access, nil }
func (a fakeAuth) LoginURL(*http.Request) string                              { return "/login" }

// Anonymous requesters are clamped to the public tier whatever the
// Authorizer claims, and an entitled-less signed-in requester gets nothing.
func TestForClampsAnonymous(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/tracker/corruption?x=1", nil)
	v, err := New(Options{Auth: fakeAuth{signedIn: false, access: host.Access{Admin: true}}}).For(r)
	if err != nil || v.Tier() != TierPublic || v.SignedIn() {
		t.Fatalf("anon: tier=%v signedIn=%v err=%v, want public", v.Tier(), v.SignedIn(), err)
	}
	v, _ = New(Options{Auth: fakeAuth{signedIn: true}}).For(r)
	if v.Tier() != TierNone {
		t.Fatalf("unentitled: tier=%v, want none", v.Tier())
	}
	ctx := context.Background()
	if _, err := v.Snapshot(ctx, "2024-W23"); err != ErrForbidden {
		t.Errorf("unentitled snapshot err=%v, want ErrForbidden", err)
	}
	if _, err := v.Case(ctx, 1); err != ErrForbidden {
		t.Errorf("unentitled case err=%v, want ErrForbidden", err)
	}
	v, _ = New(Options{Auth: fakeAuth{signedIn: true, access: host.Access{View: true}}}).For(r)
	if _, err := v.Explorer(ctx, ExplorerFilter{}); err != ErrForbidden {
		t.Errorf("viewer explorer err=%v, want ErrForbidden", err)
	}
	if _, err := v.Digest(ctx, "2024-W23", "csv"); err != ErrForbidden {
		t.Errorf("viewer digest err=%v, want ErrForbidden", err)
	}
}

func TestStripPayloadRemovesPersonLevelFields(t *testing.T) {
	p := fraudreport.Payload{NewInPeriod: fraudreport.NewInPeriod{
		Convictions: []fraudreport.CaseEntry{{CaseID: 1, Label: "Tax fraud", Names: []string{"A Person"},
			Summary: "A Person did it", EventDetail: "A Person: 2 years"}},
		ChargesLaid:    []fraudreport.CaseEntry{{CaseID: 2, Names: []string{"B"}}},
		AppealOutcomes: []fraudreport.CaseEntry{{CaseID: 3, Summary: "C"}},
	}}
	stripPayload(&p)
	for _, list := range [][]fraudreport.CaseEntry{p.NewInPeriod.Convictions, p.NewInPeriod.ChargesLaid, p.NewInPeriod.AppealOutcomes} {
		for _, e := range list {
			if len(e.Names) > 0 || e.Summary != "" || e.EventDetail != "" {
				t.Errorf("case %d kept person-level data: %+v", e.CaseID, e)
			}
		}
	}
	if p.NewInPeriod.Convictions[0].Label != "Tax fraud" {
		t.Error("non-identifying fields must survive")
	}
}

func TestParticipantViewWithholdsIdentity(t *testing.T) {
	typ, out := "home_detention", "convicted"
	months := 10
	p := domain.FraudCaseParticipant{
		PersonName: "Vera", ProfessionRaw: "director", IndividualCharges: "12 x fraud",
		Outcome: &out, SentenceType: &typ, SentenceMonths: &months, SentenceDetail: "Vera got 10 months",
	}
	got := participantView(p, false)
	if got.Name != "" || got.Profession != "" || strings.Contains(got.Sentence, "Vera") {
		t.Errorf("withheld participant leaked identity: %+v", got)
	}
	if got.Charges != "12 x fraud" || got.Outcome != "convicted" || got.Sentence != "home detention · 10 months" {
		t.Errorf("analytical fields lost: %+v", got)
	}
	if named := participantView(p, true); named.Name != "Vera" || !strings.Contains(named.Sentence, "Vera got 10 months") {
		t.Errorf("named participant incomplete: %+v", named)
	}
}

func TestExplorerRowBlanksUnnamed(t *testing.T) {
	row := store.FraudExplorerRow{ID: 7, Status: "charges_laid", NamingOK: false, Summary: "Names here",
		Participants: []store.FraudExplorerParticipant{
			{Name: "Hidden", Profession: "lawyer", Charges: "theft", SentenceType: "fine"},
			{Name: "Hidden2", Charges: "Theft"},
		}}
	got := explorerRow(row)
	if got.Summary != "" || len(got.Names) != 0 || !got.Alleged {
		t.Errorf("unnamed row leaked: %+v", got)
	}
	for _, p := range got.Participants {
		if p.Name != "" || p.Profession != "" {
			t.Errorf("participant leaked: %+v", p)
		}
	}
	if got.ChargesText != "theft" || got.SentenceText != "fine" {
		t.Errorf("charges/sentence = %q / %q", got.ChargesText, got.SentenceText)
	}
}

func TestNamingAllowed(t *testing.T) {
	agency := []domain.FraudCaseSource{{SourceType: domain.FraudSourceAgencyRelease}}
	news := []domain.FraudCaseSource{{SourceType: domain.FraudSourceNews}}
	convicted := domain.FraudCase{NamingApproved: true, Status: domain.CaseConvicted}
	alleged := domain.FraudCase{NamingApproved: true, Status: domain.CaseChargesLaid}
	clean := []domain.FraudCaseParticipant{{}}
	cases := []struct {
		name  string
		c     domain.FraudCase
		parts []domain.FraudCaseParticipant
		srcs  []domain.FraudCaseSource
		want  bool
	}{
		{"post-verdict, approved", convicted, clean, news, true},
		{"reviewer block", domain.FraudCase{Status: domain.CaseConvicted}, clean, news, false},
		{"suppression flag", convicted, []domain.FraudCaseParticipant{{SuppressionFlag: true}}, news, false},
		{"clean slate", convicted, []domain.FraudCaseParticipant{{CleanSlatePossible: true}}, news, false},
		{"pre-verdict, news only", alleged, clean, news, false},
		{"pre-verdict, agency release", alleged, clean, agency, true},
	}
	for _, tc := range cases {
		if got := NamingAllowed(tc.c, tc.parts, tc.srcs); got != tc.want {
			t.Errorf("%s: NamingAllowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
