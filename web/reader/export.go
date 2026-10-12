// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package reader

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/readmodel"
)

// export: GET /tracker/corruption/export?period=&format=csv|md — the
// period's cases as CSV (analysis) or Markdown (drafting). A 404 for anyone
// without view+export (anonymous included: the surface is never
// advertised). Every export opens with the access-agreement notice so the
// terms travel with the artefact.
func (h handlers) export(w http.ResponseWriter, r *http.Request) {
	v, ok := h.view(w, r)
	if !ok {
		return
	}
	if v.Tier() < readmodel.TierExport {
		http.NotFound(w, r)
		return
	}
	period := r.URL.Query().Get("period")
	if !periodRe.MatchString(period) {
		http.Error(w, "period required (YYYY, YYYY-Qn or YYYY-Www)", http.StatusBadRequest)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "md" {
		format = "csv"
	}
	if _, _, _, err := fraudreport.PeriodBounds(period); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := v.Digest(r.Context(), period, format)
	if errors.Is(err, readmodel.ErrForbidden) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "fraud export", err)
		return
	}

	generated := time.Now().UTC().Format("2006-01-02 15:04 MST")
	notice := fmt.Sprintf("Corruption & Fraud Tracker case digest — period %s — generated %s — "+
		"supplied under your organisation's access agreement: no republication of person-level data; "+
		"corrections (including overturned convictions) must be actioned in derived work. "+
		"Pre-verdict matters are ALLEGED: no findings have been made. %s", period, generated,
		v.Notice())

	switch format {
	case "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=fraud-tracker-digest-%s.md", period))
		writeDigestMarkdown(w, period, notice, rows)
	default:
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=fraud-tracker-digest-%s.csv", period))
		writeDigestCSV(w, notice, rows)
	}
}

func writeDigestCSV(w http.ResponseWriter, notice string, rows []readmodel.DigestRow) {
	fmt.Fprintf(w, "# %s\n", notice)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"case_id", "status", "alleged", "typology", "sector", "region", "agency", "agency_basis", "court",
		"court_identifiers", "amount_nzd", "amount_basis", "victim_class", "covid_related",
		"offending_start", "offending_end", "participants", "names", "period_events",
		"summary", "sources",
	})
	for _, row := range rows {
		var events []string
		for _, e := range row.Events {
			events = append(events, fmt.Sprintf("%s %s", e.Date.Format("2006-01-02"), e.Type))
		}
		var urls []string
		for _, src := range row.Sources {
			if src.URL != "" {
				urls = append(urls, src.URL)
			}
		}
		agencyBasis := "stated"
		if row.Labels.AgencyInferred {
			agencyBasis = "inferred (from offence type; no source states it)"
		}
		if row.Labels.Agency == "" {
			agencyBasis = ""
		}
		_ = cw.Write([]string{
			fmt.Sprint(row.ID), row.Status, fmt.Sprint(row.Alleged),
			row.Labels.Typology, row.Labels.Sector, row.Labels.Region, row.Labels.Agency, agencyBasis, row.CourtName,
			row.CourtID, fmtAmount(row.Amount), derefOr(row.AmountBasis, ""), derefOr(row.VictimClass, ""),
			fmt.Sprint(row.CovidRelated), fmtDate(row.OffendingStart), fmtDate(row.OffendingEnd),
			fmt.Sprint(row.NParts), strings.Join(row.Names, "; "), strings.Join(events, "; "),
			row.Summary, strings.Join(urls, " "),
		})
	}
}

func writeDigestMarkdown(w http.ResponseWriter, period, notice string, rows []readmodel.DigestRow) {
	fmt.Fprintf(w, "# Corruption & Fraud Tracker — case digest, %s\n\n> %s\n\n", period, notice)
	groups := []struct {
		title string
		match func(digestGroupKey) bool
	}{
		{"Verdicts & sentencings", func(st digestGroupKey) bool {
			return st.status == "convicted" || st.status == "upheld" || st.status == "acquitted"
		}},
		{"Charges laid & before the courts (ALLEGED — no findings made)", func(st digestGroupKey) bool {
			return st.alleged
		}},
		{"Appeals & other movements", func(st digestGroupKey) bool {
			return st.status == "appeal_pending" || st.status == "overturned" || st.status == "discontinued"
		}},
	}
	for _, g := range groups {
		var section []readmodel.DigestRow
		for _, row := range rows {
			if g.match(digestGroupKey{row.Status, row.Alleged}) {
				section = append(section, row)
			}
		}
		if len(section) == 0 {
			continue
		}
		fmt.Fprintf(w, "## %s\n\n", g.title)
		for _, row := range section {
			head := fmt.Sprintf("**Case #%d — %s, %s**", row.ID,
				orDash(row.Labels.Typology, "Uncoded"), orDash(row.Labels.Region, "region unspecified"))
			if len(row.Names) > 0 {
				head += " — " + strings.Join(row.Names, ", ")
			}
			fmt.Fprintf(w, "%s\n", head)
			var bits []string
			if row.Alleged {
				bits = append(bits, "**ALLEGED — before the courts, no findings made**")
			}
			bits = append(bits, "status: "+row.Status)
			// "prosecuted by X" is an assertion — never made from an
			// inferred agency (typology-prior coding no source states).
			if row.Labels.Agency != "" && !row.Labels.AgencyInferred {
				bits = append(bits, "prosecuted by "+row.Labels.Agency)
			}
			if row.CourtName != "" {
				bits = append(bits, row.CourtName)
			}
			if row.CourtID != "" {
				bits = append(bits, row.CourtID)
			}
			if row.Amount != nil {
				amt := fmt.Sprintf("$%.0f", *row.Amount)
				if row.AmountBasis != nil {
					amt += " (" + *row.AmountBasis
					if row.Alleged {
						amt += " — alleged"
					}
					amt += ")"
				}
				bits = append(bits, amt)
			}
			fmt.Fprintf(w, "%s\n", strings.Join(bits, " · "))
			for _, e := range row.Events {
				fmt.Fprintf(w, "- %s: %s\n", e.Date.Format("2 Jan 2006"), e.Type)
			}
			if row.Summary != "" {
				fmt.Fprintf(w, "\n%s\n", row.Summary)
			}
			var urls []string
			for _, src := range row.Sources {
				if src.URL != "" {
					urls = append(urls, fmt.Sprintf("[%s](%s)", orDash(src.Publisher, "source"), src.URL))
				}
			}
			if len(urls) > 0 {
				fmt.Fprintf(w, "Sources: %s\n", strings.Join(urls, " · "))
			}
			fmt.Fprintln(w)
		}
	}
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func orDash(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// digestGroupKey is what the Markdown digest groups a row by.
type digestGroupKey struct {
	status  string
	alleged bool // pre-verdict (charges laid / before the courts)
}
