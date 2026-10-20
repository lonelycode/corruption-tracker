// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"fmt"
	"time"
)

// Internal case explorer — the "fuller list" surface: every case in one
// filterable table with participant detail and offence→conviction lags.
// Read-only; the handler applies the naming rules before anything reaches a
// template or export. The query is one round trip: labels joined, lag view
// joined, participants aggregated as JSON.

// FraudExplorerFilter narrows SearchFraudCases. Zero values mean "any".
type FraudExplorerFilter struct {
	Query        string // keyword over raw coding and charges; summary, names and professions only where names may be shown
	TypologySlug string
	AgencySlug   string
	RegionSlug   string
	SectorSlug   string
	Status       string
	VictimClass  string
	CovidOnly    bool
	YearFrom     int // conviction (or latest-event) year bounds
	YearTo       int
	MinAmountNZD float64
	ReviewState  string // '' = any (handler forces 'verified' for non-admins)
	Limit        int
	Offset       int
	// NoIdentitySearch keeps the keyword off the summary and participant
	// text, so a search can't reveal which cases involve a withheld name
	// (standalone with naming off). Zero keeps the full search.
	NoIdentitySearch bool
}

// FraudExplorerParticipant is one participant inside an explorer row.
// Names are joined here; the HANDLER decides whether they may be shown.
type FraudExplorerParticipant struct {
	Name               string   `json:"name"`
	Role               string   `json:"role"`
	Profession         string   `json:"profession"`
	Charges            string   `json:"charges"`
	Outcome            string   `json:"outcome"`
	SentenceType       string   `json:"sentence_type"`
	SentenceDetail     string   `json:"sentence_detail"`
	SentenceMonths     *int     `json:"sentence_months"`
	AmountNZD          *float64 `json:"amount_nzd"`
	SuppressionFlag    bool     `json:"suppression_flag"`
	CleanSlatePossible bool     `json:"clean_slate_possible"`
}

// FraudExplorerRow is one case prepared for the explorer table / CSV.
type FraudExplorerRow struct {
	ID           int64
	Status       string
	ReviewState  string
	NamingOK     bool // post-verdict and no flagged participant (editorial gate NOT included)
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
	Summary      string

	OffendingStart *time.Time
	OffendingEnd   *time.Time
	ChargesDate    *time.Time
	ConvictionDate *time.Time

	OffenceToConvictionDays *int
	ChargeToConvictionDays  *int
	LagApproximate          bool

	Participants []FraudExplorerParticipant
	SourceURLs   []string
}

// SearchFraudCases runs the explorer query. Total is the unlimited match
// count for the pager.
func (d *Store) SearchFraudCases(ctx context.Context, f FraudExplorerFilter) ([]FraudExplorerRow, int, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	rows, err := d.Pool.Query(ctx, `
		WITH parts AS (
			SELECT cp.case_id,
			       jsonb_agg(jsonb_build_object(
			           'name', p.full_name, 'role', cp.role,
			           'profession', cp.profession_raw, 'charges', cp.individual_charges,
			           'outcome', COALESCE(cp.outcome, ''),
			           'sentence_type', COALESCE(cp.sentence_type, ''),
			           'sentence_detail', cp.sentence_detail,
			           'sentence_months', cp.sentence_months,
			           'amount_nzd', cp.individual_amount_nzd,
			           'suppression_flag', cp.suppression_flag,
			           'clean_slate_possible', cp.clean_slate_possible
			       ) ORDER BY cp.id)                                   AS participants,
			       bool_or(cp.suppression_flag OR cp.clean_slate_possible) AS any_flagged,
			       -- Searchable text, split by whether it identifies anyone:
			       -- charges always, names and professions only where the
			       -- row may show them (see the WHERE clause).
			       string_agg(lower(COALESCE(cp.individual_charges, '')), ' ') AS charge_text,
			       string_agg(lower(concat_ws(' ', p.full_name, cp.profession_raw)), ' ') AS ident_text
			  FROM fraud_case_participants cp
			  JOIN fraud_persons p ON p.id = cp.person_id
			 GROUP BY cp.case_id
		), srcs AS (
			SELECT case_id, array_agg(DISTINCT url) FILTER (WHERE url <> '') AS urls
			  FROM fraud_sources GROUP BY case_id
		), sec AS (
			SELECT ct.case_id, array_agg(t.label ORDER BY t.label) AS labels
			  FROM fraud_case_typologies ct JOIN fraud_typologies t ON t.id = ct.typology_id
			 GROUP BY ct.case_id
		)
		SELECT c.id, c.status, c.review_state, nk.naming_ok,
		       COALESCE(t.label, ''), COALESCE(sec.labels, '{}'),
		       COALESCE(s.label, ''), COALESCE(r.label, ''), COALESCE(a.label, ''), c.agency_basis,
		       c.typology_raw, c.court_name, COALESCE(c.victim_class, ''), c.victim_detail,
		       c.covid_related, c.total_amount_nzd, COALESCE(c.amount_basis, ''), c.summary,
		       c.offending_period_start, c.offending_period_end,
		       l.charges_date, l.conviction_date,
		       l.offence_to_conviction_days, l.charge_to_conviction_days,
		       COALESCE(l.approximate, false),
		       COALESCE(parts.participants, '[]'::jsonb), COALESCE(srcs.urls, '{}'),
		       count(*) OVER ()
		  FROM fraud_cases c
		  LEFT JOIN fraud_typologies t ON t.id = c.typology_id
		  LEFT JOIN fraud_sectors    s ON s.id = c.sector_id
		  LEFT JOIN fraud_regions    r ON r.id = c.region_id
		  LEFT JOIN fraud_agencies   a ON a.id = c.prosecuting_agency_id
		  LEFT JOIN fraud_case_lags  l ON l.case_id = c.id
		  LEFT JOIN parts ON parts.case_id = c.id
		  LEFT JOIN srcs  ON srcs.case_id = c.id
		  LEFT JOIN sec   ON sec.case_id = c.id
		  -- THE naming rule (mirror of readmodel.NamingAllowed): not
		  -- reviewer-blocked, no flagged participant, and pre-verdict
		  -- only when an official agency release names the matter.
		  CROSS JOIN LATERAL (SELECT
		       c.naming_approved
		           AND NOT COALESCE(parts.any_flagged, false)
		           AND (NOT c.status IN ('charges_laid','before_court')
		                OR EXISTS (SELECT 1 FROM fraud_sources fs
		                            WHERE fs.case_id = c.id AND fs.source_type = 'agency_release')) AS naming_ok) nk
		 WHERE ($1 = '' OR t.slug = $1)
		   AND ($2 = '' OR a.slug = $2)
		   AND ($3 = '' OR r.slug = $3)
		   AND ($4 = '' OR s.slug = $4)
		   AND ($5 = '' OR c.status = $5)
		   AND ($6 = '' OR c.victim_class = $6)
		   AND (NOT $7 OR c.covid_related)
		   AND ($8 = '' OR c.review_state = $8)
		   AND ($9::float8 = 0 OR c.total_amount_nzd >= $9)
		   AND ($10::int = 0 OR EXTRACT(YEAR FROM COALESCE(l.conviction_date, l.charges_date, c.created_at)) >= $10)
		   AND ($11::int = 0 OR EXTRACT(YEAR FROM COALESCE(l.conviction_date, l.charges_date, c.created_at)) <= $11)
		   -- The keyword follows the naming rule: summaries, participant
		   -- names and professions match only on rows whose names may be
		   -- shown, and never with naming off ($15), consistent with what
		   -- the row displays. Everything else is displayed anyway.
		   AND ($12 = '' OR
		        lower(concat_ws(' ', c.typology_raw, c.agency_raw,
		              c.court_name, c.victim_detail)) LIKE '%' || lower($12) || '%'
		        OR COALESCE(parts.charge_text, '') LIKE '%' || lower($12) || '%'
		        OR (NOT $15 AND nk.naming_ok AND (lower(COALESCE(c.summary, '')) LIKE '%' || lower($12) || '%'
		            OR COALESCE(parts.ident_text, '') LIKE '%' || lower($12) || '%')))
		 ORDER BY COALESCE(l.conviction_date, l.charges_date) DESC NULLS LAST, c.id DESC
		 LIMIT $13 OFFSET $14`,
		f.TypologySlug, f.AgencySlug, f.RegionSlug, f.SectorSlug, f.Status,
		f.VictimClass, f.CovidOnly, f.ReviewState, f.MinAmountNZD,
		f.YearFrom, f.YearTo, f.Query, f.Limit, f.Offset, f.NoIdentitySearch)
	if err != nil {
		return nil, 0, fmt.Errorf("search fraud cases: %w", err)
	}
	defer rows.Close()

	var out []FraudExplorerRow
	total := 0
	for rows.Next() {
		var row FraudExplorerRow
		if err := rows.Scan(&row.ID, &row.Status, &row.ReviewState, &row.NamingOK,
			&row.Typology, &row.Secondary,
			&row.Sector, &row.Region, &row.Agency, &row.AgencyBasis,
			&row.TypologyRaw, &row.CourtName, &row.VictimClass, &row.VictimDetail,
			&row.CovidRelated, &row.AmountNZD, &row.AmountBasis, &row.Summary,
			&row.OffendingStart, &row.OffendingEnd,
			&row.ChargesDate, &row.ConvictionDate,
			&row.OffenceToConvictionDays, &row.ChargeToConvictionDays,
			&row.LagApproximate,
			&row.Participants, &row.SourceURLs,
			&total); err != nil {
			return nil, 0, fmt.Errorf("scan explorer row: %w", err)
		}
		out = append(out, row)
	}
	return out, total, rows.Err()
}
