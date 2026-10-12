// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package reader

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/readmodel"
)

// explorer: GET /tracker/corruption/explorer — every case in one
// filterable, keyword-searchable table, plus a CSV of exactly the filtered
// slice (?format=csv). Tracker access AND the export entitlement: the table
// exposes the same person-level detail the digest export does.
func (h handlers) explorer(w http.ResponseWriter, r *http.Request) {
	v, ok := h.view(w, r)
	if !ok {
		return
	}
	if v.Tier() < readmodel.TierViewer {
		h.toLogin(w, r)
		return
	}
	// Has tracker access but not the explorer/export entitlement: stay 404 —
	// this is an in-product capability boundary, not a sign-in prompt.
	if v.Tier() < readmodel.TierExport {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		h.explorerCSV(w, r, v)
		return
	}
	ctx := r.Context()
	page, err := v.Explorer(ctx, parseExplorerFilter(r))
	if err != nil {
		h.internalError(w, "fraud explorer search", err)
		return
	}
	facets, err := v.Facets(ctx)
	if err != nil {
		h.internalError(w, "fraud explorer taxonomy", err)
		return
	}
	rows := make([]explorerRow, 0, len(page.Rows))
	for _, row := range page.Rows {
		rows = append(rows, newExplorerRow(row))
	}

	csvQuery := r.URL.Query()
	csvQuery.Set("format", "csv")
	csvQuery.Del("offset")
	pageHref := func(offset int) string {
		q := r.URL.Query()
		if offset > 0 {
			q.Set("offset", strconv.Itoa(offset))
		} else {
			q.Del("offset")
		}
		return Base + "/explorer?" + q.Encode()
	}
	f := page.Filter
	prevHref, nextHref := "", ""
	if f.Offset > 0 {
		prevHref = pageHref(max(0, f.Offset-f.Limit))
	}
	if f.Offset+len(rows) < page.Total {
		nextHref = pageHref(f.Offset + f.Limit)
	}

	h.page(w, r, "explorer", v, struct {
		Rows     []explorerRow
		Total    int
		Shown    int
		Filter   readmodel.ExplorerFilter
		Facets   explorerFacets
		CSVHref  string
		IsAdmin  bool
		Review   string
		PrevHref string
		NextHref string
	}{
		Rows: rows, Total: page.Total, Shown: len(rows), Filter: f,
		Facets: explorerFacets{
			Typologies: facets.Typologies, Agencies: facets.Agencies,
			Regions: facets.Regions, Sectors: facets.Sectors,
			Statuses: explorerStatuses, VictimClasses: victimClasses,
		},
		CSVHref: Base + "/explorer?" + csvQuery.Encode(),
		IsAdmin: v.Tier() >= readmodel.TierOperator, Review: r.URL.Query().Get("review"),
		PrevHref: prevHref, NextHref: nextHref,
	})
}

// explorerFacets are the dropdown options.
type explorerFacets struct {
	Typologies, Agencies, Regions, Sectors []readmodel.Term
	Statuses                               []facetOption
	VictimClasses                          []facetOption
}

// explorerRow is a read-model row plus its display labels.
type explorerRow struct {
	readmodel.ExplorerRow
	StatusLabel string
	LagYears    string // humanized offence→conviction lag
}

func newExplorerRow(row readmodel.ExplorerRow) explorerRow {
	return explorerRow{
		ExplorerRow: row,
		StatusLabel: StatusLabel(row.Status),
		LagYears:    shortLagYears(row.OffenceToConvictionDays, row.LagApproximate),
	}
}

func parseExplorerFilter(r *http.Request) readmodel.ExplorerFilter {
	q := r.URL.Query()
	atoi := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return n }
	f := readmodel.ExplorerFilter{
		Query:        strings.TrimSpace(q.Get("q")),
		TypologySlug: q.Get("typology"),
		AgencySlug:   q.Get("agency"),
		RegionSlug:   q.Get("region"),
		SectorSlug:   q.Get("sector"),
		Status:       q.Get("status"),
		VictimClass:  q.Get("victim"),
		CovidOnly:    q.Get("covid") == "1",
		YearFrom:     atoi("from"),
		YearTo:       atoi("to"),
		Review:       q.Get("review"),
		Offset:       atoi("offset"),
	}
	if v, err := strconv.ParseFloat(q.Get("min_amount"), 64); err == nil && v > 0 {
		f.MinAmountNZD = v
	}
	return f
}

// explorerCSV streams the filtered slice — same query, same naming rules,
// every row, no pagination.
func (h handlers) explorerCSV(w http.ResponseWriter, r *http.Request, v readmodel.View) {
	all, err := v.ExplorerAll(r.Context(), parseExplorerFilter(r))
	if err != nil {
		h.internalError(w, "fraud explorer csv", err)
		return
	}

	generated := time.Now().UTC().Format("2006-01-02 15:04 MST")
	notice := fmt.Sprintf("Corruption & Fraud Tracker case explorer export — generated %s — "+
		"supplied under your organisation's access agreement: no republication of person-level data; "+
		"corrections (including overturned convictions) must be actioned in derived work. "+
		"Pre-verdict matters are ALLEGED: no findings have been made. "+
		"agency_basis=inferred marks agencies derived from the offence type, not stated by a source. "+
		"lag_approximate=true marks lags computed from month/year-precision dates. %s", generated,
		v.Notice())

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=fraud-tracker-explorer.csv")
	fmt.Fprintf(w, "# %s\n", notice)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"case_id", "status", "alleged", "review_state", "typology", "secondary_typologies",
		"sector", "region", "agency", "agency_basis", "court", "victim_class", "victim_detail",
		"covid_related", "amount_nzd", "amount_basis",
		"offending_start", "offending_end", "charges_date", "conviction_date",
		"offence_to_conviction_days", "charge_to_conviction_days", "lag_approximate",
		"participants", "names", "professions", "charges", "outcomes", "sentences",
		"summary", "sources",
	})
	for _, row := range all {
		var profs, outcomes []string
		for _, p := range row.Participants {
			if p.Profession != "" {
				profs = append(profs, p.Profession)
			}
			if p.Outcome != "" {
				outcomes = append(outcomes, p.Outcome)
			}
		}
		_ = cw.Write([]string{
			fmt.Sprint(row.ID), row.Status, fmt.Sprint(row.Alleged),
			row.ReviewState, row.Typology, strings.Join(row.Secondary, "; "),
			row.Sector, row.Region, row.Agency, row.AgencyBasis, row.CourtName,
			row.VictimClass, row.VictimDetail,
			fmt.Sprint(row.CovidRelated), fmtAmount(row.AmountNZD), row.AmountBasis,
			fmtDate(row.OffendingStart), fmtDate(row.OffendingEnd),
			fmtDate(row.ChargesDate), fmtDate(row.ConvictionDate),
			fmtIntPtr(row.OffenceToConvictionDays), fmtIntPtr(row.ChargeToConvictionDays),
			fmt.Sprint(row.LagApproximate),
			fmt.Sprint(len(row.Participants)), strings.Join(row.Names, "; "),
			strings.Join(profs, "; "), row.ChargesText, strings.Join(outcomes, "; "),
			row.SentenceText, row.Summary, strings.Join(row.SourceURLs, " "),
		})
	}
}

func fmtIntPtr(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func fmtAmount(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%.0f", *v)
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}
