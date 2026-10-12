// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudsheet

import (
	"testing"

	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

func TestParseSentence(t *testing.T) {
	cases := []struct {
		raw    string
		typ    string // "" = nil
		months int    // 0 = nil
	}{
		{"8 1/2 years", "imprisonment", 102},
		{"2 years 3 months imprisonment", "imprisonment", 27},
		{"jailed for 4 years", "imprisonment", 48},
		{"11 months home detention", "home_detention", 0},
		{"6 months community detention", "community_detention", 0},
		{"300 hours community work", "community_work", 0},
		{"periodic detention ", "community_work", 0},
		{"fined $50,000", "fine", 0},
		{"reparation of $12,000", "reparation_only", 0},
		{"discharged without conviction", "discharge", 0},
		{"deported after sentence", "deportation_followed", 0},
		{"", "", 0},
		{"pending", "", 0},
	}
	for _, tc := range cases {
		typ, months := ParseSentence(tc.raw)
		gotTyp := ""
		if typ != nil {
			gotTyp = *typ
		}
		gotMonths := 0
		if months != nil {
			gotMonths = *months
		}
		if gotTyp != tc.typ || gotMonths != tc.months {
			t.Errorf("ParseSentence(%q) = (%q,%d), want (%q,%d)", tc.raw, gotTyp, gotMonths, tc.typ, tc.months)
		}
	}
}

func TestVictimClass(t *testing.T) {
	cases := map[string]string{
		"NZ Government":             "taxpayer_central_govt",
		"Taxpayers":                 "taxpayer_central_govt",
		"Local Government":          "local_govt",
		"employer":                  "employer",
		"Investors lost everything": "investors",
		"Charitable trust closed":   "charity",
		"PNG Migrants":              "individuals",
		"IRD, MBIE":                 "taxpayer_central_govt",
		"Real Estate Company, Selwyn District Council": "local_govt",
		"Drug Victims and the IRD":                     "mixed",
		"":                                             "",
		"unknown":                                      "",
	}
	for raw, want := range cases {
		got := victimClass(raw)
		gotS := ""
		if got != nil {
			gotS = *got
		}
		if gotS != want {
			t.Errorf("victimClass(%q) = %q, want %q", raw, gotS, want)
		}
	}
}

func TestSplitNameAliases(t *testing.T) {
	name, aliases := splitNameAliases("Marla Fenning aka Quist", "Mar Fenning, M. Quist")
	if name != "Marla Fenning" {
		t.Errorf("name = %q", name)
	}
	if len(aliases) != 3 || aliases[0] != "Quist" {
		t.Errorf("aliases = %v", aliases)
	}
}

func TestClusterRowsCorroboration(t *testing.T) {
	amount := 54000000.0
	year := 2018
	mk := func(row int, name, region, co, org string) Row {
		return Row{Row: row, FullName: name, AmountNZD: &amount, YearConvicted: &year,
			AreaRaw: "Mortgage Fraud", RegionRaw: region, CoAccusedRaw: co, CompanyRaw: org}
	}
	empty := fraudtracker.NewTermResolver(nil)
	res := &resolvers{agency: empty, typology: empty, region: empty, profession: empty}

	// Same region: merge.
	groups, amb := clusterRows([]Row{
		mk(1, "A One", "Auckland", "", ""),
		mk(2, "B Two", "Auckland", "", ""),
	}, res)
	if len(groups) != 1 || len(amb) != 0 {
		t.Errorf("same-region: groups=%d amb=%d, want 1 group", len(groups), len(amb))
	}

	// No corroboration: split + reported.
	groups, amb = clusterRows([]Row{
		mk(3, "C Three", "", "", "Org A"),
		mk(4, "D Four", "", "", "Org B"),
	}, res)
	if len(groups) != 2 || len(amb) != 1 {
		t.Errorf("uncorroborated: groups=%d amb=%d, want 2 groups + 1 note", len(groups), len(amb))
	}

	// Co-accused note: merge even without shared region/org.
	groups, _ = clusterRows([]Row{
		mk(5, "E Five", "", "with business partner", ""),
		mk(6, "F Six", "Hamilton", "", ""),
	}, res)
	if len(groups) != 1 {
		t.Errorf("co-accused: groups=%d, want 1", len(groups))
	}

	// Different amounts never cluster.
	other := 1000.0
	r2 := mk(8, "H Eight", "Auckland", "", "")
	r2.AmountNZD = &other
	groups, _ = clusterRows([]Row{mk(7, "G Seven", "Auckland", "", ""), r2}, res)
	if len(groups) != 2 {
		t.Errorf("different amounts: groups=%d, want 2", len(groups))
	}
}

func TestClusterCrossReference(t *testing.T) {
	amount := 4321000.0
	year := 2015
	a := Row{Row: 3, FullName: "Corin Waddell", AmountNZD: &amount, YearConvicted: &year,
		AreaRaw: "Tax Fraud", Details: "alongside business partner Hollis Brandt"}
	b := Row{Row: 4, FullName: "Hollis Brandt", AmountNZD: &amount, YearConvicted: &year,
		AreaRaw: "tax fraud"}
	empty := fraudtracker.NewTermResolver(nil)
	res := &resolvers{agency: empty, typology: empty, region: empty, profession: empty}
	groups, amb := clusterRows([]Row{a, b}, res)
	if len(groups) != 1 || len(amb) != 0 {
		t.Errorf("cross-referenced rows must merge: groups=%d amb=%d", len(groups), len(amb))
	}
}

// Pending-penalty rows ("not yet", "not yet heard") are matters before the
// courts: charges_laid event, no fabricated conviction. "not yet sentenced"
// means convicted-awaiting-sentence and stays a conviction.
func TestPendingPenaltyRows(t *testing.T) {
	for raw, pending := range map[string]bool{
		"not yet":           true,
		"not yet heard":     true,
		"awaiting trial":    true,
		"not yet sentenced": false,
		"to be sentenced":   false,
		"8 months home det": false,
		"":                  false,
	} {
		if got := penaltyPending(raw); got != pending {
			t.Errorf("penaltyPending(%q) = %v, want %v", raw, got, pending)
		}
	}

	year := 2024
	empty := fraudtracker.NewTermResolver(nil)
	res := &resolvers{agency: empty, typology: empty, region: empty, profession: empty}
	in := buildCaseInput([]Row{{Row: 1, FullName: "Pending Person", YearConvicted: &year,
		AreaRaw: "investment fraud", PenaltyRaw: "not yet"}}, res)
	if in.Events[0].EventType != "charges_laid" {
		t.Errorf("pending row event = %s, want charges_laid", in.Events[0].EventType)
	}
	if in.Participants[0].Participant.Outcome != nil {
		t.Errorf("pending row outcome = %v, want nil (pre-verdict)", *in.Participants[0].Participant.Outcome)
	}

	in = buildCaseInput([]Row{{Row: 2, FullName: "Sentenced Person", YearConvicted: &year,
		AreaRaw: "tax fraud", PenaltyRaw: "8 months home det"}}, res)
	if in.Events[0].EventType != "sentencing" || in.Participants[0].Participant.Outcome == nil {
		t.Errorf("sentenced row should stay a conviction: %s %v", in.Events[0].EventType, in.Participants[0].Participant.Outcome)
	}
}

// Placeholder cells must not survive into detail fields; substantive
// pending notes must.
func TestPlaceholderDetail(t *testing.T) {
	for _, raw := range []string{"not yet", "Not yet heard", "not yet sentenced",
		"pending", "N/A", "?", "-", "TBC", "awaiting trial", " Awaiting sentencing. "} {
		if !placeholderDetail(raw) {
			t.Errorf("placeholderDetail(%q) = false, want true", raw)
		}
		if got := detailOrEmpty(raw); got != "" {
			t.Errorf("detailOrEmpty(%q) = %q, want empty", raw, got)
		}
	}
	for _, raw := range []string{"awaiting trial March 2027", "2 years imprisonment",
		"not yet sentenced; reparations of $50,000 ordered", "home detention"} {
		if placeholderDetail(raw) {
			t.Errorf("placeholderDetail(%q) = true, want false (substantive)", raw)
		}
	}
}
