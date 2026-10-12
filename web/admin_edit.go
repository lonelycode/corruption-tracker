// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
)

// Editable fraud-case detail: a full-field edit form, an existing-participant
// edit, the manual status override, and the append-only History tab. All
// mutations route through the db layer, which writes an entity_revisions row
// (EntityKindFraudCase) in the same transaction. Gated by
// requireFraudTrackerAdmin (see router.go), so both full admins and the
// scoped fraud_admin role reach them.

// fraudEditStatuses is the closed set a manual status override may take —
// mirrors the fraud_cases_status_chk DB constraint. Validated in the handler
// so a bad value flashes a clean error instead of a raw constraint failure.
var fraudEditStatuses = []string{
	"charges_laid", "before_court", "convicted", "acquitted",
	"appeal_pending", "overturned", "upheld", "discontinued",
}

var (
	fraudEditAmountBases   = []string{"alleged", "proven", "reparations_ordered"}
	fraudEditVictimClasses = []string{
		"taxpayer_central_govt", "local_govt", "employer", "investors",
		"consumers", "individuals", "charity", "iwi", "mixed",
	}
	fraudEditPrecisions    = []string{"day", "month", "year"}
	fraudEditRoles         = []string{"defendant", "co_accused", "named_associate"}
	fraudEditOutcomes      = []string{"convicted", "acquitted", "charges_withdrawn", "conviction_overturned"}
	fraudEditSentenceTypes = []string{
		"imprisonment", "home_detention", "community_detention", "community_work",
		"fine", "reparation_only", "discharge", "deportation_followed",
	}
)

// GET /admin/fraud/cases/{id}/edit — the editable form.
func (h *Handlers) adminFraudCaseEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Param(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	c, parts, _, _, err := h.st.GetFraudCase(ctx, id)
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("fraud case edit load", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	typologies, err1 := h.st.LoadFraudTaxonomy(ctx, "typology")
	agencies, err2 := h.st.LoadFraudTaxonomy(ctx, "agency")
	regions, err3 := h.st.LoadFraudTaxonomy(ctx, "region")
	sectors, err4 := h.st.LoadFraudTaxonomy(ctx, "sector")
	if err := firstErr(err1, err2, err3, err4); err != nil {
		h.log.Error("fraud case edit taxonomies", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	secondary, err := h.st.ListFraudCaseSecondaryTypologyIDs(ctx, id)
	if err != nil {
		h.log.Error("fraud case edit secondary typologies", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	secSet := map[int64]bool{}
	for _, tid := range secondary {
		secSet[tid] = true
	}
	h.auditAdmin(r, "admin_case_edit", fmt.Sprintf("case %d", id))

	h.page(w, r, "case_edit", struct {
		Case          domain.FraudCase
		Participants  []domain.FraudCaseParticipant
		Typologies    []domain.FraudTaxonomyTerm
		Agencies      []domain.FraudTaxonomyTerm
		Regions       []domain.FraudTaxonomyTerm
		Sectors       []domain.FraudTaxonomyTerm
		SecondarySet  map[int64]bool
		Statuses      []string
		AmountBases   []string
		VictimClasses []string
		Precisions    []string
		Roles         []string
		Outcomes      []string
		SentenceTypes []string
	}{c, parts, typologies, agencies, regions, sectors, secSet,
		fraudEditStatuses, fraudEditAmountBases, fraudEditVictimClasses, fraudEditPrecisions,
		fraudEditRoles, fraudEditOutcomes, fraudEditSentenceTypes})
}

// POST /admin/fraud/cases/{id}/edit — apply a full-field edit.
func (h *Handlers) adminFraudCaseUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Param(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	editURL := fmt.Sprintf("/admin/fraud/cases/%d/edit", id)

	start, err := formOptionalDate(r.PostForm.Get("offending_period_start"))
	if err != nil {
		h.fraudEditRedirect(w, r, editURL, errors.New("offending start date: "+err.Error()), "")
		return
	}
	end, err := formOptionalDate(r.PostForm.Get("offending_period_end"))
	if err != nil {
		h.fraudEditRedirect(w, r, editURL, errors.New("offending end date: "+err.Error()), "")
		return
	}

	in := store.FraudCaseEdit{
		CaseName:                 strings.TrimSpace(r.PostForm.Get("case_name")),
		TypologyID:               formOptionalInt64(r.PostForm.Get("typology_id")),
		TypologyRaw:              strings.TrimSpace(r.PostForm.Get("typology_raw")),
		SecondaryTypologyIDs:     formInt64List(r.PostForm["secondary_typology"]),
		SectorID:                 formOptionalInt64(r.PostForm.Get("sector_id")),
		RegionID:                 formOptionalInt64(r.PostForm.Get("region_id")),
		RegionRaw:                strings.TrimSpace(r.PostForm.Get("region_raw")),
		ProsecutingAgencyID:      formOptionalInt64(r.PostForm.Get("prosecuting_agency_id")),
		AgencyRaw:                strings.TrimSpace(r.PostForm.Get("agency_raw")),
		TotalAmountNZD:           formOptionalFloat(r.PostForm.Get("total_amount_nzd")),
		AmountBasis:              formOptionalEnum(r.PostForm.Get("amount_basis"), fraudEditAmountBases),
		AmountNote:               strings.TrimSpace(r.PostForm.Get("amount_note")),
		OffendingPeriodStart:     start,
		OffendingPeriodEnd:       end,
		OffendingPeriodPrecision: formOptionalEnum(r.PostForm.Get("offending_period_precision"), fraudEditPrecisions),
		CourtName:                strings.TrimSpace(r.PostForm.Get("court_name")),
		CourtLevel:               strings.TrimSpace(r.PostForm.Get("court_level")),
		CourtIdentifiers:         parseIdentifiers(r.PostForm.Get("court_identifiers")),
		VictimClass:              formOptionalEnum(r.PostForm.Get("victim_class"), fraudEditVictimClasses),
		VictimDetail:             strings.TrimSpace(r.PostForm.Get("victim_detail")),
		VictimCount:              formOptionalInt(r.PostForm.Get("victim_count")),
		CovidRelated:             r.PostForm.Get("covid_related") == "1",
		Summary:                  strings.TrimSpace(r.PostForm.Get("summary")),
	}

	err = h.st.UpdateFraudCaseAdmin(r.Context(), id, in, h.label(r))
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fraudEditRedirect(w, r, editURL, err, "")
		return
	}
	h.fraudEditRedirect(w, r, fmt.Sprintf("/admin/fraud/cases/%d", id), nil, fmt.Sprintf("case #%d updated", id))
}

// POST /admin/fraud/cases/{id}/participants/{pid} — edit one participant.
func (h *Handlers) adminFraudParticipantUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Param(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	pid, ok := parseInt64Param(r, "pid")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	in := store.FraudParticipantEdit{
		Role:                r.PostForm.Get("role"),
		ProfessionRaw:       strings.TrimSpace(r.PostForm.Get("profession_raw")),
		Gender:              strings.TrimSpace(r.PostForm.Get("gender")),
		IndividualCharges:   strings.TrimSpace(r.PostForm.Get("individual_charges")),
		IndividualAmountNZD: formOptionalFloat(r.PostForm.Get("individual_amount_nzd")),
		Outcome:             formOptionalEnum(r.PostForm.Get("outcome"), fraudEditOutcomes),
		SentenceType:        formOptionalEnum(r.PostForm.Get("sentence_type"), fraudEditSentenceTypes),
		SentenceDetail:      strings.TrimSpace(r.PostForm.Get("sentence_detail")),
		SentenceMonths:      formOptionalInt(r.PostForm.Get("sentence_months")),
		SuppressionFlag:     r.PostForm.Get("suppression_flag") == "1",
		CleanSlatePossible:  r.PostForm.Get("clean_slate_possible") == "1",
	}
	editURL := fmt.Sprintf("/admin/fraud/cases/%d/edit", id)
	err := h.st.UpdateFraudParticipantAdmin(r.Context(), id, pid, in, h.label(r))
	if errors.Is(err, store.ErrRowMissing) {
		h.fraudEditRedirect(w, r, editURL, errors.New("participant not found on this case"), "")
		return
	}
	if err != nil {
		h.fraudEditRedirect(w, r, editURL, err, "")
		return
	}
	h.fraudEditRedirect(w, r, editURL, nil, "participant updated")
}

// POST /admin/fraud/cases/{id}/status — manual status override.
func (h *Handlers) adminFraudCaseStatusUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Param(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	status := r.FormValue("status")
	if !inSet(status, fraudEditStatuses) {
		h.fraudEditRedirect(w, r, fmt.Sprintf("/admin/fraud/cases/%d/edit", id), errors.New("invalid status value"), "")
		return
	}
	err := h.st.SetFraudCaseStatusOverride(r.Context(), id, status, h.label(r))
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	h.fraudEditRedirect(w, r, fmt.Sprintf("/admin/fraud/cases/%d", id), err,
		fmt.Sprintf("case #%d status manually set to %s", id, status))
}

// POST /admin/fraud/cases/{id}/status/revert — drop the override, re-derive.
func (h *Handlers) adminFraudCaseStatusRevert(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Param(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := h.st.ClearFraudCaseStatusOverride(r.Context(), id, h.label(r))
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	h.fraudEditRedirect(w, r, fmt.Sprintf("/admin/fraud/cases/%d", id), err,
		fmt.Sprintf("case #%d status reverted to auto-derived", id))
}

// GET /admin/fraud/cases/{id}/history — the append-only revision trail.
func (h *Handlers) adminFraudCaseHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Param(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	c, _, _, _, err := h.st.GetFraudCase(ctx, id)
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("fraud case history load", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	revisions, err := h.st.ListCaseRevisions(ctx, id, 0)
	if err != nil {
		h.log.Error("fraud case revisions", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.page(w, r, "case_history", struct {
		Case      domain.FraudCase
		Revisions []store.CaseRevisionRow
	}{c, revisions})
}

// fraudEditRedirect flashes and redirects to an explicit URL (unlike
// fraudCasesRedirect which bounces to Referer). On error it goes back to the
// supplied URL (typically the edit form); on success to dest.
func (h *Handlers) fraudEditRedirect(w http.ResponseWriter, r *http.Request, dest string, err error, okMsg string) {
	if err != nil {
		h.o.Layout.SetFlash(w, "error", err.Error())
	} else if okMsg != "" {
		h.o.Layout.SetFlash(w, "success", okMsg)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ---- form parsing helpers ----------------------------------------------

func formOptionalInt64(v string) *int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

func formOptionalInt(v string) *int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func formOptionalFloat(v string) *float64 {
	v = strings.TrimSpace(strings.ReplaceAll(v, ",", ""))
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

// formOptionalEnum returns a *string only when v is a non-empty member of
// allowed; anything else (empty, or an unexpected value) maps to nil so the
// nullable column is cleared rather than rejected by its CHECK.
func formOptionalEnum(v string, allowed []string) *string {
	v = strings.TrimSpace(v)
	if v == "" || !inSet(v, allowed) {
		return nil
	}
	return &v
}

func formOptionalDate(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return nil, fmt.Errorf("expected YYYY-MM-DD")
	}
	return &t, nil
}

func formInt64List(vals []string) []int64 {
	out := make([]int64, 0, len(vals))
	for _, v := range vals {
		if p := formOptionalInt64(v); p != nil {
			out = append(out, *p)
		}
	}
	return out
}

// parseIdentifiers splits a comma/newline-separated court-identifier field
// into a trimmed, de-duplicated, order-preserving slice.
func parseIdentifiers(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func inSet(v string, allowed []string) bool {
	for _, a := range allowed {
		if a == v {
			return true
		}
	}
	return false
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
