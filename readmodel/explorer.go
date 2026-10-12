// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
)

// ExplorerFilter is the explorer query. Review ("unverified" | "any") is
// honoured for operators only; everyone else sees verified cases.
type ExplorerFilter struct {
	Query        string
	TypologySlug string
	AgencySlug   string
	RegionSlug   string
	SectorSlug   string
	Status       string
	VictimClass  string
	CovidOnly    bool
	YearFrom     int
	YearTo       int
	MinAmountNZD float64
	Review       string
	Limit        int
	Offset       int
}

// ExplorerPage is one page of explorer rows.
type ExplorerPage struct {
	Rows  []ExplorerRow
	Total int
	// Filter is the filter actually applied (review state resolved, limit
	// defaulted) — what pagination links must be built from.
	Filter ExplorerFilter
}

// ExplorerParticipant is one participant on an explorer row; Name and
// Profession are empty unless the row's NamesShown. Sentence free text is
// never carried (it routinely embeds names).
type ExplorerParticipant struct {
	Name           string
	Role           string
	Profession     string
	Charges        string
	Outcome        string
	SentenceType   string
	SentenceMonths *int
	AmountNZD      *float64
}

// ExplorerRow is one case after the naming rule.
type ExplorerRow struct {
	ID           int64
	Status       string
	ReviewState  string
	Alleged      bool
	NamesShown   bool
	NamingOff    bool     // withheld because naming is off for this tracker
	Names        []string // empty when withheld
	Summary      string   // empty when withheld
	Typology     string
	Secondary    []string
	Sector       string
	Region       string
	Agency       string
	AgencyBasis  string
	TypologyRaw  string
	CourtName    string
	VictimClass  string
	VictimDetail string
	CovidRelated bool
	AmountNZD    *float64
	AmountBasis  string

	OffendingStart          *time.Time
	OffendingEnd            *time.Time
	ChargesDate             *time.Time
	ConvictionDate          *time.Time
	OffenceToConvictionDays *int
	ChargeToConvictionDays  *int
	LagApproximate          bool

	Participants []ExplorerParticipant
	SourceURLs   []string

	ChargesText  string // participant charge descriptions, deduped and joined
	SentenceText string
}

// Facets are the explorer's dropdown options (active taxonomy terms).
type Facets struct {
	Typologies, Agencies, Regions, Sectors []Term
}

// Term is one taxonomy option.
type Term struct {
	Slug, Label string
}

const explorerPageSize = 200

func (v *view) storeFilter(f ExplorerFilter) store.FraudExplorerFilter {
	sf := store.FraudExplorerFilter{
		Query: f.Query, TypologySlug: f.TypologySlug, AgencySlug: f.AgencySlug,
		RegionSlug: f.RegionSlug, SectorSlug: f.SectorSlug, Status: f.Status,
		VictimClass: f.VictimClass, CovidOnly: f.CovidOnly, YearFrom: f.YearFrom,
		YearTo: f.YearTo, MinAmountNZD: f.MinAmountNZD,
		ReviewState: "verified", Limit: f.Limit, Offset: f.Offset,
	}
	// Operators may inspect the unverified tail (review=unverified|any).
	if v.operator() {
		switch f.Review {
		case "unverified":
			sf.ReviewState = "unverified"
		case "any":
			sf.ReviewState = ""
		}
	}
	return sf
}

// Explorer returns one page of the filtered register (export tier and up),
// audited as "explorer_view" with the request's raw filters.
func (v *view) Explorer(ctx context.Context, f ExplorerFilter) (ExplorerPage, error) {
	if !v.atLeast(TierExport) {
		return ExplorerPage{}, ErrForbidden
	}
	if f.Limit <= 0 {
		f.Limit = explorerPageSize
	}
	naming := v.namingOn(ctx)
	sf := v.storeFilter(f)
	sf.NoIdentitySearch = !naming
	rows, total, err := v.o.Store.SearchFraudCases(ctx, sf)
	if err != nil {
		return ExplorerPage{}, err
	}
	page := ExplorerPage{Total: total, Filter: f, Rows: make([]ExplorerRow, 0, len(rows))}
	for _, row := range rows {
		row.NamingOK = row.NamingOK && naming
		er := explorerRow(row)
		er.NamingOff = !naming
		page.Rows = append(page.Rows, er)
	}
	v.audit(ctx, host.EntitlementExport, "explorer_view",
		fmt.Sprintf("filters=%s (%d/%d cases)", v.rawQuery, len(page.Rows), total))
	return page, nil
}

// ExplorerAll returns every row of the filtered slice (no pagination) for
// CSV export, audited as "explorer_export".
func (v *view) ExplorerAll(ctx context.Context, f ExplorerFilter) ([]ExplorerRow, error) {
	if !v.atLeast(TierExport) {
		return nil, ErrForbidden
	}
	naming := v.namingOn(ctx)
	sf := v.storeFilter(f)
	sf.NoIdentitySearch = !naming
	sf.Limit, sf.Offset = 1000, 0
	var all []ExplorerRow
	for {
		rows, _, err := v.o.Store.SearchFraudCases(ctx, sf)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			row.NamingOK = row.NamingOK && naming
			er := explorerRow(row)
			er.NamingOff = !naming
			all = append(all, er)
		}
		if len(rows) < sf.Limit {
			break
		}
		sf.Offset += sf.Limit
	}
	v.audit(ctx, host.EntitlementExport, "explorer_export",
		fmt.Sprintf("filters=%s (%d cases)", v.rawQuery, len(all)))
	return all, nil
}

// Facets loads the active taxonomy terms for the explorer dropdowns.
func (v *view) Facets(ctx context.Context) (Facets, error) {
	if !v.atLeast(TierExport) {
		return Facets{}, ErrForbidden
	}
	var out Facets
	for kind, dst := range map[string]*[]Term{
		"typology": &out.Typologies, "agency": &out.Agencies,
		"region": &out.Regions, "sector": &out.Sectors,
	} {
		terms, err := v.o.Store.LoadFraudTaxonomy(ctx, kind)
		if err != nil {
			return Facets{}, fmt.Errorf("taxonomy %s: %w", kind, err)
		}
		for _, t := range terms {
			if t.Active {
				*dst = append(*dst, Term{Slug: t.Slug, Label: t.Label})
			}
		}
	}
	return out, nil
}

// explorerRow applies the naming rule and flattens participant detail.
// Charge/sentence text is kept even when names are withheld — the offending
// description is the analytical substance and contains no identity; names,
// professions and summaries (which routinely embed names) are what's gated.
func explorerRow(row store.FraudExplorerRow) ExplorerRow {
	out := ExplorerRow{
		ID: row.ID, Status: row.Status, ReviewState: row.ReviewState,
		Alleged:    domain.FraudCaseStatus(row.Status).PreVerdict(),
		NamesShown: row.NamingOK,
		Typology:   row.Typology, Secondary: row.Secondary, Sector: row.Sector,
		Region: row.Region, Agency: row.Agency, AgencyBasis: row.AgencyBasis,
		TypologyRaw: row.TypologyRaw, CourtName: row.CourtName,
		VictimClass: row.VictimClass, VictimDetail: row.VictimDetail,
		CovidRelated: row.CovidRelated, AmountNZD: row.AmountNZD, AmountBasis: row.AmountBasis,
		OffendingStart: row.OffendingStart, OffendingEnd: row.OffendingEnd,
		ChargesDate: row.ChargesDate, ConvictionDate: row.ConvictionDate,
		OffenceToConvictionDays: row.OffenceToConvictionDays,
		ChargeToConvictionDays:  row.ChargeToConvictionDays,
		LagApproximate:          row.LagApproximate,
		SourceURLs:              row.SourceURLs,
	}
	if out.NamesShown {
		out.Summary = row.Summary
	}
	var charges, sentences []string
	for _, p := range row.Participants {
		if p.Charges != "" {
			charges = append(charges, p.Charges)
		}
		st := p.SentenceType
		if p.SentenceMonths != nil {
			st += fmt.Sprintf(" %dmo", *p.SentenceMonths)
		}
		if strings.TrimSpace(st) != "" {
			sentences = append(sentences, strings.TrimSpace(st))
		}
		ep := ExplorerParticipant{
			Role: p.Role, Charges: p.Charges, Outcome: p.Outcome,
			SentenceType: p.SentenceType, SentenceMonths: p.SentenceMonths, AmountNZD: p.AmountNZD,
		}
		if out.NamesShown {
			ep.Name, ep.Profession = p.Name, p.Profession
			out.Names = append(out.Names, p.Name)
		}
		out.Participants = append(out.Participants, ep)
	}
	out.ChargesText = strings.Join(dedupeStrings(charges), "; ")
	out.SentenceText = strings.Join(sentences, "; ")
	return out
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if k := strings.ToLower(strings.TrimSpace(s)); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}
