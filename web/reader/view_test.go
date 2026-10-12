// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package reader

import (
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/readmodel"
)

// Undated events slot into the timeline by lifecycle stage — an undated
// first appearance belongs between dated charges and a dated verdict, never
// dangling after the verdict (case 139 regression).
func TestOrderTimeline(t *testing.T) {
	d := func(s string) *time.Time { v, _ := time.Parse("2006-01-02", s); return &v }
	got := orderTimeline([]timelineEvent{
		{Type: "charges_laid", Date: d("2024-10-01")},
		{Type: "verdict", Date: d("2026-05-26")},
		{Type: "first_appearance"}, // undated
	})
	want := []string{"charges_laid", "first_appearance", "verdict"}
	for i, w := range want {
		if got[i].Type != w {
			t.Fatalf("order[%d] = %s, want %s (full: %v)", i, got[i].Type, w, types(got))
		}
	}

	// Undated appeal stays after the verdict; undated charges stay first;
	// dated events keep chronological order regardless of rank.
	got = orderTimeline([]timelineEvent{
		{Type: "appeal_outcome"}, // undated
		{Type: "sentencing", Date: d("2025-03-01")},
		{Type: "charges_laid"}, // undated
		{Type: "verdict", Date: d("2025-02-01")},
	})
	want = []string{"charges_laid", "verdict", "sentencing", "appeal_outcome"}
	for i, w := range want {
		if got[i].Type != w {
			t.Fatalf("order[%d] = %s, want %s (full: %v)", i, got[i].Type, w, types(got))
		}
	}
}

func types(es []timelineEvent) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

func TestBuildTimelineLabelsAndFallback(t *testing.T) {
	labels := map[string]string{}
	for _, e := range buildTimeline([]readmodel.Event{{Type: "sentencing"}, {Type: "mystery_step"}}) {
		labels[e.Type] = e.Label
	}
	if labels["sentencing"] != "Sentenced" || labels["mystery_step"] != "mystery_step" {
		t.Errorf("labels = %v", labels)
	}
}

// The small-cell bucket sinks below every real typology regardless of size.
func TestBuildReportViewTrendOrder(t *testing.T) {
	v := buildReportView(&fraudreport.Payload{TypologyTrend: []fraudreport.TrendCell{
		{Quarter: "2024-Q1", Typology: fraudreport.SmallCellLabel, Count: 9},
		{Quarter: "2024-Q1", Typology: "Tax fraud", Count: 3},
		{Quarter: "2024-Q2", Typology: "Benefit fraud", Count: 4},
	}})
	var order []string
	for _, r := range v.TrendRows {
		order = append(order, r.Typology)
	}
	if len(order) != 3 || order[0] != "Benefit fraud" || order[2] != fraudreport.SmallCellLabel {
		t.Errorf("trend order = %v", order)
	}
}

func TestLagYears(t *testing.T) {
	days := 730
	if got := lagYears(&readmodel.Lag{OffenceToConvictionDays: &days, Approximate: true}, true); got != "~2.0 years" {
		t.Errorf("lagYears = %q", got)
	}
	if got := shortLagYears(&days, false); got != "2.0y" {
		t.Errorf("shortLagYears = %q", got)
	}
	if lagYears(nil, true) != "" || shortLagYears(nil, true) != "" {
		t.Error("nil lag must render empty")
	}
}
