// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudreport

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
)

func TestPeriodBounds(t *testing.T) {
	kind, start, end, err := PeriodBounds("2026-W23")
	if err != nil || kind != "week" {
		t.Fatalf("week: kind=%s err=%v", kind, err)
	}
	if start.Weekday() != time.Monday {
		t.Errorf("week must start Monday, got %s", start.Weekday())
	}
	if got := end.Sub(start); got != 7*24*time.Hour {
		t.Errorf("week length = %v", got)
	}
	if ISOWeekOf(start) != "2026-W23" || ISOWeekOf(end.Add(-time.Second)) != "2026-W23" {
		t.Errorf("bounds don't round-trip: %s / %s", ISOWeekOf(start), ISOWeekOf(end.Add(-time.Second)))
	}

	kind, start, end, err = PeriodBounds("2025")
	if err != nil || kind != "year" || start.Month() != time.January || end.Year() != 2026 {
		t.Errorf("year bounds: kind=%s %v..%v err=%v", kind, start, end, err)
	}
	kind, start, end, err = PeriodBounds("2025-Q3")
	if err != nil || kind != "quarter" || start.Month() != time.July || end.Month() != time.October {
		t.Errorf("quarter bounds: kind=%s %v..%v err=%v", kind, start, end, err)
	}
	for _, bad := range []string{"garbage", "2025-Q5", "25", "2025-W99"} {
		if _, _, _, err := PeriodBounds(bad); err == nil {
			t.Errorf("PeriodBounds(%q) must error", bad)
		}
	}
}

func datePtr(s string) *time.Time {
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &tm
}

func f64(v float64) *float64 { return &v }
func strp(s string) *string  { return &s }

// seedCase creates a sentenced case with one typology/region and one named
// defendant inside the given week.
func seedCase(t *testing.T, ctx context.Context, d *store.Store, name, person, typologySlug string, amount float64, sentencedOn string, naming bool) domain.FraudCase {
	t.Helper()
	terms, err := d.LoadFraudTaxonomy(ctx, "typology")
	if err != nil {
		t.Fatal(err)
	}
	var typID *int64
	for _, tt := range terms {
		if tt.Slug == typologySlug {
			id := tt.ID
			typID = &id
		}
	}
	c, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{
			CaseName: name, TypologyID: typID, TotalAmountNZD: f64(amount),
			AmountBasis: strp("proven"), NamingApproved: naming,
			Summary: "reviewer summary for " + name,
		},
		Participants: []store.FraudParticipantInput{{
			PersonName:  person,
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Events: []domain.FraudCaseEvent{{
			EventType: domain.EventSentencing, EventDate: datePtr(sentencedOn),
		}},
		Sources: []domain.FraudCaseSource{{
			SourceType: domain.FraudSourceManual, ContentHash: "h-" + name,
		}},
	})
	if err != nil {
		t.Fatalf("seed case %s: %v", name, err)
	}
	// Naming defaults ON at create (0128); blocking is a reviewer action.
	if !naming {
		if err := d.SetFraudCaseNamingApproved(ctx, c.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestBuildSnapshotPrivacyRules(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	// Four convicted tax-fraud cases in week 2026-W23 (>= k so they survive
	// suppression), one of them naming-approved.
	seedCase(t, ctx, d, "Named", "Publish Me", "tax-fraud", 100000, "2026-06-01", true)
	seedCase(t, ctx, d, "Unnamed1", "Hidden One", "tax-fraud", 200000, "2026-06-02", false)
	seedCase(t, ctx, d, "Unnamed2", "Hidden Two", "tax-fraud", 300000, "2026-06-03", false)
	seedCase(t, ctx, d, "Unnamed3", "Hidden Three", "tax-fraud", 400000, "2026-06-04", false)
	// One singleton typology — must be suppressed out of trend/sums.
	seedCase(t, ctx, d, "Lonely", "Small Cell", "cartel-conduct", 999999, "2026-06-05", false)
	// One pre-verdict case: charges laid this week, must render ALLEGED and
	// unnamed even with naming_approved mistakenly set.
	alleged, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Alleged", NamingApproved: true},
		Participants: []store.FraudParticipantInput{{
			PersonName:  "Presumed Innocent",
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Events: []domain.FraudCaseEvent{{
			EventType: domain.EventChargesLaid, EventDate: datePtr("2026-06-02"),
		}},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: "h-alleged"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if alleged.Status != domain.CaseChargesLaid {
		t.Fatalf("alleged case status = %s", alleged.Status)
	}

	b := &Builder{DB: d, Log: slog.Default(), IncludeUnverified: true}
	p, version, err := b.Build(ctx, "2026-W23", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d", version)
	}

	// New-this-week grouping.
	if len(p.NewInPeriod.Convictions) != 5 {
		t.Errorf("convictions = %d, want 5", len(p.NewInPeriod.Convictions))
	}
	if len(p.NewInPeriod.ChargesLaid) != 1 {
		t.Fatalf("charges laid = %d, want 1", len(p.NewInPeriod.ChargesLaid))
	}

	// ALLEGED entry: badge flag set, no names, despite naming_approved.
	a := p.NewInPeriod.ChargesLaid[0]
	if !a.Alleged || len(a.Names) != 0 {
		t.Errorf("pre-verdict entry must be alleged+unnamed: alleged=%v names=%v", a.Alleged, a.Names)
	}

	// Naming rules: only the approved case carries names; labels never leak
	// the internal case_name.
	named := 0
	for _, e := range p.NewInPeriod.Convictions {
		if len(e.Names) > 0 {
			named++
			if e.Names[0] != "Publish Me" {
				t.Errorf("wrong name published: %v", e.Names)
			}
		}
		for _, banned := range []string{"Named", "Unnamed1", "Unnamed2", "Unnamed3", "Lonely"} {
			if e.Label == banned {
				t.Errorf("label leaks internal case_name: %q", e.Label)
			}
		}
	}
	if named != 1 {
		t.Errorf("named entries = %d, want exactly 1", named)
	}

	// k-suppression: the cartel singleton must not appear as its own
	// trend/sum cell.
	for _, c := range p.TypologyTrend {
		if c.Typology == "Cartel conduct" {
			t.Errorf("singleton typology must be suppressed from trend")
		}
	}
	for _, s := range p.Sums {
		if s.Typology == "Cartel conduct" {
			t.Errorf("singleton typology must be suppressed from sums")
		}
	}
	if p.SuppressedCells == 0 {
		t.Error("suppression counter should be non-zero")
	}

	// Alleged/proven separation: pipeline carries the alleged case; no
	// proven aggregate includes it.
	var allegedInPipeline bool
	for _, sc := range p.Pipeline {
		if sc.Alleged && sc.Count > 0 {
			allegedInPipeline = true
		}
	}
	if !allegedInPipeline {
		t.Error("alleged case missing from pipeline view")
	}
	var trendTotal int
	for _, c := range p.TypologyTrend {
		trendTotal += c.Count
	}
	if trendTotal != 5 { // 4 tax + 1 suppressed-merged; alleged excluded
		t.Errorf("trend total = %d, want 5 (alleged must not enter proven series)", trendTotal)
	}

	// Lag: no offending dates seeded -> suppressed, not fabricated.
	if !p.Lag.Suppressed || p.Lag.SampleSize != 0 {
		t.Errorf("lag should be suppressed with no data: %+v", p.Lag)
	}
}

func TestSnapshotVersioning(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	seedCase(t, ctx, d, "V", "Person V", "tax-fraud", 1000, "2026-06-01", false)
	b := &Builder{DB: d, Log: slog.Default(), IncludeUnverified: true}

	if _, v, err := b.Build(ctx, "2026-W23", ""); err != nil || v != 1 {
		t.Fatalf("v1 build: v=%d err=%v", v, err)
	}
	// Re-issuing without a note must fail (immutable + visible revisions).
	if _, _, err := b.Build(ctx, "2026-W23", ""); err == nil {
		t.Fatal("revision without note must fail")
	}
	if _, v, err := b.Build(ctx, "2026-W23", "conviction overturned on appeal"); err != nil || v != 2 {
		t.Fatalf("v2 build: v=%d err=%v", v, err)
	}

	// Reads: latest version wins; weeks list marks revisions; dev builds
	// hidden from non-dev reads.
	snap, err := d.GetFraudReportSnapshot(ctx, "2026-W23", true)
	if err != nil || snap.Version != 2 || snap.RevisionNote == "" {
		t.Fatalf("get snapshot: v=%d note=%q err=%v", snap.Version, snap.RevisionNote, err)
	}
	issues, err := d.ListFraudReportPeriods(ctx, true)
	if err != nil || len(issues) != 1 || !issues[0].Revised || issues[0].Kind() != "week" {
		t.Fatalf("issues: %v err=%v", issues, err)
	}
	if _, err := d.GetFraudReportSnapshot(ctx, "2026-W23", false); err == nil {
		t.Fatal("unverified dev snapshot must be hidden from verified-only reads")
	}
}
