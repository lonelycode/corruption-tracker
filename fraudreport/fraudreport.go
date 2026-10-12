// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package fraudreport builds the weekly corruption & fraud tracker report
// snapshots. The privacy safeguards are enforced HERE, at payload-build
// time, not left to templates:
//
//   - Person names never enter a payload unless the case has explicit
//     reviewer naming approval, is past verdict, and carries no suppression
//     or Clean Slate flag. The internal case_name label is never published.
//   - Every case entry carries Alleged; pre-verdict entries are always
//     unnamed and templates must badge them ALLEGED.
//   - Aggregates separate alleged from proven — no mixed series exists in
//     the payload, so no chart can mix them.
//   - k-anonymity: any aggregate cell with fewer than MinCellSize cases is
//     merged into a "small cells" bucket (counts) or dropped with a
//     suppression marker (sums/lag).
//
// Snapshots are immutable: corrections produce a new version with a
// revision note (see migration 0119).
package fraudreport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
)

// MinCellSize is k for small-cell suppression: aggregate cells smaller than
// this are merged or suppressed — small cells are de-facto identifying in NZ.
const MinCellSize = 3

// SmallCellLabel is the merge bucket for suppressed cells.
const SmallCellLabel = "Other (small cells)"

// Payload is the versioned snapshot content. Everything the report page
// renders comes from here; the page never queries live tables.
type Payload struct {
	Period       string    `json:"period"`      // YYYY | YYYY-Qn | YYYY-Www
	PeriodKind   string    `json:"period_kind"` // year | quarter | week
	PeriodStart  time.Time `json:"period_start"`
	PeriodEnd    time.Time `json:"period_end"`
	GeneratedAt  time.Time `json:"generated_at"`
	VerifiedOnly bool      `json:"verified_only"`

	NewInPeriod NewInPeriod   `json:"new_in_period"`
	Pipeline    []StatusCount `json:"pipeline"`

	TypologyTrend []TrendCell  `json:"typology_trend"`
	Sums          []SumCell    `json:"sums"`
	Regions       []LabelCount `json:"regions"`
	AgencyMix     []LabelCount `json:"agency_mix"`
	Lag           LagStats     `json:"lag"`

	SuppressedCells int `json:"suppressed_cells"`
}

// CaseEntry is one case as published in a report. No internal label, no
// participant names unless namingAllowed (see entry()).
type CaseEntry struct {
	CaseID      int64    `json:"case_id"`
	Label       string   `json:"label"`
	Typology    string   `json:"typology"`
	Region      string   `json:"region"`
	Agency      string   `json:"agency"`
	Amount      *float64 `json:"amount,omitempty"`
	Basis       string   `json:"basis,omitempty"`
	Status      string   `json:"status"`
	Alleged     bool     `json:"alleged"`
	Names       []string `json:"names,omitempty"` // only when naming approved post-verdict
	Summary     string   `json:"summary,omitempty"`
	EventDetail string   `json:"event_detail,omitempty"`
	// EventDate is the date of the lifecycle event that put this entry in
	// the period (annual issues list months of entries; weekly made it
	// implicit).
	EventDate *time.Time `json:"event_date,omitempty"`
}

// NewInPeriod groups the period's lifecycle movements.
type NewInPeriod struct {
	ChargesLaid    []CaseEntry `json:"charges_laid"`
	Convictions    []CaseEntry `json:"convictions"`
	AppealOutcomes []CaseEntry `json:"appeal_outcomes"`
}

// StatusCount is the pipeline view ("what's before the courts now").
type StatusCount struct {
	Status  string `json:"status"`
	Alleged bool   `json:"alleged"`
	Count   int    `json:"count"`
}

// TrendCell is one (quarter, typology) count. Proven cases only — alleged
// volume lives in Pipeline, never in the same series.
type TrendCell struct {
	Quarter  string `json:"quarter"`
	Typology string `json:"typology"`
	Count    int    `json:"count"`
}

// SumCell aggregates amounts by typology and amount basis.
type SumCell struct {
	Typology string  `json:"typology"`
	Basis    string  `json:"basis"`
	Count    int     `json:"count"`
	TotalNZD float64 `json:"total_nzd"`
}

// LabelCount is a generic labelled count (regions, agencies).
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// LagStats summarises offending -> conviction lag where the data exists.
type LagStats struct {
	SampleSize   int     `json:"sample_size"`
	MedianMonths float64 `json:"median_months"`
	P25Months    float64 `json:"p25_months"`
	P75Months    float64 `json:"p75_months"`
	Suppressed   bool    `json:"suppressed"` // n < MinCellSize
}

// Builder computes and persists snapshots.
type Builder struct {
	DB  *store.Store
	Log *slog.Logger
	// IncludeUnverified bypasses the verified-only gate for dev/preview
	// builds; such snapshots are flagged in the DB and never served to
	// non-admins.
	IncludeUnverified bool
}

// PeriodBounds resolves a period string to its kind and [start, end) UTC
// bounds. Periods: "2025" (calendar year), "2025-Q3" (quarter),
// "2026-W23" (ISO week).
func PeriodBounds(period string) (kind string, start, end time.Time, err error) {
	var year, n int
	switch {
	case len(period) == 4:
		if _, err = fmt.Sscanf(period, "%d", &year); err != nil || year < 1990 || year > 2100 {
			return "", start, end, fmt.Errorf("bad period %q (want YYYY, YYYY-Qn or YYYY-Www)", period)
		}
		start = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		return "year", start, start.AddDate(1, 0, 0), nil
	case len(period) == 7 && period[5] == 'Q':
		if _, err = fmt.Sscanf(period, "%d-Q%d", &year, &n); err != nil || n < 1 || n > 4 {
			return "", start, end, fmt.Errorf("bad quarter %q (want YYYY-Qn)", period)
		}
		start = time.Date(year, time.Month((n-1)*3+1), 1, 0, 0, 0, 0, time.UTC)
		return "quarter", start, start.AddDate(0, 3, 0), nil
	default:
		if _, err = fmt.Sscanf(period, "%d-W%d", &year, &n); err != nil || n < 1 || n > 53 {
			return "", start, end, fmt.Errorf("bad period %q (want YYYY, YYYY-Qn or YYYY-Www)", period)
		}
		// Jan 4 is always in ISO week 1; walk back to its Monday.
		jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
		monday := jan4.AddDate(0, 0, -(int(jan4.Weekday())+6)%7)
		start = monday.AddDate(0, 0, (n-1)*7)
		return "week", start, start.AddDate(0, 0, 7), nil
	}
}

// ISOWeekOf formats t's ISO week as "YYYY-Www".
func ISOWeekOf(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// Build computes the payload for a period (year, quarter, or week) and
// stores it as the next version. Returns the stored version number.
func (b *Builder) Build(ctx context.Context, period, revisionNote string) (Payload, int, error) {
	kind, start, end, err := PeriodBounds(period)
	if err != nil {
		return Payload{}, 0, err
	}
	p := Payload{
		Period: period, PeriodKind: kind, PeriodStart: start, PeriodEnd: end,
		GeneratedAt: time.Now().UTC(), VerifiedOnly: !b.IncludeUnverified,
	}

	if err := b.buildNewInPeriod(ctx, &p, start, end); err != nil {
		return p, 0, err
	}
	if err := b.buildPipeline(ctx, &p); err != nil {
		return p, 0, err
	}
	if err := b.buildTrend(ctx, &p); err != nil {
		return p, 0, err
	}
	if err := b.buildSums(ctx, &p); err != nil {
		return p, 0, err
	}
	if err := b.buildLabelCounts(ctx, &p); err != nil {
		return p, 0, err
	}
	if err := b.buildLag(ctx, &p); err != nil {
		return p, 0, err
	}

	version, err := b.store(ctx, p, revisionNote)
	return p, version, err
}

// reviewFilter is the SQL fragment gating on verified data. Interpolated as
// a boolean param, not string concat.
const caseCols = `c.id, c.status, c.naming_approved, c.total_amount_nzd, c.amount_basis,
	COALESCE(t.label,''), COALESCE(r.label,''), COALESCE(a.label,''), c.summary`

// entry assembles a CaseEntry under the naming rules.
func (b *Builder) entry(ctx context.Context, caseID int64, status, typology, region, agency string,
	amount *float64, basis *string, namingApproved bool, summary string) CaseEntry {

	st := domain.FraudCaseStatus(status)
	e := CaseEntry{
		CaseID:   caseID,
		Typology: orUnknown(typology, "Uncoded"),
		Region:   orUnknown(region, "Unspecified region"),
		Agency:   orUnknown(agency, "Unspecified agency"),
		Amount:   amount,
		Status:   status,
		Alleged:  st.PreVerdict(),
	}
	if basis != nil {
		e.Basis = *basis
	}
	// Label never includes the internal case_name. Names appear only when:
	// not reviewer-blocked AND no participant suppression or Clean Slate
	// flag AND (past verdict OR the prosecuting agency itself published the
	// matter — an agency_release source; we mirror the prosecutor's naming).
	e.Label = fmt.Sprintf("Case #%d — %s, %s", caseID, e.Typology, e.Region)
	if namingApproved && (!st.PreVerdict() || b.agencyNamed(ctx, caseID)) {
		names, ok := b.publishableNames(ctx, caseID)
		if ok {
			e.Names = names
			e.Summary = summary // reviewer-written; only on namable cases
		}
	}
	return e
}

// agencyNamed reports whether the case carries an official agency media
// release — the pre-verdict naming basis (the prosecutor went public).
func (b *Builder) agencyNamed(ctx context.Context, caseID int64) bool {
	var ok bool
	err := b.DB.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM fraud_sources
		                WHERE case_id = $1 AND source_type = 'agency_release')`, caseID).Scan(&ok)
	if err != nil {
		b.Log.Warn("agency-named lookup failed", "case", caseID, "err", err)
		return false // fail closed: unnamed
	}
	return ok
}

// publishableNames returns participant names when NO participant carries a
// suppression or Clean Slate flag. One flagged participant withholds the
// whole case's names (partial naming of a co-accused set is identifying).
func (b *Builder) publishableNames(ctx context.Context, caseID int64) ([]string, bool) {
	rows, err := b.DB.Pool.Query(ctx, `
		SELECT p.full_name, cp.suppression_flag OR cp.clean_slate_possible
		  FROM fraud_case_participants cp
		  JOIN fraud_persons p ON p.id = cp.person_id
		 WHERE cp.case_id = $1 ORDER BY cp.id`, caseID)
	if err != nil {
		b.Log.Warn("publishable names query failed", "case", caseID, "err", err)
		return nil, false
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		var flagged bool
		if err := rows.Scan(&name, &flagged); err != nil || flagged {
			return nil, false
		}
		names = append(names, name)
	}
	if rows.Err() != nil || len(names) == 0 {
		return nil, false
	}
	return names, true
}

func (b *Builder) buildNewInPeriod(ctx context.Context, p *Payload, start, end time.Time) error {
	rows, err := b.DB.Pool.Query(ctx, `
		SELECT DISTINCT ON (c.id, e.event_type) `+caseCols+`, e.event_type, e.outcome_detail, e.event_date
		  FROM fraud_case_events e
		  JOIN fraud_cases c ON c.id = e.case_id
		  LEFT JOIN fraud_typologies t ON t.id = c.typology_id
		  LEFT JOIN fraud_regions r    ON r.id = c.region_id
		  -- Inferred agencies (typology-prior coding, no source states them)
		  -- never reach published output: the join miss renders "Unspecified".
		  LEFT JOIN fraud_agencies a   ON a.id = c.prosecuting_agency_id AND c.agency_basis <> 'inferred'
		 WHERE e.event_date >= $1 AND e.event_date < $2
		   AND e.event_type IN ('charges_laid','verdict','sentencing','appeal_outcome')
		   AND ($3 OR c.review_state = 'verified')
		 ORDER BY c.id, e.event_type, e.id DESC`, start, end, b.IncludeUnverified)
	if err != nil {
		return fmt.Errorf("new this week: %w", err)
	}
	defer rows.Close()

	type rec struct {
		id                                int64
		status                            string
		naming                            bool
		amount                            *float64
		basis                             *string
		typology, region, agency, summary string
		eventType, detail                 string
		eventDate                         *time.Time
	}
	var recs []rec
	for rows.Next() {
		var x rec
		if err := rows.Scan(&x.id, &x.status, &x.naming, &x.amount, &x.basis,
			&x.typology, &x.region, &x.agency, &x.summary, &x.eventType, &x.detail, &x.eventDate); err != nil {
			return fmt.Errorf("scan new this week: %w", err)
		}
		recs = append(recs, x)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range recs {
		e := b.entry(ctx, x.id, x.status, x.typology, x.region, x.agency, x.amount, x.basis, x.naming, x.summary)
		e.EventDetail = x.detail
		e.EventDate = x.eventDate
		switch x.eventType {
		case "charges_laid":
			p.NewInPeriod.ChargesLaid = append(p.NewInPeriod.ChargesLaid, e)
		case "verdict", "sentencing":
			p.NewInPeriod.Convictions = append(p.NewInPeriod.Convictions, e)
		case "appeal_outcome":
			p.NewInPeriod.AppealOutcomes = append(p.NewInPeriod.AppealOutcomes, e)
		}
	}
	// A case sentenced AND convicted in the same period appears once, and
	// long periods (annual issues) read chronologically.
	p.NewInPeriod.Convictions = dedupeEntries(p.NewInPeriod.Convictions)
	for _, list := range [][]CaseEntry{p.NewInPeriod.ChargesLaid, p.NewInPeriod.Convictions, p.NewInPeriod.AppealOutcomes} {
		sort.SliceStable(list, func(i, j int) bool {
			di, dj := time.Time{}, time.Time{}
			if list[i].EventDate != nil {
				di = *list[i].EventDate
			}
			if list[j].EventDate != nil {
				dj = *list[j].EventDate
			}
			return di.Before(dj)
		})
	}
	return nil
}

func dedupeEntries(in []CaseEntry) []CaseEntry {
	seen := map[int64]bool{}
	var out []CaseEntry
	for _, e := range in {
		if !seen[e.CaseID] {
			seen[e.CaseID] = true
			out = append(out, e)
		}
	}
	return out
}

func (b *Builder) buildPipeline(ctx context.Context, p *Payload) error {
	rows, err := b.DB.Pool.Query(ctx, `
		SELECT status, count(*) FROM fraud_cases c
		 WHERE ($1 OR c.review_state = 'verified')
		 GROUP BY status`, b.IncludeUnverified)
	if err != nil {
		return fmt.Errorf("pipeline: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sc StatusCount
		if err := rows.Scan(&sc.Status, &sc.Count); err != nil {
			return err
		}
		sc.Alleged = domain.FraudCaseStatus(sc.Status).PreVerdict()
		p.Pipeline = append(p.Pipeline, sc)
	}
	sort.Slice(p.Pipeline, func(i, j int) bool { return p.Pipeline[i].Status < p.Pipeline[j].Status })
	return rows.Err()
}

// provenStatuses: cases with a standing conviction. Alleged volume is shown
// in the pipeline view only — never in the same series as proven.
const provenStatuses = `('convicted','appeal_pending','upheld')`

func (b *Builder) buildTrend(ctx context.Context, p *Payload) error {
	rows, err := b.DB.Pool.Query(ctx, `
		WITH anchored AS (
			SELECT c.id, COALESCE(t.label,'Uncoded') AS typology,
			       to_char(date_trunc('quarter', MIN(e.event_date)), 'YYYY"Q"Q') AS quarter
			  FROM fraud_cases c
			  LEFT JOIN fraud_typologies t ON t.id = c.typology_id
			  JOIN fraud_case_events e ON e.case_id = c.id
			 WHERE c.status IN `+provenStatuses+`
			   AND e.event_date IS NOT NULL
			   AND e.event_type IN ('verdict','sentencing')
			   AND ($1 OR c.review_state = 'verified')
			 GROUP BY c.id, t.label
		)
		SELECT quarter, typology, count(*) FROM anchored
		 WHERE quarter IS NOT NULL
		 GROUP BY quarter, typology ORDER BY quarter, typology`, b.IncludeUnverified)
	if err != nil {
		return fmt.Errorf("trend: %w", err)
	}
	defer rows.Close()
	var cells []TrendCell
	for rows.Next() {
		var c TrendCell
		if err := rows.Scan(&c.Quarter, &c.Typology, &c.Count); err != nil {
			return err
		}
		cells = append(cells, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// k-anonymity: merge sub-k cells into the small-cell bucket per quarter.
	merged := map[string]int{}
	var out []TrendCell
	for _, c := range cells {
		if c.Count < MinCellSize {
			merged[c.Quarter] += c.Count
			p.SuppressedCells++
			continue
		}
		out = append(out, c)
	}
	for q, n := range merged {
		out = append(out, TrendCell{Quarter: q, Typology: SmallCellLabel, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Quarter != out[j].Quarter {
			return out[i].Quarter < out[j].Quarter
		}
		return out[i].Typology < out[j].Typology
	})
	p.TypologyTrend = out
	return nil
}

func (b *Builder) buildSums(ctx context.Context, p *Payload) error {
	rows, err := b.DB.Pool.Query(ctx, `
		SELECT COALESCE(t.label,'Uncoded'), c.amount_basis, count(*), SUM(c.total_amount_nzd)
		  FROM fraud_cases c
		  LEFT JOIN fraud_typologies t ON t.id = c.typology_id
		 WHERE c.total_amount_nzd IS NOT NULL AND c.amount_basis IS NOT NULL
		   AND c.status IN `+provenStatuses+`
		   AND ($1 OR c.review_state = 'verified')
		 GROUP BY t.label, c.amount_basis
		 ORDER BY t.label, c.amount_basis`, b.IncludeUnverified)
	if err != nil {
		return fmt.Errorf("sums: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s SumCell
		if err := rows.Scan(&s.Typology, &s.Basis, &s.Count, &s.TotalNZD); err != nil {
			return err
		}
		if s.Count < MinCellSize {
			// Sums are more identifying than counts: sub-k cells are dropped
			// entirely (not merged — a merged sum still leaks a small cell's
			// magnitude when the bucket is dominated by one case).
			p.SuppressedCells++
			continue
		}
		p.Sums = append(p.Sums, s)
	}
	return rows.Err()
}

func (b *Builder) buildLabelCounts(ctx context.Context, p *Payload) error {
	for _, q := range []struct {
		dst  *[]LabelCount
		join string
	}{
		{&p.Regions, `LEFT JOIN fraud_regions x ON x.id = c.region_id`},
		// Inferred agencies count as Unspecified in the published mix — a
		// prior is internal coding, not an attribution any source supports.
		{&p.AgencyMix, `LEFT JOIN fraud_agencies x ON x.id = c.prosecuting_agency_id AND c.agency_basis <> 'inferred'`},
	} {
		rows, err := b.DB.Pool.Query(ctx, `
			SELECT COALESCE(x.label,'Unspecified'), count(*)
			  FROM fraud_cases c `+q.join+`
			 WHERE c.status IN `+provenStatuses+`
			   AND ($1 OR c.review_state = 'verified')
			 GROUP BY x.label ORDER BY count(*) DESC`, b.IncludeUnverified)
		if err != nil {
			return fmt.Errorf("label counts: %w", err)
		}
		small := 0
		for rows.Next() {
			var lc LabelCount
			if err := rows.Scan(&lc.Label, &lc.Count); err != nil {
				rows.Close()
				return err
			}
			if lc.Count < MinCellSize {
				small += lc.Count
				p.SuppressedCells++
				continue
			}
			*q.dst = append(*q.dst, lc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if small > 0 {
			*q.dst = append(*q.dst, LabelCount{Label: SmallCellLabel, Count: small})
		}
	}
	return nil
}

func (b *Builder) buildLag(ctx context.Context, p *Payload) error {
	rows, err := b.DB.Pool.Query(ctx, `
		SELECT EXTRACT(EPOCH FROM (MIN(e.event_date)::timestamp - c.offending_period_start::timestamp)) / 2629800
		  FROM fraud_cases c
		  JOIN fraud_case_events e ON e.case_id = c.id
		 WHERE c.offending_period_start IS NOT NULL
		   AND e.event_type IN ('verdict','sentencing') AND e.event_date IS NOT NULL
		   AND c.status IN `+provenStatuses+`
		   AND ($1 OR c.review_state = 'verified')
		 GROUP BY c.id, c.offending_period_start`, b.IncludeUnverified)
	if err != nil {
		return fmt.Errorf("lag: %w", err)
	}
	defer rows.Close()
	var months []float64
	for rows.Next() {
		var m float64
		if err := rows.Scan(&m); err != nil {
			return err
		}
		if m >= 0 {
			months = append(months, m)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.Lag.SampleSize = len(months)
	if len(months) < MinCellSize {
		p.Lag.Suppressed = true
		return nil
	}
	sort.Float64s(months)
	p.Lag.MedianMonths = quantile(months, 0.5)
	p.Lag.P25Months = quantile(months, 0.25)
	p.Lag.P75Months = quantile(months, 0.75)
	return nil
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := q * float64(len(sorted)-1)
	lo := int(pos)
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[lo+1]*frac
}

// store persists the payload as the next version for its week. INSERT-only:
// existing versions are never touched.
func (b *Builder) store(ctx context.Context, p Payload, revisionNote string) (int, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}
	var version int
	err = b.DB.Pool.QueryRow(ctx, `
		INSERT INTO fraud_report_snapshots (period, version, payload, revision_note, include_unverified)
		SELECT $1, COALESCE(MAX(version),0)+1, $2, $3, $4
		  FROM fraud_report_snapshots WHERE period = $1
		RETURNING version`, p.Period, raw, revisionNote, b.IncludeUnverified).Scan(&version)
	if err != nil {
		// The frs_revision_note_chk constraint fires when re-issuing a week
		// without a revision note — surface that as the actionable error.
		return 0, fmt.Errorf("store snapshot (re-issuing an existing week requires a revision note): %w", err)
	}
	return version, nil
}

func orUnknown(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
