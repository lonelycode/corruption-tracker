// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

// Corruption & fraud case tracker persistence — cases, participants,
// persons/orgs, lifecycle events, sources, taxonomies. ALL writes to the
// fraud_* case tables go through this file. Case status is always derived
// (fraudtracker.DeriveCaseStatus) inside the same transaction as any event
// write; nothing ever hand-sets it. The claims review queue lives in
// fraudclaims.go.

// normalizeFraudPersonName trims and collapses whitespace, then strips a
// single trailing token that exactly duplicates the token before it
// (case-insensitive). The extraction step occasionally echoes the surname —
// "Kiri-Ann Fenwick Fenwick", "Rowan Ellis McTavish McTavish" — and that doubled
// form was reaching the explorer table and CSV export verbatim. Conservative
// by design: only the final whitespace-delimited token, only when it equals
// its predecessor, so legitimate multi-word names are untouched.
func normalizeFraudPersonName(fullName string) string {
	name := strings.Join(strings.Fields(fullName), " ")
	fields := strings.Fields(name)
	if n := len(fields); n >= 2 && strings.EqualFold(fields[n-1], fields[n-2]) {
		name = strings.Join(fields[:n-1], " ")
	}
	return name
}

// fraudTaxonomyTables whitelists the kind → table mapping so no caller can
// inject a table name.
var fraudTaxonomyTables = map[string]string{
	"typology":            "fraud_typologies",
	"agency":              "fraud_agencies",
	"region":              "fraud_regions",
	"profession_category": "fraud_profession_categories",
	"sector":              "fraud_sectors",
}

// LoadFraudTaxonomy returns all terms of one taxonomy kind ("typology",
// "agency", "region", "profession_category", "sector"), active and inactive
// (resolvers skip inactive terms themselves).
func (d *Store) LoadFraudTaxonomy(ctx context.Context, kind string) ([]domain.FraudTaxonomyTerm, error) {
	table, ok := fraudTaxonomyTables[kind]
	if !ok {
		return nil, fmt.Errorf("unknown fraud taxonomy kind %q", kind)
	}
	rows, err := d.Pool.Query(ctx, `SELECT id, slug, label, aliases, active, sort FROM `+table+` ORDER BY sort, slug`)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", table, err)
	}
	defer rows.Close()
	var out []domain.FraudTaxonomyTerm
	for rows.Next() {
		var t domain.FraudTaxonomyTerm
		if err := rows.Scan(&t.ID, &t.Slug, &t.Label, &t.Aliases, &t.Active, &t.Sort); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FindOrCreateFraudPerson matches on exact lowercased full name (or alias)
// and creates a minimal-PII row when no match exists. Returns (id, created).
// Deliberately no fuzzy matching: ambiguous identity is a reviewer decision,
// not a resolver guess.
func (d *Store) FindOrCreateFraudPerson(ctx context.Context, fullName string, aliases []string) (int64, bool, error) {
	name := normalizeFraudPersonName(fullName)
	if name == "" {
		return 0, false, errors.New("fraud person requires a name")
	}
	var id int64
	err := d.Pool.QueryRow(ctx, `
		SELECT id FROM fraud_persons
		 WHERE lower(full_name) = lower($1)
		    OR aliases @> to_jsonb(ARRAY[lower($1)])
		 ORDER BY id LIMIT 1`, name).Scan(&id)
	switch {
	case err == nil:
		return id, false, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to insert
	default:
		return 0, false, fmt.Errorf("find fraud person: %w", err)
	}
	lowered := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			lowered = append(lowered, a)
		}
	}
	err = d.Pool.QueryRow(ctx, `
		INSERT INTO fraud_persons (full_name, aliases)
		VALUES ($1, to_jsonb($2::text[]))
		RETURNING id`, name, lowered).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("insert fraud person: %w", err)
	}
	return id, true, nil
}

// FindOrCreateFraudOrganisation matches on exact lowercased name.
func (d *Store) FindOrCreateFraudOrganisation(ctx context.Context, name string) (int64, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, errors.New("fraud organisation requires a name")
	}
	var id int64
	err := d.Pool.QueryRow(ctx, `
		SELECT id FROM fraud_organisations WHERE lower(name)=lower($1) ORDER BY id LIMIT 1`, name).Scan(&id)
	switch {
	case err == nil:
		return id, false, nil
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return 0, false, fmt.Errorf("find fraud org: %w", err)
	}
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO fraud_organisations (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("insert fraud org: %w", err)
	}
	return id, true, nil
}

// FraudParticipantInput pairs a participant row with the person identity it
// belongs to; the person is found-or-created inside the case transaction.
type FraudParticipantInput struct {
	PersonName    string
	PersonAliases []string
	Participant   domain.FraudCaseParticipant // PersonID/CaseID filled in
}

// FraudOrgInput links an organisation by name with its per-case role.
type FraudOrgInput struct {
	Name string
	Role string // vehicle | victim | employer_of_offender | regulator
}

// FraudCaseInput is the composite payload for creating a case with its
// participants, organisations, events, and sources in one transaction.
type FraudCaseInput struct {
	Case                 domain.FraudCase
	SecondaryTypologyIDs []int64
	CoAgencyIDs          []int64
	Participants         []FraudParticipantInput
	Organisations        []FraudOrgInput
	Events               []domain.FraudCaseEvent
	Sources              []domain.FraudCaseSource
}

const fraudCaseCols = `id, case_name, status, typology_id, typology_raw, sector_id,
	region_id, region_raw, prosecuting_agency_id, agency_raw,
	total_amount_nzd, amount_basis, amount_note,
	offending_period_start, offending_period_end, offending_period_precision,
	court_name, court_level, court_identifiers, victim_class, victim_detail, victim_count,
	covid_related, summary, naming_approved, review_state, verified_by, verified_at,
	status_overridden, created_at, updated_at`

func scanFraudCase(row pgx.Row) (domain.FraudCase, error) {
	var c domain.FraudCase
	var status string
	err := row.Scan(&c.ID, &c.CaseName, &status, &c.TypologyID, &c.TypologyRaw, &c.SectorID,
		&c.RegionID, &c.RegionRaw, &c.ProsecutingAgencyID, &c.AgencyRaw,
		&c.TotalAmountNZD, &c.AmountBasis, &c.AmountNote,
		&c.OffendingPeriodStart, &c.OffendingPeriodEnd, &c.OffendingPeriodPrecision,
		&c.CourtName, &c.CourtLevel, &c.CourtIdentifiers, &c.VictimClass, &c.VictimDetail, &c.VictimCount,
		&c.CovidRelated, &c.Summary, &c.NamingApproved, &c.ReviewState, &c.VerifiedBy, &c.VerifiedAt,
		&c.StatusOverridden, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	c.Status = domain.FraudCaseStatus(status)
	return c, nil
}

// CreateFraudCase inserts a case plus all its satellites atomically. Status
// is derived from the supplied events (never taken from in.Case.Status).
// Persons and organisations are found-or-created by exact name inside the
// transaction.
func (d *Store) CreateFraudCase(ctx context.Context, in FraudCaseInput) (domain.FraudCase, error) {
	var out domain.FraudCase
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	c := in.Case
	if c.CourtIdentifiers == nil {
		c.CourtIdentifiers = []string{}
	}
	// naming_approved is deliberately NOT in the column list: new cases take
	// the column default (true since 0128 — names-by-default behind the org
	// gate). Blocking naming is a post-creation reviewer action.
	out, err = scanFraudCase(tx.QueryRow(ctx, `
		INSERT INTO fraud_cases
			(case_name, typology_id, typology_raw, sector_id, region_id, region_raw,
			 prosecuting_agency_id, agency_raw, total_amount_nzd, amount_basis, amount_note,
			 offending_period_start, offending_period_end, offending_period_precision,
			 court_name, court_level, court_identifiers, victim_class, victim_detail, victim_count,
			 covid_related, summary, review_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
		        COALESCE(NULLIF($23,''),'unverified'))
		RETURNING `+fraudCaseCols,
		c.CaseName, c.TypologyID, c.TypologyRaw, c.SectorID, c.RegionID, c.RegionRaw,
		c.ProsecutingAgencyID, c.AgencyRaw, c.TotalAmountNZD, c.AmountBasis, c.AmountNote,
		c.OffendingPeriodStart, c.OffendingPeriodEnd, c.OffendingPeriodPrecision,
		c.CourtName, c.CourtLevel, c.CourtIdentifiers, c.VictimClass, c.VictimDetail, c.VictimCount,
		c.CovidRelated, c.Summary, c.ReviewState))
	if err != nil {
		return out, fmt.Errorf("insert fraud case: %w", err)
	}

	for _, tid := range in.SecondaryTypologyIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_typologies (case_id, typology_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, out.ID, tid); err != nil {
			return out, fmt.Errorf("link secondary typology: %w", err)
		}
	}
	for _, aid := range in.CoAgencyIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_agencies (case_id, agency_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, out.ID, aid); err != nil {
			return out, fmt.Errorf("link co-agency: %w", err)
		}
	}

	for _, p := range in.Participants {
		personID, err := findOrCreateFraudPersonTx(ctx, tx, p.PersonName, p.PersonAliases)
		if err != nil {
			return out, err
		}
		pp := p.Participant
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_participants
				(case_id, person_id, role, profession_raw, profession_category_id, gender,
				 individual_charges, individual_amount_nzd, outcome, sentence_type,
				 sentence_detail, sentence_months, suppression_flag, clean_slate_possible)
			VALUES ($1,$2,COALESCE(NULLIF($3,''),'defendant'),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (case_id, person_id) DO NOTHING`,
			out.ID, personID, pp.Role, pp.ProfessionRaw, pp.ProfessionCategoryID, pp.Gender,
			pp.IndividualCharges, pp.IndividualAmountNZD, pp.Outcome, pp.SentenceType,
			pp.SentenceDetail, pp.SentenceMonths, pp.SuppressionFlag, pp.CleanSlatePossible); err != nil {
			return out, fmt.Errorf("insert participant: %w", err)
		}
	}

	for _, o := range in.Organisations {
		orgID, err := findOrCreateFraudOrgTx(ctx, tx, o.Name)
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_organisations (case_id, organisation_id, role)
			VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, out.ID, orgID, o.Role); err != nil {
			return out, fmt.Errorf("link organisation: %w", err)
		}
	}

	for _, s := range in.Sources {
		if _, err := attachFraudSourceTx(ctx, tx, out.ID, s); err != nil {
			return out, err
		}
	}

	for _, e := range in.Events {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_events
				(case_id, event_type, event_date, event_date_precision, court, outcome_detail, source_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			out.ID, string(e.EventType), e.EventDate, e.EventDatePrecision, e.Court, e.OutcomeDetail, e.SourceID); err != nil {
			return out, fmt.Errorf("insert event: %w", err)
		}
	}

	if out, err = recomputeFraudCaseStatusTx(ctx, tx, out.ID); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

func findOrCreateFraudPersonTx(ctx context.Context, tx pgx.Tx, fullName string, aliases []string) (int64, error) {
	name := normalizeFraudPersonName(fullName)
	if name == "" {
		return 0, errors.New("fraud person requires a name")
	}
	var id int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM fraud_persons
		 WHERE lower(full_name) = lower($1)
		    OR aliases @> to_jsonb(ARRAY[lower($1)])
		 ORDER BY id LIMIT 1`, name).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return 0, fmt.Errorf("find fraud person: %w", err)
	}
	lowered := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			lowered = append(lowered, a)
		}
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO fraud_persons (full_name, aliases)
		VALUES ($1, to_jsonb($2::text[])) RETURNING id`, name, lowered).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert fraud person: %w", err)
	}
	return id, nil
}

func findOrCreateFraudOrgTx(ctx context.Context, tx pgx.Tx, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("fraud organisation requires a name")
	}
	var id int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM fraud_organisations WHERE lower(name)=lower($1) ORDER BY id LIMIT 1`, name).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return 0, fmt.Errorf("find fraud org: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO fraud_organisations (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert fraud org: %w", err)
	}
	return id, nil
}

func attachFraudSourceTx(ctx context.Context, tx pgx.Tx, caseID int64, s domain.FraudCaseSource) (int64, error) {
	if strings.TrimSpace(s.ContentHash) == "" {
		return 0, errors.New("fraud source requires a content_hash (provenance)")
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO fraud_sources
			(case_id, url, publisher, source_type, licence_basis, published_date,
			 retrieved_at, excerpt, content_hash)
		VALUES ($1,$2,$3,$4,$5,$6,COALESCE($7, now()),$8,$9)
		ON CONFLICT (case_id, content_hash) DO NOTHING
		RETURNING id`,
		caseID, s.URL, s.Publisher, string(s.SourceType), s.LicenceBasis, s.PublishedDate,
		nullableTime(s.RetrievedAt), s.Excerpt, s.ContentHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Identical capture already attached; return the existing row id.
		if err := tx.QueryRow(ctx, `
			SELECT id FROM fraud_sources WHERE case_id=$1 AND content_hash=$2`,
			caseID, s.ContentHash).Scan(&id); err != nil {
			return 0, fmt.Errorf("fetch existing fraud source: %w", err)
		}
		return id, nil
	}
	if err != nil {
		return 0, fmt.Errorf("insert fraud source: %w", err)
	}
	return id, nil
}

// AttachFraudCaseSource adds provenance to an existing case (idempotent on
// content_hash). Returns the source id.
func (d *Store) AttachFraudCaseSource(ctx context.Context, caseID int64, s domain.FraudCaseSource) (int64, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := attachFraudSourceTx(ctx, tx, caseID, s)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// AddFraudCaseEvent appends a lifecycle event (optionally with the source
// that evidences it) and re-derives the case status in the same transaction.
// This is the ONLY path by which case status changes.
func (d *Store) AddFraudCaseEvent(ctx context.Context, caseID int64, e domain.FraudCaseEvent, src *domain.FraudCaseSource) (domain.FraudCase, error) {
	var out domain.FraudCase
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM fraud_cases WHERE id=$1 FOR UPDATE`, caseID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrRowMissing
		}
		return out, fmt.Errorf("lock case: %w", err)
	}

	var sourceID *int64
	if src != nil {
		id, err := attachFraudSourceTx(ctx, tx, caseID, *src)
		if err != nil {
			return out, err
		}
		sourceID = &id
	} else {
		sourceID = e.SourceID
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO fraud_case_events
			(case_id, event_type, event_date, event_date_precision, court, outcome_detail, source_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		caseID, string(e.EventType), e.EventDate, e.EventDatePrecision, e.Court, e.OutcomeDetail, sourceID); err != nil {
		return out, fmt.Errorf("insert event: %w", err)
	}

	if out, err = recomputeFraudCaseStatusTx(ctx, tx, caseID); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// recomputeFraudCaseStatusTx re-derives status from the full event list and
// persists it, returning the fresh case row.
func recomputeFraudCaseStatusTx(ctx context.Context, tx pgx.Tx, caseID int64) (domain.FraudCase, error) {
	// A manually-overridden status is sticky: skip derivation entirely so an
	// event edit, recode, or duplicate merge can't silently clobber the human
	// value. Re-derivation resumes once an operator reverts the override.
	var overridden bool
	if err := tx.QueryRow(ctx, `SELECT status_overridden FROM fraud_cases WHERE id=$1`, caseID).Scan(&overridden); err != nil {
		return domain.FraudCase{}, fmt.Errorf("read status override: %w", err)
	}
	if overridden {
		out, err := scanFraudCase(tx.QueryRow(ctx, `
			SELECT `+fraudCaseCols+` FROM fraud_cases WHERE id=$1`, caseID))
		if err != nil {
			return out, fmt.Errorf("read overridden case: %w", err)
		}
		return out, nil
	}
	events, err := listFraudCaseEventsTx(ctx, tx, caseID)
	if err != nil {
		return domain.FraudCase{}, err
	}
	status := fraudtracker.DeriveCaseStatus(events)
	out, err := scanFraudCase(tx.QueryRow(ctx, `
		UPDATE fraud_cases SET status=$2, updated_at=now()
		 WHERE id=$1 RETURNING `+fraudCaseCols, caseID, string(status)))
	if err != nil {
		return out, fmt.Errorf("update case status: %w", err)
	}
	return out, nil
}

func listFraudCaseEventsTx(ctx context.Context, tx pgx.Tx, caseID int64) ([]domain.FraudCaseEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, case_id, event_type, event_date, event_date_precision, court,
		       outcome_detail, source_id, created_at
		  FROM fraud_case_events WHERE case_id=$1 ORDER BY id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	return scanFraudCaseEvents(rows)
}

func scanFraudCaseEvents(rows pgx.Rows) ([]domain.FraudCaseEvent, error) {
	var out []domain.FraudCaseEvent
	for rows.Next() {
		var e domain.FraudCaseEvent
		var typ string
		if err := rows.Scan(&e.ID, &e.CaseID, &typ, &e.EventDate, &e.EventDatePrecision,
			&e.Court, &e.OutcomeDetail, &e.SourceID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.EventType = domain.FraudEventType(typ)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetFraudCase returns a case with participants, events, organisations, and
// sources. Returns ErrRowMissing if not found.
func (d *Store) GetFraudCase(ctx context.Context, id int64) (domain.FraudCase, []domain.FraudCaseParticipant, []domain.FraudCaseEvent, []domain.FraudCaseSource, error) {
	c, err := scanFraudCase(d.Pool.QueryRow(ctx, `
		SELECT `+fraudCaseCols+` FROM fraud_cases WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil, nil, nil, ErrRowMissing
	}
	if err != nil {
		return c, nil, nil, nil, fmt.Errorf("get fraud case: %w", err)
	}

	parts, err := d.ListFraudCaseParticipants(ctx, id)
	if err != nil {
		return c, nil, nil, nil, err
	}

	evRows, err := d.Pool.Query(ctx, `
		SELECT id, case_id, event_type, event_date, event_date_precision, court,
		       outcome_detail, source_id, created_at
		  FROM fraud_case_events WHERE case_id=$1 ORDER BY id`, id)
	if err != nil {
		return c, nil, nil, nil, fmt.Errorf("list events: %w", err)
	}
	events, err := scanFraudCaseEvents(evRows)
	evRows.Close()
	if err != nil {
		return c, nil, nil, nil, err
	}

	srcRows, err := d.Pool.Query(ctx, `
		SELECT id, case_id, url, publisher, source_type, licence_basis, published_date,
		       retrieved_at, excerpt, content_hash, created_at
		  FROM fraud_sources WHERE case_id=$1 ORDER BY id`, id)
	if err != nil {
		return c, nil, nil, nil, fmt.Errorf("list sources: %w", err)
	}
	defer srcRows.Close()
	var sources []domain.FraudCaseSource
	for srcRows.Next() {
		var s domain.FraudCaseSource
		var typ string
		if err := srcRows.Scan(&s.ID, &s.CaseID, &s.URL, &s.Publisher, &typ, &s.LicenceBasis,
			&s.PublishedDate, &s.RetrievedAt, &s.Excerpt, &s.ContentHash, &s.CreatedAt); err != nil {
			return c, nil, nil, nil, fmt.Errorf("scan source: %w", err)
		}
		s.SourceType = domain.FraudSourceType(typ)
		sources = append(sources, s)
	}
	return c, parts, events, sources, srcRows.Err()
}

// ListFraudCaseParticipants returns a case's participants with names joined.
func (d *Store) ListFraudCaseParticipants(ctx context.Context, caseID int64) ([]domain.FraudCaseParticipant, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT cp.id, cp.case_id, cp.person_id, cp.role, cp.profession_raw,
		       cp.profession_category_id, cp.gender, cp.individual_charges,
		       cp.individual_amount_nzd, cp.outcome, cp.sentence_type, cp.sentence_detail,
		       cp.sentence_months, cp.suppression_flag, cp.clean_slate_possible,
		       cp.created_at, cp.updated_at, p.full_name
		  FROM fraud_case_participants cp
		  JOIN fraud_persons p ON p.id = cp.person_id
		 WHERE cp.case_id=$1 ORDER BY cp.id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list participants: %w", err)
	}
	defer rows.Close()
	var out []domain.FraudCaseParticipant
	for rows.Next() {
		var p domain.FraudCaseParticipant
		var amount *float64
		if err := rows.Scan(&p.ID, &p.CaseID, &p.PersonID, &p.Role, &p.ProfessionRaw,
			&p.ProfessionCategoryID, &p.Gender, &p.IndividualCharges,
			&amount, &p.Outcome, &p.SentenceType, &p.SentenceDetail,
			&p.SentenceMonths, &p.SuppressionFlag, &p.CleanSlatePossible,
			&p.CreatedAt, &p.UpdatedAt, &p.PersonName); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		p.IndividualAmountNZD = amount
		out = append(out, p)
	}
	return out, rows.Err()
}

// FraudCaseFilter narrows ListFraudCases.
type FraudCaseFilter struct {
	Status      string
	ReviewState string
	TypologyID  *int64
	Limit       int
	Offset      int
}

// ListFraudCases returns cases newest-first with optional filters.
func (d *Store) ListFraudCases(ctx context.Context, f FraudCaseFilter) ([]domain.FraudCase, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := d.Pool.Query(ctx, `
		SELECT `+fraudCaseCols+`
		  FROM fraud_cases
		 WHERE ($1 = '' OR status = $1)
		   AND ($2 = '' OR review_state = $2)
		   AND ($3::bigint IS NULL OR typology_id = $3)
		 ORDER BY id DESC
		 LIMIT $4 OFFSET $5`,
		f.Status, f.ReviewState, f.TypologyID, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("list fraud cases: %w", err)
	}
	defer rows.Close()
	var out []domain.FraudCase
	for rows.Next() {
		c, err := scanFraudCase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan fraud case: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// VerifyFraudCase records reviewer sign-off on a case (the post-import or
// post-promotion attestation pass).
func (d *Store) VerifyFraudCase(ctx context.Context, caseID int64, reviewerID int64) (domain.FraudCase, error) {
	out, err := scanFraudCase(d.Pool.QueryRow(ctx, `
		UPDATE fraud_cases
		   SET review_state='verified', verified_by=$2, verified_at=now(), updated_at=now()
		 WHERE id=$1 RETURNING `+fraudCaseCols, caseID, reviewerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRowMissing
	}
	if err != nil {
		return out, fmt.Errorf("verify fraud case: %w", err)
	}
	return out, nil
}

// SetFraudCaseNamingApproved flips the explicit reviewer naming decision.
func (d *Store) SetFraudCaseNamingApproved(ctx context.Context, caseID int64, approved bool) error {
	tag, err := d.Pool.Exec(ctx, `
		UPDATE fraud_cases SET naming_approved=$2, updated_at=now() WHERE id=$1`, caseID, approved)
	if err != nil {
		return fmt.Errorf("set naming approved: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRowMissing
	}
	return nil
}
