// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package reader

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"time"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/readmodel"
	"github.com/lonelycode/corruption-tracker/web/internal/render"
)

// ───────────────────── report display model ─────────────────────
//
// The snapshot payload is storage-shaped (flat cell lists); the page wants
// reading-shaped data. buildReportView reshapes it: headline stats, a
// quarters×typology trend matrix, sorted+humanized sums, and proportion
// bars — all computed here so the template stays declarative.

type reportView struct {
	StatChargesLaid int
	StatConvictions int
	StatAppeals     int
	StatOpenAlleged int    // pipeline cases pre-verdict
	StatConvictedNZ int    // pipeline cases with a standing conviction
	WeekSumLabel    string // humanized sum of this period's conviction amounts

	TrendQuarters []string
	TrendRows     []trendRow
	Sums          []sumRow
	Regions       []barRow
	Agencies      []barRow
}

type trendRow struct {
	Typology string `json:"typology"`
	Counts   []int  `json:"counts"`
	Total    int    `json:"total"`
}

type sumRow struct {
	Typology string  `json:"typology"`
	Basis    string  `json:"basis"`
	Count    int     `json:"count"`
	Label    string  `json:"label"` // humanized NZD
	Total    float64 `json:"total"` // raw NZD, for charts
}

type barRow struct {
	Label string `json:"label"`
	Count int    `json:"count"`
	Pct   int    `json:"-"` // of the largest row, for the proportion bar
}

func buildReportView(p *fraudreport.Payload) *reportView {
	v := &reportView{
		StatChargesLaid: len(p.NewInPeriod.ChargesLaid),
		StatConvictions: len(p.NewInPeriod.Convictions),
		StatAppeals:     len(p.NewInPeriod.AppealOutcomes),
	}
	for _, sc := range p.Pipeline {
		if sc.Alleged {
			v.StatOpenAlleged += sc.Count
		} else if sc.Status == "convicted" || sc.Status == "appeal_pending" || sc.Status == "upheld" {
			v.StatConvictedNZ += sc.Count
		}
	}
	var weekSum float64
	for _, e := range p.NewInPeriod.Convictions {
		if e.Amount != nil {
			weekSum += *e.Amount
		}
	}
	if weekSum > 0 {
		v.WeekSumLabel = render.HumanNZD(weekSum)
	}

	// Trend matrix: last 6 quarters as columns, typologies as rows sorted by
	// total volume; the small-cell bucket sinks to the bottom.
	qSet := map[string]bool{}
	for _, c := range p.TypologyTrend {
		qSet[c.Quarter] = true
	}
	quarters := make([]string, 0, len(qSet))
	for q := range qSet {
		quarters = append(quarters, q)
	}
	sort.Strings(quarters)
	if len(quarters) > 6 {
		quarters = quarters[len(quarters)-6:]
	}
	v.TrendQuarters = quarters
	qIdx := map[string]int{}
	for i, q := range quarters {
		qIdx[q] = i
	}
	rowMap := map[string]*trendRow{}
	for _, c := range p.TypologyTrend {
		i, in := qIdx[c.Quarter]
		if !in {
			continue
		}
		row, ok := rowMap[c.Typology]
		if !ok {
			row = &trendRow{Typology: c.Typology, Counts: make([]int, len(quarters))}
			rowMap[c.Typology] = row
		}
		row.Counts[i] += c.Count
		row.Total += c.Count
	}
	for _, row := range rowMap {
		v.TrendRows = append(v.TrendRows, *row)
	}
	sort.Slice(v.TrendRows, func(i, j int) bool {
		si, sj := v.TrendRows[i], v.TrendRows[j]
		if (si.Typology == fraudreport.SmallCellLabel) != (sj.Typology == fraudreport.SmallCellLabel) {
			return sj.Typology == fraudreport.SmallCellLabel
		}
		if si.Total != sj.Total {
			return si.Total > sj.Total
		}
		return si.Typology < sj.Typology
	})

	// Sums: largest first, humanized.
	sums := make([]fraudreport.SumCell, len(p.Sums))
	copy(sums, p.Sums)
	sort.Slice(sums, func(i, j int) bool { return sums[i].TotalNZD > sums[j].TotalNZD })
	for _, sc := range sums {
		v.Sums = append(v.Sums, sumRow{
			Typology: sc.Typology, Basis: sc.Basis, Count: sc.Count,
			Label: render.HumanNZD(sc.TotalNZD), Total: sc.TotalNZD,
		})
	}

	v.Regions = barRows(p.Regions)
	v.Agencies = barRows(p.AgencyMix)
	return v
}

func barRows(in []fraudreport.LabelCount) []barRow {
	max := 0
	for _, lc := range in {
		if lc.Count > max {
			max = lc.Count
		}
	}
	out := make([]barRow, 0, len(in))
	for _, lc := range in {
		pct := 0
		if max > 0 {
			pct = lc.Count * 100 / max
		}
		out = append(out, barRow{Label: lc.Label, Count: lc.Count, Pct: pct})
	}
	return out
}

// chartJSON serialises the display model's chartable series for the page's
// ECharts block. Marshals only already-suppressed, already-published data —
// nothing reaches the charts that isn't in the tables beside them.
func chartJSON(v *reportView) template.JS {
	if v == nil {
		return "null"
	}
	payload := struct {
		Quarters []string   `json:"quarters"`
		Trend    []trendRow `json:"trend"`
		Sums     []sumRow   `json:"sums"`
		Regions  []barRow   `json:"regions"`
		Agencies []barRow   `json:"agencies"`
	}{v.TrendQuarters, v.TrendRows, v.Sums, v.Regions, v.Agencies}
	b, err := json.Marshal(payload)
	if err != nil {
		return "null"
	}
	return template.JS(b)
}

// ───────────────────────── labels ─────────────────────────

// StatusLabels are the human forms of case statuses, shared by the
// explorer table, its filter dropdown and the case page.
var StatusLabels = map[string]string{
	"charges_laid":   "Charges laid",
	"before_court":   "Before the courts",
	"convicted":      "Convicted",
	"acquitted":      "Acquitted",
	"appeal_pending": "Appeal pending",
	"overturned":     "Overturned",
	"upheld":         "Upheld on appeal",
	"discontinued":   "Discontinued",
}

// StatusLabel is the human form of a status (falls back to the raw value).
func StatusLabel(s string) string {
	if l, ok := StatusLabels[s]; ok {
		return l
	}
	return s
}

// facetOption is one dropdown choice (stored value + human label).
type facetOption struct {
	Value, Label string
}

var explorerStatuses = []facetOption{
	{"charges_laid", "Charges laid"}, {"before_court", "Before the courts"},
	{"convicted", "Convicted"}, {"acquitted", "Acquitted"},
	{"appeal_pending", "Appeal pending"}, {"overturned", "Overturned"},
	{"upheld", "Upheld on appeal"}, {"discontinued", "Discontinued"},
}

var victimClasses = []facetOption{
	{"taxpayer_central_govt", "Taxpayer — central govt"}, {"local_govt", "Local government"},
	{"employer", "Employer"}, {"investors", "Investors"}, {"consumers", "Consumers"},
	{"individuals", "Individuals"}, {"charity", "Charity"}, {"iwi", "Iwi"}, {"mixed", "Mixed"},
}

// ───────────────────────── case timeline ─────────────────────────

// timelineEvent is one renderable lifecycle step.
type timelineEvent struct {
	Type      string
	Label     string
	Date      *time.Time
	Precision string
	Court     string
	Detail    string // empty when naming is withheld
	SourceURL string
	Publisher string
}

// eventRank is each event type's canonical position in the court
// lifecycle, used to slot UNDATED events into a dated timeline at the
// position a reader expects (an undated first appearance belongs between
// the charges and the verdict, not dangling after it). Display-only —
// status derivation has its own, deliberately different ordering
// (DeriveCaseStatus: undated events never claim recency).
var eventRank = map[string]int{
	"charges_laid": 1, "first_appearance": 2, "plea": 3, "trial_start": 4,
	"retrial": 5, "verdict": 6, "sentencing": 7, "appeal_filed": 8,
	"appeal_heard": 9, "appeal_outcome": 10, "discontinued": 11,
}

var eventLabels = map[string]string{
	"charges_laid":     "Charges laid",
	"first_appearance": "First appearance",
	"plea":             "Plea",
	"trial_start":      "Trial began",
	"verdict":          "Verdict",
	"sentencing":       "Sentenced",
	"appeal_filed":     "Appeal filed",
	"appeal_heard":     "Appeal heard",
	"appeal_outcome":   "Appeal outcome",
	"retrial":          "Retrial",
	"discontinued":     "Discontinued",
}

func buildTimeline(events []readmodel.Event) []timelineEvent {
	out := make([]timelineEvent, 0, len(events))
	for _, e := range events {
		te := timelineEvent{
			Type: e.Type, Label: eventLabels[e.Type], Date: e.Date, Precision: e.Precision,
			Court: e.Court, Detail: e.Detail, SourceURL: e.SourceURL, Publisher: e.Publisher,
		}
		if te.Label == "" {
			te.Label = e.Type
		}
		out = append(out, te)
	}
	return orderTimeline(out)
}

// orderTimeline orders dated events chronologically, then inserts each
// undated event before the first event of a LATER lifecycle stage —
// i.e. at the latest position its stage allows.
func orderTimeline(events []timelineEvent) []timelineEvent {
	var dated, undated []timelineEvent
	for _, e := range events {
		if e.Date != nil {
			dated = append(dated, e)
		} else {
			undated = append(undated, e)
		}
	}
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].Date.Before(*dated[j].Date) })
	out := dated
	for _, u := range undated {
		idx := len(out)
		for i, e := range out {
			if eventRank[e.Type] > eventRank[u.Type] {
				idx = i
				break
			}
		}
		out = append(out, timelineEvent{})
		copy(out[idx+1:], out[idx:])
		out[idx] = u
	}
	return out
}

// lagYears humanizes a lag-view interval as years ("~3.2 years"); offence
// picks the offence→conviction lag, otherwise charge→conviction. Empty when
// the dates aren't recoverable.
func lagYears(l *readmodel.Lag, offence bool) string {
	if l == nil {
		return ""
	}
	d := l.ChargeToConvictionDays
	if offence {
		d = l.OffenceToConvictionDays
	}
	if d == nil {
		return ""
	}
	out := fmt.Sprintf("%.1f years", float64(*d)/365.25)
	if l.Approximate {
		out = "~" + out
	}
	return out
}

// shortLagYears is the explorer's compact form ("~3.2y").
func shortLagYears(days *int, approx bool) string {
	if days == nil {
		return ""
	}
	out := fmt.Sprintf("%.1fy", float64(*days)/365.25)
	if approx {
		out = "~" + out
	}
	return out
}
