// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
)

// Case drill-down reads: organisations linked to a case and the claims that
// fed it. Used by the admin case detail and the org-gated timeline view.

// FraudCaseOrg is one organisation link with its per-case role.
type FraudCaseOrg struct {
	OrganisationID int64
	Name           string
	NZBN           *string
	Role           string
}

// ListFraudCaseOrganisations returns a case's organisation links.
func (d *Store) ListFraudCaseOrganisations(ctx context.Context, caseID int64) ([]FraudCaseOrg, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT o.id, o.name, o.nzbn, co.role
		  FROM fraud_case_organisations co
		  JOIN fraud_organisations o ON o.id = co.organisation_id
		 WHERE co.case_id = $1
		 ORDER BY co.role, o.name`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list case orgs: %w", err)
	}
	defer rows.Close()
	var out []FraudCaseOrg
	for rows.Next() {
		var o FraudCaseOrg
		if err := rows.Scan(&o.OrganisationID, &o.Name, &o.NZBN, &o.Role); err != nil {
			return nil, fmt.Errorf("scan case org: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListFraudCaseIDsWithEventsIn returns ids of VERIFIED cases that have at
// least one dated lifecycle event inside [start, end) — the export digest's
// case set for a period.
func (d *Store) ListFraudCaseIDsWithEventsIn(ctx context.Context, start, end time.Time) ([]int64, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT DISTINCT c.id
		  FROM fraud_cases c
		  JOIN fraud_case_events e ON e.case_id = c.id
		 WHERE c.review_state = 'verified'
		   AND e.event_date >= $1 AND e.event_date < $2
		 ORDER BY c.id`, start, end)
	if err != nil {
		return nil, fmt.Errorf("list cases with events: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan case id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FraudEnrichTarget is one (case, source URL) pair eligible for the
// imported-case enrichment backfill: a spreadsheet-linked article whose
// original page can be re-fetched and extracted for richer lifecycle data.
type FraudEnrichTarget struct {
	CaseID int64
	URL    string
}

// ListFraudEnrichTargets returns linked sources from the spreadsheet import
// (publisher 'spreadsheet link'), oldest case first. The enricher dedupes
// per-URL against existing claims itself.
func (d *Store) ListFraudEnrichTargets(ctx context.Context, limit int) ([]FraudEnrichTarget, error) {
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT DISTINCT ON (s.url) s.case_id, s.url
		  FROM fraud_sources s
		 WHERE s.publisher = 'spreadsheet link' AND s.url <> ''
		 ORDER BY s.url, s.case_id
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list enrich targets: %w", err)
	}
	defer rows.Close()
	var out []FraudEnrichTarget
	for rows.Next() {
		var t FraudEnrichTarget
		if err := rows.Scan(&t.CaseID, &t.URL); err != nil {
			return nil, fmt.Errorf("scan enrich target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddFraudCaseCourtIdentifiers merges identifiers into a case's set
// (deduplicated; a case accumulates CRI then CA/SC numbers over its life).
func (d *Store) AddFraudCaseCourtIdentifiers(ctx context.Context, caseID int64, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.Pool.Exec(ctx, `
		UPDATE fraud_cases
		   SET court_identifiers = (
		         SELECT array_agg(DISTINCT x ORDER BY x)
		           FROM unnest(court_identifiers || $2::text[]) AS x),
		       updated_at = now()
		 WHERE id = $1`, caseID, ids)
	if err != nil {
		return fmt.Errorf("add court identifiers: %w", err)
	}
	return nil
}

// BackfillFraudCaseOffending fills a case's offending period ONLY where it
// is currently null — promote-time enrichment never overwrites
// reviewer-entered data.
func (d *Store) BackfillFraudCaseOffending(ctx context.Context, caseID int64, start, end *time.Time, precision *string) error {
	if start == nil && end == nil {
		return nil
	}
	_, err := d.Pool.Exec(ctx, `
		UPDATE fraud_cases
		   SET offending_period_start     = COALESCE(offending_period_start, $2),
		       offending_period_end       = COALESCE(offending_period_end, $3),
		       offending_period_precision = COALESCE(offending_period_precision, $4),
		       updated_at = now()
		 WHERE id = $1`, caseID, start, end, precision)
	if err != nil {
		return fmt.Errorf("backfill offending period: %w", err)
	}
	return nil
}

// FraudCaseLabels are the display labels for a case's coded dimensions.
// AgencyInferred marks an agency the recode pass derived from the offence
// type (agency_basis='inferred') — no source states it, so org-facing
// surfaces must not assert "prosecuted by X" for it.
type FraudCaseLabels struct {
	Typology       string
	Region         string
	Agency         string
	AgencyInferred bool
	Sector         string
}

// GetFraudCaseLabels resolves a case's taxonomy ids to labels (empty where
// uncoded).
func (d *Store) GetFraudCaseLabels(ctx context.Context, caseID int64) (FraudCaseLabels, error) {
	var l FraudCaseLabels
	err := d.Pool.QueryRow(ctx, `
		SELECT COALESCE(t.label,''), COALESCE(r.label,''), COALESCE(a.label,''),
		       c.agency_basis = 'inferred', COALESCE(s.label,'')
		  FROM fraud_cases c
		  LEFT JOIN fraud_typologies t ON t.id = c.typology_id
		  LEFT JOIN fraud_regions r    ON r.id = c.region_id
		  LEFT JOIN fraud_agencies a   ON a.id = c.prosecuting_agency_id
		  LEFT JOIN fraud_sectors s    ON s.id = c.sector_id
		 WHERE c.id = $1`, caseID).
		Scan(&l.Typology, &l.Region, &l.Agency, &l.AgencyInferred, &l.Sector)
	if err != nil {
		return l, fmt.Errorf("case labels: %w", err)
	}
	return l, nil
}

// ListFraudCaseSecondaryTypologies returns the labels of a case's secondary
// typologies (the fraud_case_typologies M2M — multi-typology offending like
// "bribery + money laundering"; the primary lives on the case row).
func (d *Store) ListFraudCaseSecondaryTypologies(ctx context.Context, caseID int64) ([]string, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT t.label
		  FROM fraud_case_typologies ct
		  JOIN fraud_typologies t ON t.id = ct.typology_id
		 WHERE ct.case_id = $1
		 ORDER BY t.label`, caseID)
	if err != nil {
		return nil, fmt.Errorf("secondary typologies: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("scan secondary typology: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListFraudClaimsByCase returns the claims attached to a case (promoted onto
// it or matched to it), newest first — the admin detail's provenance panel.
func (d *Store) ListFraudClaimsByCase(ctx context.Context, caseID int64) ([]domain.FraudClaim, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT `+fraudClaimCols+`
		  FROM fraud_claims
		 WHERE case_id = $1 AND deleted_at IS NULL
		 ORDER BY id DESC`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list claims by case: %w", err)
	}
	defer rows.Close()
	var out []domain.FraudClaim
	for rows.Next() {
		c, err := scanFraudClaim(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claim by case: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
