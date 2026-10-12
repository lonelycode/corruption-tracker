// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lonelycode/corruption-tracker/domain"
)

// Machine recode persistence (migration 0127). The recode pass fills coding
// gaps on existing cases — taxonomy IDs, offending periods, undated event
// dates — and NEVER overwrites a value that is already set: every update
// below is COALESCE-shaped so human coding always wins. Status is re-derived
// whenever an event date is filled (date order is what status derivation
// reads).

// ListFraudRecodeTargets returns case IDs the recode pass should attempt:
// any coding gap, not yet attempted (recoded_at IS NULL) unless retry is set.
func (d *Store) ListFraudRecodeTargets(ctx context.Context, limit int, retry bool) ([]int64, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT c.id FROM fraud_cases c
		 WHERE ($2 OR c.recoded_at IS NULL)
		   AND (c.typology_id IS NULL
		        OR c.prosecuting_agency_id IS NULL
		        OR c.region_id IS NULL
		        OR c.sector_id IS NULL
		        OR c.offending_period_start IS NULL
		        OR EXISTS (SELECT 1 FROM fraud_case_events e
		                    WHERE e.case_id = c.id
		                      AND e.event_type IN ('charges_laid', 'verdict', 'sentencing')
		                      AND e.event_date IS NULL)
		        OR EXISTS (SELECT 1 FROM fraud_case_participants cp
		                    WHERE cp.case_id = c.id AND btrim(cp.individual_charges) = ''))
		 ORDER BY c.id
		 LIMIT $1`, limit, retry)
	if err != nil {
		return nil, fmt.Errorf("list recode targets: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recode target: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FraudRecodePatch carries the fields the recode pass decided. Nil fields
// are not touched; non-nil fields fill only NULL columns.
type FraudRecodePatch struct {
	TypologyID           *int64
	SecondaryTypologyIDs []int64
	AgencyID             *int64
	AgencyBasis          string // 'stated' | 'inferred'; required when AgencyID set
	RegionID             *int64
	SectorID             *int64

	OffendingStart     *time.Time
	OffendingEnd       *time.Time
	OffendingPrecision *string

	// Fill the earliest UNDATED event of each kind; never moves a dated one.
	ConvictionDate      *time.Time
	ConvictionPrecision string
	ChargesDate         *time.Time
	ChargesPrecision    string

	// Charge descriptions per named participant; applied by exact
	// (case-insensitive) person-name match, empty columns only.
	ParticipantCharges []FraudChargeFill
}

// FraudChargeFill is one participant's charge text recovered from a source.
type FraudChargeFill struct {
	Name    string
	Charges string
}

// ApplyFraudRecode applies a recode patch in one transaction and stamps
// recoded_at (also when the patch is empty — an attempted-but-uncodable case
// must not be retried forever). Returns the fresh case row.
func (d *Store) ApplyFraudRecode(ctx context.Context, caseID int64, p FraudRecodePatch) (domain.FraudCase, error) {
	var out domain.FraudCase
	if p.AgencyID != nil && p.AgencyBasis != "stated" && p.AgencyBasis != "inferred" {
		return out, errors.New("recode: agency fill requires basis 'stated' or 'inferred'")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out, err = scanFraudCase(tx.QueryRow(ctx, `
		UPDATE fraud_cases SET
			typology_id              = COALESCE(typology_id, $2),
			agency_basis             = CASE WHEN prosecuting_agency_id IS NULL AND $3::bigint IS NOT NULL
			                                THEN $4 ELSE agency_basis END,
			prosecuting_agency_id    = COALESCE(prosecuting_agency_id, $3),
			region_id                = COALESCE(region_id, $5),
			sector_id                = COALESCE(sector_id, $6),
			offending_period_end     = CASE WHEN offending_period_start IS NULL THEN $8 ELSE offending_period_end END,
			offending_period_precision = CASE WHEN offending_period_start IS NULL AND $7::date IS NOT NULL
			                                  THEN $9 ELSE offending_period_precision END,
			offending_period_start   = COALESCE(offending_period_start, $7),
			recoded_at               = now(),
			updated_at               = now()
		 WHERE id = $1
		 RETURNING `+fraudCaseCols,
		caseID, p.TypologyID, p.AgencyID, p.AgencyBasis, p.RegionID, p.SectorID,
		p.OffendingStart, p.OffendingEnd, p.OffendingPrecision))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRowMissing
	}
	if err != nil {
		return out, fmt.Errorf("recode case: %w", err)
	}

	for _, tid := range p.SecondaryTypologyIDs {
		// The M2M holds secondary typologies only; skip the primary.
		if out.TypologyID != nil && *out.TypologyID == tid {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_typologies (case_id, typology_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, caseID, tid); err != nil {
			return out, fmt.Errorf("recode secondary typology: %w", err)
		}
	}

	datedEvent := false
	for _, fill := range []struct {
		date      *time.Time
		precision string
		types     []string
	}{
		{p.ConvictionDate, p.ConvictionPrecision, []string{"verdict", "sentencing"}},
		{p.ChargesDate, p.ChargesPrecision, []string{"charges_laid"}},
	} {
		if fill.date == nil {
			continue
		}
		prec := fill.precision
		if prec == "" {
			prec = "day"
		}
		tag, err := tx.Exec(ctx, `
			UPDATE fraud_case_events SET event_date=$2, event_date_precision=$3
			 WHERE id = (SELECT id FROM fraud_case_events
			              WHERE case_id=$1 AND event_type = ANY($4) AND event_date IS NULL
			              ORDER BY id LIMIT 1)`,
			caseID, fill.date, prec, fill.types)
		if err != nil {
			return out, fmt.Errorf("recode event date: %w", err)
		}
		if tag.RowsAffected() > 0 {
			datedEvent = true
		}
	}
	if datedEvent {
		if out, err = recomputeFraudCaseStatusTx(ctx, tx, caseID); err != nil {
			return out, err
		}
	}

	for _, f := range p.ParticipantCharges {
		if _, err := tx.Exec(ctx, `
			UPDATE fraud_case_participants cp
			   SET individual_charges = $3, updated_at = now()
			  FROM fraud_persons per
			 WHERE cp.case_id = $1 AND cp.person_id = per.id
			   AND lower(per.full_name) = lower($2)
			   AND btrim(cp.individual_charges) = ''`,
			caseID, f.Name, f.Charges); err != nil {
			return out, fmt.Errorf("recode participant charges: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// AddFraudTaxonomyAlias appends a normalised alias to a taxonomy term so the
// deterministic resolver self-heals (idempotent; no-op when present).
func (d *Store) AddFraudTaxonomyAlias(ctx context.Context, kind string, termID int64, alias string) error {
	table, ok := fraudTaxonomyTables[kind]
	if !ok {
		return fmt.Errorf("unknown fraud taxonomy kind %q", kind)
	}
	if alias == "" {
		return errors.New("empty alias")
	}
	_, err := d.Pool.Exec(ctx, `
		UPDATE `+table+` SET aliases = array_append(aliases, $2)
		 WHERE id = $1 AND NOT ($2 = ANY(aliases))`, termID, alias)
	if err != nil {
		return fmt.Errorf("add %s alias: %w", kind, err)
	}
	return nil
}

// FraudCodingCoverage quantifies coding completeness for the admin
// dashboard: the recode pass pushes these toward Total, and any residual
// tail is the (small) human worklist.
type FraudCodingCoverage struct {
	Total           int
	TypologyCoded   int
	AgencyCoded     int
	AgencyInferred  int // subset of AgencyCoded
	RegionCoded     int
	SectorCoded     int
	OffendingPeriod int
	ConvictionDated int // cases with a dated verdict/sentencing event
	RecodeUnattempt int // cases with a gap the recode pass has not yet tried

	// Participant-denominated (the others are per case).
	Participants       int
	ParticipantCharges int // participants with a charge description
}

// GetFraudCodingCoverage computes the coverage panel in one round trip.
func (d *Store) GetFraudCodingCoverage(ctx context.Context) (FraudCodingCoverage, error) {
	var c FraudCodingCoverage
	err := d.Pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE typology_id IS NOT NULL),
		       count(*) FILTER (WHERE prosecuting_agency_id IS NOT NULL),
		       count(*) FILTER (WHERE prosecuting_agency_id IS NOT NULL AND agency_basis = 'inferred'),
		       count(*) FILTER (WHERE region_id IS NOT NULL),
		       count(*) FILTER (WHERE sector_id IS NOT NULL),
		       count(*) FILTER (WHERE offending_period_start IS NOT NULL),
		       count(*) FILTER (WHERE EXISTS (
		           SELECT 1 FROM fraud_case_events e
		            WHERE e.case_id = fraud_cases.id
		              AND e.event_type IN ('verdict','sentencing') AND e.event_date IS NOT NULL)),
		       count(*) FILTER (WHERE recoded_at IS NULL AND (
		           typology_id IS NULL OR prosecuting_agency_id IS NULL OR region_id IS NULL
		           OR sector_id IS NULL OR offending_period_start IS NULL))
		  FROM fraud_cases`).Scan(
		&c.Total, &c.TypologyCoded, &c.AgencyCoded, &c.AgencyInferred,
		&c.RegionCoded, &c.SectorCoded, &c.OffendingPeriod, &c.ConvictionDated, &c.RecodeUnattempt)
	if err != nil {
		return c, fmt.Errorf("fraud coding coverage: %w", err)
	}
	err = d.Pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE btrim(individual_charges) <> '')
		  FROM fraud_case_participants`).Scan(&c.Participants, &c.ParticipantCharges)
	if err != nil {
		return c, fmt.Errorf("fraud charge coverage: %w", err)
	}
	return c, nil
}

// FraudCaseLag is one row of the fraud_case_lags view.
type FraudCaseLag struct {
	CaseID                  int64
	OffenceEndEst           *time.Time
	ChargesDate             *time.Time
	ConvictionDate          *time.Time
	OffenceToChargeDays     *int
	OffenceToConvictionDays *int
	ChargeToConvictionDays  *int
	Approximate             bool
}

// GetFraudCaseLags returns lag rows for a set of cases.
func (d *Store) GetFraudCaseLags(ctx context.Context, caseIDs []int64) (map[int64]FraudCaseLag, error) {
	out := make(map[int64]FraudCaseLag, len(caseIDs))
	if len(caseIDs) == 0 {
		return out, nil
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT case_id, offence_end_est, charges_date, conviction_date,
		       offence_to_charge_days, offence_to_conviction_days,
		       charge_to_conviction_days, approximate
		  FROM fraud_case_lags WHERE case_id = ANY($1)`, caseIDs)
	if err != nil {
		return nil, fmt.Errorf("case lags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l FraudCaseLag
		if err := rows.Scan(&l.CaseID, &l.OffenceEndEst, &l.ChargesDate, &l.ConvictionDate,
			&l.OffenceToChargeDays, &l.OffenceToConvictionDays,
			&l.ChargeToConvictionDays, &l.Approximate); err != nil {
			return nil, fmt.Errorf("scan case lag: %w", err)
		}
		out[l.CaseID] = l
	}
	return out, rows.Err()
}
