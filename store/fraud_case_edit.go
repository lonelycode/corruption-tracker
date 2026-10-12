// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Admin curation of fraud cases: full-field edits to a case row, edits to
// an existing participant, and the manual status override. Every mutation
// writes a matching entity_revisions row (EntityKindFraudCase) in the same
// transaction, so the case History tab can never drift from the canonical
// data. This mirrors the UpdateInterestAdmin / UpdatePersonAdmin pattern.
//
// Scope (Phase 1): case scalar fields + secondary typologies, and existing
// participants. Adding/removing participants, org links, sources, and
// lifecycle events is deliberately out of scope here.

// FraudCaseEdit is the editable scalar surface of a case. Pointer fields map
// to nullable columns (nil clears them); the plain string/bool fields map to
// their columns verbatim (empty string is stored as-is). Status is NOT here —
// it is derived, or set via the separate override path.
// NamingApproved is NOT here — it keeps its own suppression-attestation flow.
type FraudCaseEdit struct {
	CaseName string

	TypologyID           *int64
	TypologyRaw          string
	SecondaryTypologyIDs []int64
	SectorID             *int64
	RegionID             *int64
	RegionRaw            string
	ProsecutingAgencyID  *int64
	AgencyRaw            string

	TotalAmountNZD *float64
	AmountBasis    *string // alleged | proven | reparations_ordered | nil
	AmountNote     string

	OffendingPeriodStart     *time.Time
	OffendingPeriodEnd       *time.Time
	OffendingPeriodPrecision *string // day | month | year | nil

	CourtName        string
	CourtLevel       string
	CourtIdentifiers []string

	VictimClass  *string // enum or nil
	VictimDetail string
	VictimCount  *int

	CovidRelated bool
	Summary      string
}

// snapshotFraudCaseEditTx reads the current editable surface of a case
// (including its secondary typologies) for the before-image of a revision.
func snapshotFraudCaseEditTx(ctx context.Context, tx pgx.Tx, id int64) (FraudCaseEdit, error) {
	var e FraudCaseEdit
	err := tx.QueryRow(ctx, `
		SELECT case_name, typology_id, typology_raw, sector_id, region_id, region_raw,
		       prosecuting_agency_id, agency_raw, total_amount_nzd, amount_basis, amount_note,
		       offending_period_start, offending_period_end, offending_period_precision,
		       court_name, court_level, court_identifiers, victim_class, victim_detail, victim_count,
		       covid_related, summary
		  FROM fraud_cases WHERE id = $1`, id).Scan(
		&e.CaseName, &e.TypologyID, &e.TypologyRaw, &e.SectorID, &e.RegionID, &e.RegionRaw,
		&e.ProsecutingAgencyID, &e.AgencyRaw, &e.TotalAmountNZD, &e.AmountBasis, &e.AmountNote,
		&e.OffendingPeriodStart, &e.OffendingPeriodEnd, &e.OffendingPeriodPrecision,
		&e.CourtName, &e.CourtLevel, &e.CourtIdentifiers, &e.VictimClass, &e.VictimDetail, &e.VictimCount,
		&e.CovidRelated, &e.Summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrRowMissing
	}
	if err != nil {
		return e, fmt.Errorf("snapshot fraud case: %w", err)
	}
	secs, err := listSecondaryTypologyIDsTx(ctx, tx, id)
	if err != nil {
		return e, err
	}
	e.SecondaryTypologyIDs = secs
	return e, nil
}

// ListFraudCaseSecondaryTypologyIDs returns the ids of a case's secondary
// typologies (the fraud_case_typologies m2m) — the checked state for the
// edit form's multi-select.
func (d *Store) ListFraudCaseSecondaryTypologyIDs(ctx context.Context, caseID int64) ([]int64, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT typology_id FROM fraud_case_typologies WHERE case_id = $1 ORDER BY typology_id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list secondary typology ids: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan secondary typology id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func listSecondaryTypologyIDsTx(ctx context.Context, tx pgx.Tx, caseID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT typology_id FROM fraud_case_typologies WHERE case_id = $1 ORDER BY typology_id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list secondary typology ids: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan secondary typology id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpdateFraudCaseAdmin applies a full-field admin edit to a case and writes a
// matching entity_revisions row in the same transaction. Status is untouched
// (derived or overridden separately). The prosecuting-agency basis is set to
// 'stated' whenever a human assigns an agency (a curator's assignment is
// authoritative, not the recode's 'inferred' guess) and cleared when the
// agency is removed.
func (d *Store) UpdateFraudCaseAdmin(ctx context.Context, id int64, in FraudCaseEdit, editedBy string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	prior, err := snapshotFraudCaseEditTx(ctx, tx, id)
	if err != nil {
		return err
	}

	agencyBasis := ""
	if in.ProsecutingAgencyID != nil {
		agencyBasis = "stated"
	}
	identifiers := in.CourtIdentifiers
	if identifiers == nil {
		identifiers = []string{}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE fraud_cases SET
			case_name                  = $2,
			typology_id                = $3,
			typology_raw               = $4,
			sector_id                  = $5,
			region_id                  = $6,
			region_raw                 = $7,
			prosecuting_agency_id      = $8,
			agency_raw                 = $9,
			agency_basis               = $10,
			total_amount_nzd           = $11,
			amount_basis               = $12,
			amount_note                = $13,
			offending_period_start     = $14,
			offending_period_end       = $15,
			offending_period_precision = $16,
			court_name                 = $17,
			court_level                = $18,
			court_identifiers          = $19,
			victim_class               = $20,
			victim_detail              = $21,
			victim_count               = $22,
			covid_related              = $23,
			summary                    = $24,
			updated_at                 = now()
		 WHERE id = $1`,
		id, in.CaseName, in.TypologyID, in.TypologyRaw, in.SectorID, in.RegionID, in.RegionRaw,
		in.ProsecutingAgencyID, in.AgencyRaw, agencyBasis, in.TotalAmountNZD, in.AmountBasis, in.AmountNote,
		in.OffendingPeriodStart, in.OffendingPeriodEnd, in.OffendingPeriodPrecision,
		in.CourtName, in.CourtLevel, identifiers, in.VictimClass, in.VictimDetail, in.VictimCount,
		in.CovidRelated, in.Summary); err != nil {
		return fmt.Errorf("update fraud case: %w", err)
	}

	// Replace the secondary-typology set. Cheap and order-independent for a
	// handful of rows; keeps the m2m in lockstep with the form.
	if _, err := tx.Exec(ctx, `DELETE FROM fraud_case_typologies WHERE case_id = $1`, id); err != nil {
		return fmt.Errorf("clear secondary typologies: %w", err)
	}
	for _, tid := range in.SecondaryTypologyIDs {
		if tid == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fraud_case_typologies (case_id, typology_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, tid); err != nil {
			return fmt.Errorf("link secondary typology: %w", err)
		}
	}

	beforeJSON, _ := json.Marshal(prior)
	afterJSON, _ := json.Marshal(in)
	if err := d.writeCaseRevisionTx(ctx, tx, CaseRevision{
		CaseID:     id,
		BeforeJSON: beforeJSON,
		AfterJSON:  afterJSON,
		ActorKind:  "admin",
		ChangeKind: "update",
		Note:       "case fields edited by " + editedBy,
	}); err != nil {
		return fmt.Errorf("revision: %w", err)
	}
	return tx.Commit(ctx)
}

// FraudParticipantEdit is the editable surface of one participant row.
type FraudParticipantEdit struct {
	Role                string // defendant | co_accused | named_associate
	ProfessionRaw       string
	Gender              string
	IndividualCharges   string
	IndividualAmountNZD *float64
	Outcome             *string // convicted | acquitted | charges_withdrawn | conviction_overturned | nil
	SentenceType        *string // enum or nil
	SentenceDetail      string
	SentenceMonths      *int
	SuppressionFlag     bool
	CleanSlatePossible  bool
}

// UpdateFraudParticipantAdmin edits an existing participant and logs the
// change under the parent CASE's revision stream (field_name
// "participant_<id>"), so it shows on the case History tab. participantID
// must belong to caseID or ErrRowMissing is returned.
func (d *Store) UpdateFraudParticipantAdmin(ctx context.Context, caseID, participantID int64, in FraudParticipantEdit, editedBy string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var prior FraudParticipantEdit
	var personName string
	err = tx.QueryRow(ctx, `
		SELECT cp.role, cp.profession_raw, cp.gender, cp.individual_charges,
		       cp.individual_amount_nzd, cp.outcome, cp.sentence_type, cp.sentence_detail,
		       cp.sentence_months, cp.suppression_flag, cp.clean_slate_possible, p.full_name
		  FROM fraud_case_participants cp
		  JOIN fraud_persons p ON p.id = cp.person_id
		 WHERE cp.id = $1 AND cp.case_id = $2`, participantID, caseID).Scan(
		&prior.Role, &prior.ProfessionRaw, &prior.Gender, &prior.IndividualCharges,
		&prior.IndividualAmountNZD, &prior.Outcome, &prior.SentenceType, &prior.SentenceDetail,
		&prior.SentenceMonths, &prior.SuppressionFlag, &prior.CleanSlatePossible, &personName)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRowMissing
	}
	if err != nil {
		return fmt.Errorf("snapshot participant: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE fraud_case_participants SET
			role                  = COALESCE(NULLIF($3,''),'defendant'),
			profession_raw        = $4,
			gender                = $5,
			individual_charges    = $6,
			individual_amount_nzd = $7,
			outcome               = $8,
			sentence_type         = $9,
			sentence_detail       = $10,
			sentence_months       = $11,
			suppression_flag      = $12,
			clean_slate_possible  = $13,
			updated_at            = now()
		 WHERE id = $1 AND case_id = $2`,
		participantID, caseID, in.Role, in.ProfessionRaw, in.Gender, in.IndividualCharges,
		in.IndividualAmountNZD, in.Outcome, in.SentenceType, in.SentenceDetail,
		in.SentenceMonths, in.SuppressionFlag, in.CleanSlatePossible); err != nil {
		return fmt.Errorf("update participant: %w", err)
	}

	beforeJSON, _ := json.Marshal(prior)
	afterJSON, _ := json.Marshal(in)
	if err := d.writeCaseRevisionTx(ctx, tx, CaseRevision{
		CaseID:     caseID,
		FieldName:  fmt.Sprintf("participant_%d", participantID),
		BeforeJSON: beforeJSON,
		AfterJSON:  afterJSON,
		ActorKind:  "admin",
		ChangeKind: "update",
		Note:       fmt.Sprintf("participant %q edited by %s", personName, editedBy),
	}); err != nil {
		return fmt.Errorf("revision: %w", err)
	}
	return tx.Commit(ctx)
}

// SetFraudCaseStatusOverride pins a case's status to a hand-chosen value and
// marks it overridden so derivation won't clobber it. status must be a valid
// fraud_cases.status value (the DB CHECK enforces this). Logs a revision.
func (d *Store) SetFraudCaseStatusOverride(ctx context.Context, id int64, status, editedBy string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var priorStatus string
	var priorOverridden bool
	err = tx.QueryRow(ctx, `SELECT status, status_overridden FROM fraud_cases WHERE id = $1`, id).
		Scan(&priorStatus, &priorOverridden)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRowMissing
	}
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE fraud_cases SET status = $2, status_overridden = TRUE, updated_at = now()
		 WHERE id = $1`, id, status); err != nil {
		return fmt.Errorf("set status override: %w", err)
	}

	before, _ := json.Marshal(map[string]any{"status": priorStatus, "status_overridden": priorOverridden})
	after, _ := json.Marshal(map[string]any{"status": status, "status_overridden": true})
	if err := d.writeCaseRevisionTx(ctx, tx, CaseRevision{
		CaseID:     id,
		FieldName:  "status",
		BeforeJSON: before,
		AfterJSON:  after,
		ActorKind:  "admin",
		ChangeKind: "update",
		Note:       "status manually set by " + editedBy,
	}); err != nil {
		return fmt.Errorf("revision: %w", err)
	}
	return tx.Commit(ctx)
}

// ClearFraudCaseStatusOverride removes the manual override and re-derives the
// status from events in the same transaction. Logs a revision.
func (d *Store) ClearFraudCaseStatusOverride(ctx context.Context, id int64, editedBy string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var priorStatus string
	var priorOverridden bool
	err = tx.QueryRow(ctx, `SELECT status, status_overridden FROM fraud_cases WHERE id = $1`, id).
		Scan(&priorStatus, &priorOverridden)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRowMissing
	}
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}

	// Clear the flag FIRST so recomputeFraudCaseStatusTx (which short-circuits
	// on the flag) actually re-derives within this transaction.
	if _, err := tx.Exec(ctx, `
		UPDATE fraud_cases SET status_overridden = FALSE, updated_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("clear status override: %w", err)
	}
	out, err := recomputeFraudCaseStatusTx(ctx, tx, id)
	if err != nil {
		return err
	}

	before, _ := json.Marshal(map[string]any{"status": priorStatus, "status_overridden": priorOverridden})
	after, _ := json.Marshal(map[string]any{"status": string(out.Status), "status_overridden": false})
	if err := d.writeCaseRevisionTx(ctx, tx, CaseRevision{
		CaseID:     id,
		FieldName:  "status",
		BeforeJSON: before,
		AfterJSON:  after,
		ActorKind:  "admin",
		ChangeKind: "update",
		Note:       "status override cleared (reverted to derived) by " + editedBy,
	}); err != nil {
		return fmt.Errorf("revision: %w", err)
	}
	return tx.Commit(ctx)
}
