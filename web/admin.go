// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/suppression"
)

// Admin surface for the corruption & fraud tracker: the claims review
// queue, case sign-off (verify + naming approval), and the HITL
// appeal-check worklist. (The per-org flag register is host-owned and
// arrives through host.AdminExtension.)
//
// Handlers parse, call the DB layer (which enforces every legal guard),
// and redirect with a flash. Admin access to person-level data (claim
// detail, appeal worklist) goes to the host AccessAuditor — the same Privacy
// Act audit stream as org report views.

// GET /admin/fraud — dashboard. The host's AdminExtension (alitheia: the
// team-flag register) renders its panel below the tracker's own.
func (h *Handlers) adminFraudDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claimCounts, err := h.st.CountFraudClaimsByState(ctx)
	if err != nil {
		h.log.Error("fraud dashboard claims", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	caseCounts, err := h.st.CountFraudCasesByStatus(ctx)
	if err != nil {
		h.log.Error("fraud dashboard cases", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var panel template.HTML
	if h.o.AdminExt != nil {
		if panel, err = h.o.AdminExt.DashboardPanel(ctx, r); err != nil {
			h.log.Error("fraud dashboard host panel", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	coverage, err := h.st.GetFraudCodingCoverage(ctx)
	if err != nil {
		h.log.Error("fraud dashboard coverage", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.page(w, r, "dashboard", struct {
		ClaimCounts map[string]int
		CaseCounts  map[string]int
		Coverage    store.FraudCodingCoverage
		HostPanel   template.HTML
	}{claimCounts, caseCounts, coverage, panel})
}

// adminFraudClaimRow is one queue row with display fields precomputed.
type adminFraudClaimRow struct {
	domain.FraudClaim
	Stage        string
	ExcerptShort string
}

// GET /admin/fraud/claims — review queue.
func (h *Handlers) adminFraudClaims(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "pending"
	}
	f := store.FraudClaimFilter{
		State:         state,
		Suppression:   q.Get("suppression") == "1",
		ManualExtract: q.Get("manual") == "1",
		Ready:         q.Get("ready") == "1",
	}
	if f.Ready {
		state = "ready"
	}
	if f.State == "all" {
		f.State = ""
	}
	claims, err := h.st.ListFraudClaims(r.Context(), f)
	if err != nil {
		h.log.Error("fraud claims list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	counts, err := h.st.CountFraudClaimsByState(r.Context())
	if err != nil {
		h.log.Error("fraud claims counts", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	counts["all"] = total

	rows := make([]adminFraudClaimRow, 0, len(claims))
	for _, c := range claims {
		row := adminFraudClaimRow{FraudClaim: c}
		if len(c.Payload) > 0 {
			var ex fraudagency.Extraction
			if json.Unmarshal(c.Payload, &ex) == nil {
				row.Stage = ex.CaseStage
			}
		}
		if e := strings.TrimSpace(c.Excerpt); e != "" {
			if len(e) > 140 {
				e = e[:140] + "…"
			}
			row.ExcerptShort = e
		}
		rows = append(rows, row)
	}

	h.page(w, r, "claims", struct {
		Claims []adminFraudClaimRow
		Counts map[string]int
		State  string
	}{rows, counts, state})
}

// fraudAutoAttachScore is the candidate score at or above which bulk
// approve in auto-resolve mode attaches a claim to the suggested case
// instead of skipping it. 0.9 admits exactly the two non-colliding
// matcher signals — court-identifier overlap (0.97) and exact
// participant name/alias (0.9); fuzzy names (sim*0.85), org matches
// (≤0.7) and amount echoes (0.55) stay below it and become new cases,
// because a wrong attachment corrupts a case timeline silently while a
// duplicate case is visible and fixable.
const fraudAutoAttachScore = 0.9

// POST /admin/fraud/claims/batch — row and bulk approve/reject from the
// queue. Approve runs the FULL legal sequence per claim through the DB
// guards (attachment decision -> in_review -> verified-with-attestation ->
// promote); the button is the reviewer's attestation. Claims needing
// manual extraction are always skipped (nothing to promote). Claims with
// pending match candidates are skipped UNLESS the reviewer ticks
// auto-resolve, which attaches high-confidence candidates
// (>= fraudAutoAttachScore) and turns the rest into new cases — the
// solo-reviewer throughput valve; every auto decision is audited.
func (h *Handlers) adminFraudClaimsBatch(w http.ResponseWriter, r *http.Request) {
	const base = "/admin/fraud/claims"
	if err := r.ParseForm(); err != nil {
		h.flashRedirect(w, r, "error", "bad form", base)
		return
	}
	target := base
	if back := strings.TrimSpace(r.PostForm.Get("back")); back != "" && strings.HasPrefix(back, base) {
		target = back
	}

	// A row button (row_approve/row_reject) acts on that claim alone and
	// overrides the checkbox selection; otherwise the ticked ids apply.
	action := strings.TrimSpace(r.PostForm.Get("action"))
	var ids []int64
	if v := strings.TrimSpace(r.PostForm.Get("row_approve")); v != "" {
		action = "approve"
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = []int64{id}
		}
	} else if v := strings.TrimSpace(r.PostForm.Get("row_reject")); v != "" {
		action = "reject"
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = []int64{id}
		}
	} else {
		for _, raw := range r.PostForm["id"] {
			if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		h.flashRedirect(w, r, "error", "No claims selected.", target)
		return
	}
	if action != "approve" && action != "reject" {
		h.flashRedirect(w, r, "error", "Unknown action.", target)
		return
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if action == "reject" && note == "" {
		h.flashRedirect(w, r, "error", "Rejection requires a note.", target)
		return
	}
	autoResolve := r.PostForm.Get("auto_resolve") != ""

	rid, label := h.actor(r)
	done, skipped, failed := 0, 0, 0
	var res *fraudagency.Resolvers
	for _, id := range ids {
		var err error
		switch action {
		case "approve":
			if res == nil {
				if res, err = fraudagency.LoadResolvers(r.Context(), h.st); err != nil {
					h.log.Error("fraud batch resolvers", "err", err)
					h.flashRedirect(w, r, "error", "internal error", target)
					return
				}
			}
			var ok bool
			ok, err = h.approveFraudClaim(r.Context(), id, note, rid, label, res, autoResolve)
			if err == nil && !ok {
				skipped++
				continue
			}
		case "reject":
			err = h.rejectFraudClaim(r.Context(), id, note, rid, label)
		}
		if err != nil {
			failed++
			h.log.Warn("fraud batch item", "claim", id, "action", action, "err", err)
			continue
		}
		done++
	}

	msg := fmt.Sprintf("%s %d claim(s)", map[string]string{"approve": "Approved & promoted", "reject": "Rejected"}[action], done)
	if skipped > 0 {
		reason := "pending match candidates, manual extraction or suppression flag — decide on the detail page, or tick auto-resolve"
		if autoResolve {
			reason = "manual extraction or suppression flag — decide on the detail page"
		}
		msg += fmt.Sprintf(", skipped %d (%s)", skipped, reason)
	}
	if failed > 0 {
		msg += fmt.Sprintf(", %d failed (see logs)", failed)
	}
	level := "success"
	if done == 0 {
		level = "error"
	}
	h.flashRedirect(w, r, level, msg+".", target)
}

// approveFraudClaim runs one claim through attachment-decision -> review ->
// verify (attestation) -> promote. Returns ok=false (no error) when the
// claim needs a human decision first. With autoResolve the attachment
// decision is taken here instead of skipping: the top pending candidate
// attaches when its score clears fraudAutoAttachScore, otherwise the claim
// is marked no-match and promotes to a NEW case.
func (h *Handlers) approveFraudClaim(ctx context.Context, id int64, note string, rid *int64, label string, res *fraudagency.Resolvers, autoResolve bool) (bool, error) {
	claim, _, _, err := h.st.GetFraudClaim(ctx, id)
	if err != nil {
		return false, err
	}
	if claim.NeedsManualExtraction || len(claim.Payload) == 0 {
		return false, nil
	}
	// Suppression-flagged claims normally quarantine at ingest, but a claim
	// can carry the flag in the pending queue (failed quarantine transition,
	// post-release). Those are never bulk-approved — detail page only.
	if claim.SuppressionFlag {
		return false, nil
	}
	var ex fraudagency.Extraction
	if json.Unmarshal(claim.Payload, &ex) != nil || !ex.IsFraudCase {
		return false, nil
	}
	switch claim.MatchStatus {
	case domain.MatchCandidatesPending:
		if !autoResolve {
			return false, nil // attachment stays a human decision unless opted in
		}
		cands, err := h.st.ListFraudCaseMatchCandidates(ctx, id)
		if err != nil {
			return false, err
		}
		var top *domain.FraudCaseMatchCandidate
		for i := range cands { // already ordered score DESC
			if cands[i].Decision == domain.DecisionPending {
				top = &cands[i]
				break
			}
		}
		if top != nil && top.MatchScore >= fraudAutoAttachScore {
			if err := h.st.DecideFraudCaseMatch(ctx, id, top.ID, true, rid, label); err != nil {
				return false, err
			}
			if note == "" {
				note = fmt.Sprintf("approved from queue (auto-attached case #%d: %s %.2f)",
					top.CaseID, top.MatchMethod, top.MatchScore)
			}
		} else {
			if err := h.st.SetFraudClaimNoMatch(ctx, id, rid, label); err != nil {
				return false, err
			}
			if note == "" && top != nil {
				note = fmt.Sprintf("approved from queue (new case: top candidate #%d %s %.2f below auto-attach threshold %.2f)",
					top.CaseID, top.MatchMethod, top.MatchScore, fraudAutoAttachScore)
			}
		}
	case domain.MatchUnmatched:
		if err := h.st.SetFraudClaimNoMatch(ctx, id, rid, label); err != nil {
			return false, err
		}
	}
	if claim.ReviewState == domain.ReviewPending {
		if _, err := h.st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: id, To: domain.ReviewInReview, ReviewerID: rid, ReviewerLabel: label,
		}); err != nil {
			return false, err
		}
	}
	if note == "" {
		note = "approved from queue (suppression, Clean Slate and attachment checked)"
	}
	if _, err := h.st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: id, To: domain.ReviewVerified, ReviewerID: rid, ReviewerLabel: label,
		Note: note, Attestation: true,
	}); err != nil {
		return false, err
	}
	fresh, _, _, err := h.st.GetFraudClaim(ctx, id)
	if err != nil {
		return false, err
	}
	if fresh.MatchStatus == domain.MatchConfirmed && fresh.CaseID != nil {
		ev, src := fraudagency.EventFromExtraction(&ex, fresh)
		var promoted domain.FraudCase
		promoted, err = h.st.PromoteFraudClaim(ctx, id, nil, &ev, &src, rid, label)
		if err == nil {
			h.backfillOffending(ctx, promoted.ID, &ex, claim.Excerpt)
		}
	} else {
		in := fraudagency.CaseInputFromExtraction(&ex, fresh, res)
		_, err = h.st.PromoteFraudClaim(ctx, id, &in, nil, nil, rid, label)
	}
	return err == nil, err
}

// backfillOffending enriches an existing case at attach-promote time: fills
// the offending period where the case has none (fill-only; reviewer-entered
// data is never overwritten) and merges any court identifiers the claim
// carries (a case accumulates CRI then CA/SC numbers over its life).
// New-case promotes get both via CaseInputFromExtraction already.
func (h *Handlers) backfillOffending(ctx context.Context, caseID int64, ex *fraudagency.Extraction, excerpt string) {
	if ids := fraudagency.CourtIDsFromExtraction(ex, excerpt); len(ids) > 0 {
		if err := h.st.AddFraudCaseCourtIdentifiers(ctx, caseID, ids); err != nil {
			h.log.Warn("court id backfill", "case", caseID, "err", err)
		}
	}
	start := fraudagency.ParseExtractionDate(ex.OffendingPeriod.Start)
	end := fraudagency.ParseExtractionDate(ex.OffendingPeriod.End)
	if start == nil && end == nil {
		return
	}
	var precision *string
	if start != nil {
		p := fraudagency.DatePrecision(ex.OffendingPeriod.Start)
		precision = &p
	}
	if err := h.st.BackfillFraudCaseOffending(ctx, caseID, start, end, precision); err != nil {
		h.log.Warn("offending backfill", "case", caseID, "err", err)
	}
}

func (h *Handlers) rejectFraudClaim(ctx context.Context, id int64, note string, rid *int64, label string) error {
	claim, _, _, err := h.st.GetFraudClaim(ctx, id)
	if err != nil {
		return err
	}
	// needs_more_info and pending/in_review all have a legal edge to
	// rejected; the state machine validates.
	_ = claim
	_, err = h.st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
		ClaimID: id, To: domain.ReviewRejected, ReviewerID: rid, ReviewerLabel: label, Note: note,
	})
	return err
}

// GET /admin/fraud/claims/{id} — claim detail.
func (h *Handlers) adminFraudClaimDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	claim, cands, reviews, err := h.st.GetFraudClaim(r.Context(), id)
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("fraud claim get", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.auditAdmin(r, "admin_claim_view", fmt.Sprintf("claim %d", id))

	var pretty string
	if len(claim.Payload) > 0 {
		var buf map[string]any
		if json.Unmarshal(claim.Payload, &buf) == nil {
			if b, err := json.MarshalIndent(buf, "", "  "); err == nil {
				pretty = string(b)
			}
		}
	}
	h.page(w, r, "claim_detail", struct {
		Claim         domain.FraudClaim
		Candidates    []domain.FraudCaseMatchCandidate
		Reviews       []domain.FraudClaimReview
		PayloadPretty string
	}{claim, cands, reviews, pretty})
}

// POST /admin/fraud/claims/{id}/transition — any review-state edge.
func (h *Handlers) adminFraudClaimTransition(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rid, label := h.actor(r)
	_, err = h.st.TransitionFraudClaim(r.Context(), fraudtracker.TransitionInput{
		ClaimID:       id,
		To:            domain.ReviewState(r.FormValue("to")),
		ReviewerID:    rid,
		ReviewerLabel: label,
		Note:          strings.TrimSpace(r.FormValue("note")),
		Attestation:   r.FormValue("attestation") == "1",
	})
	h.fraudActionRedirect(w, r, id, err, "state updated")
}

// POST /admin/fraud/claims/{id}/release-vote — quarantine release vote.
func (h *Handlers) adminFraudClaimReleaseVote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rid, label := h.actor(r)
	err = h.st.RecordFraudReview(r.Context(), domain.FraudClaimReview{
		ClaimID: id, Reviewer: rid, ReviewerLabel: label,
		Action: "quarantine_release_vote", ToState: domain.ReviewQuarantined,
		Note: strings.TrimSpace(r.FormValue("note")),
	})
	h.fraudActionRedirect(w, r, id, err, "release vote recorded")
}

// POST /admin/fraud/claims/{id}/match/{cand} — confirm/reject a candidate.
func (h *Handlers) adminFraudClaimMatchDecide(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	cand, err2 := strconv.ParseInt(chi.URLParam(r, "cand"), 10, 64)
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	rid, label := h.actor(r)
	confirm := r.FormValue("decision") == "confirm"
	err := h.st.DecideFraudCaseMatch(r.Context(), id, cand, confirm, rid, label)
	h.fraudActionRedirect(w, r, id, err, "match decided")
}

// POST /admin/fraud/claims/{id}/no-match — explicit "new case" decision.
func (h *Handlers) adminFraudClaimNoMatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rid, label := h.actor(r)
	err = h.st.SetFraudClaimNoMatch(r.Context(), id, rid, label)
	h.fraudActionRedirect(w, r, id, err, "marked as new case")
}

// POST /admin/fraud/claims/{id}/promote — verified claim -> case data.
// With a confirmed attachment the claim becomes an event on that case;
// with no_match its payload becomes a new case.
func (h *Handlers) adminFraudClaimPromote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	claim, _, _, err := h.st.GetFraudClaim(ctx, id)
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("fraud promote get", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var ex fraudagency.Extraction
	if len(claim.Payload) == 0 || json.Unmarshal(claim.Payload, &ex) != nil {
		h.fraudActionRedirect(w, r, id, errors.New("claim has no parseable payload — promote it manually via case edit"), "")
		return
	}

	rid, label := h.actor(r)
	var promoted domain.FraudCase
	if claim.MatchStatus == domain.MatchConfirmed && claim.CaseID != nil {
		ev, src := fraudagency.EventFromExtraction(&ex, claim)
		promoted, err = h.st.PromoteFraudClaim(ctx, id, nil, &ev, &src, rid, label)
		if err == nil {
			h.backfillOffending(ctx, promoted.ID, &ex, claim.Excerpt)
		}
	} else {
		res, rerr := fraudagency.LoadResolvers(ctx, h.st)
		if rerr != nil {
			h.log.Error("fraud promote resolvers", "err", rerr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		in := fraudagency.CaseInputFromExtraction(&ex, claim, res)
		promoted, err = h.st.PromoteFraudClaim(ctx, id, &in, nil, nil, rid, label)
	}
	h.fraudActionRedirect(w, r, id, err, fmt.Sprintf("promoted to case #%d", promoted.ID))
}

// GET /admin/fraud/cases — case register for the sign-off pass.
func (h *Handlers) adminFraudCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.FraudCaseFilter{
		Status:      q.Get("status"),
		ReviewState: q.Get("review"),
	}
	if off, err := strconv.Atoi(q.Get("offset")); err == nil {
		f.Offset = off
	}
	cases, err := h.st.ListFraudCases(r.Context(), f)
	if err != nil {
		h.log.Error("fraud cases list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.auditAdmin(r, "admin_cases_view", fmt.Sprintf("list offset=%d", f.Offset))
	h.page(w, r, "cases", struct {
		Cases  []domain.FraudCase
		Filter store.FraudCaseFilter
		Next   int
	}{cases, f, f.Offset + len(cases)})
}

// GET /admin/fraud/cases/{id} — the full, unredacted case detail: internal
// label, participants with outcomes and flags, the complete event timeline,
// sources (excerpts included — admin-only, never republished), linked
// claims, and the verify/naming actions. Person-level access is logged.
func (h *Handlers) adminFraudCaseDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	c, parts, events, sources, err := h.st.GetFraudCase(ctx, id)
	if errors.Is(err, store.ErrRowMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("admin fraud case detail", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	labels, err := h.st.GetFraudCaseLabels(ctx, id)
	if err != nil {
		h.log.Error("admin fraud case labels", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	orgs, err := h.st.ListFraudCaseOrganisations(ctx, id)
	if err != nil {
		h.log.Error("admin fraud case orgs", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	claims, err := h.st.ListFraudClaimsByCase(ctx, id)
	if err != nil {
		h.log.Error("admin fraud case claims", "case", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.auditAdmin(r, "admin_case_view", fmt.Sprintf("case %d", id))

	srcByID := map[int64]domain.FraudCaseSource{}
	for _, src := range sources {
		srcByID[src.ID] = src
	}
	type eventRow struct {
		domain.FraudCaseEvent
		SourceURL string
		Publisher string
	}
	rows := make([]eventRow, 0, len(events))
	for _, e := range events {
		row := eventRow{FraudCaseEvent: e}
		if e.SourceID != nil {
			if src, ok := srcByID[*e.SourceID]; ok {
				row.SourceURL, row.Publisher = src.URL, src.Publisher
			}
		}
		rows = append(rows, row)
	}

	h.page(w, r, "case_detail", struct {
		Case         domain.FraudCase
		Labels       store.FraudCaseLabels
		Participants []domain.FraudCaseParticipant
		Orgs         []store.FraudCaseOrg
		Events       []eventRow
		Sources      []domain.FraudCaseSource
		Claims       []domain.FraudClaim
	}{c, labels, parts, orgs, rows, sources, claims})
}

// POST /admin/fraud/cases/{id}/verify — reviewer sign-off.
func (h *Handlers) adminFraudCaseVerify(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rid, email := h.actor(r)
	if rid == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_, err = h.st.VerifyFraudCase(r.Context(), id, *rid)
	if err == nil {
		after, _ := json.Marshal(map[string]any{"review_state": "verified"})
		if rerr := h.st.RecordCaseRevision(r.Context(), store.CaseRevision{
			CaseID: id, FieldName: "review_state",
			AfterJSON: after, ActorKind: "admin", ActorID: rid, ChangeKind: "update",
			Note: "verified (signed off) by " + email,
		}); rerr != nil {
			h.log.Warn("fraud verify revision", "case", id, "err", rerr)
		}
	}
	h.fraudCasesRedirect(w, r, err, fmt.Sprintf("case #%d verified", id))
}

// POST /admin/fraud/cases/{id}/naming — explicit naming approval toggle.
// The reviewer attests no suppression order applies before approving.
func (h *Handlers) adminFraudCaseNaming(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	approve := r.FormValue("approved") == "1"
	if approve && r.FormValue("attestation") != "1" {
		h.fraudCasesRedirect(w, r, errors.New("naming approval requires the suppression-check attestation"), "")
		return
	}
	err = h.st.SetFraudCaseNamingApproved(r.Context(), id, approve)
	if err == nil {
		rid, email := h.actor(r)
		after, _ := json.Marshal(map[string]any{"naming_approved": approve})
		if rerr := h.st.RecordCaseRevision(r.Context(), store.CaseRevision{
			CaseID: id, FieldName: "naming_approved",
			AfterJSON: after, ActorKind: "admin", ActorID: rid, ChangeKind: "update",
			Note: fmt.Sprintf("naming set to %v by %s", approve, email),
		}); rerr != nil {
			h.log.Warn("fraud naming revision", "case", id, "err", rerr)
		}
	}
	h.fraudCasesRedirect(w, r, err, fmt.Sprintf("case #%d naming set to %v", id, approve))
}

// GET /admin/fraud/appeals — the HITL appeal-check worklist. Prefilled
// JDO/NZLII search links open in the reviewer's BROWSER; this server never
// fetches those sites (their terms prohibit automated access).
// fraudAppealCoolDays is how long a "no appeal found" check keeps a case
// off the worklist before it resurfaces for a re-check.
const fraudAppealCoolDays = 60

func (h *Handlers) adminFraudAppeals(w http.ResponseWriter, r *http.Request) {
	const appealWindowDays = 365
	checks, err := h.st.FraudAppealWorklist(r.Context(), appealWindowDays, fraudAppealCoolDays)
	if err != nil {
		h.log.Error("fraud appeals worklist", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.auditAdmin(r, "admin_appeal_worklist", fmt.Sprintf("%d cases", len(checks)))

	type row struct {
		store.FraudAppealCheck
		JDOLink   string
		NZLIILink string
	}
	rows := make([]row, 0, len(checks))
	for _, c := range checks {
		query := c.Typology
		if len(c.ParticipantNames) > 0 {
			query = c.ParticipantNames[0]
		}
		rows = append(rows, row{
			FraudAppealCheck: c,
			// Senior courts (HC/CoA/SC) judgment search — appeals of the
			// tracker's convictions surface here. No query-param prefill
			// supported; the NZLII link carries the prefilled search.
			JDOLink:   "https://www.courtsofnz.govt.nz/judgments/",
			NZLIILink: "https://www.nzlii.org/cgi-bin/sinosrch.cgi?method=boolean&query=" + url.QueryEscape(query),
		})
	}
	h.page(w, r, "appeals", struct {
		Rows       []row
		WindowDays int
		CoolDays   int
	}{rows, appealWindowDays, fraudAppealCoolDays})
}

// redirect helpers -----------------------------------------------------------

func (h *Handlers) fraudActionRedirect(w http.ResponseWriter, r *http.Request, claimID int64, err error, okMsg string) {
	if err != nil {
		h.o.Layout.SetFlash(w, "error", err.Error())
	} else if okMsg != "" {
		h.o.Layout.SetFlash(w, "success", okMsg)
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/fraud/claims/%d", claimID), http.StatusSeeOther)
}

func (h *Handlers) fraudCasesRedirect(w http.ResponseWriter, r *http.Request, err error, okMsg string) {
	if err != nil {
		h.o.Layout.SetFlash(w, "error", err.Error())
	} else if okMsg != "" {
		h.o.Layout.SetFlash(w, "success", okMsg)
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		ref = "/admin/fraud/cases"
	}
	http.Redirect(w, r, ref, http.StatusSeeOther)
}

// ───────────────────── manual submission + appeal checks ─────────────────────

// GET /admin/fraud/submit — the manual-submission form: the entry point for
// anything the connectors can't fetch (JDO/NZLII findings from the appeal
// worklist, paywalled reporting, tips). ?case=N pre-attaches the claim to a
// case; ?origin=appeals also logs the appeal check.
func (h *Handlers) adminFraudSubmitForm(w http.ResponseWriter, r *http.Request) {
	var caseRef *domain.FraudCase
	if v := r.URL.Query().Get("case"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if c, _, _, _, err := h.st.GetFraudCase(r.Context(), id); err == nil {
				caseRef = &c
			}
		}
	}
	h.page(w, r, "submit", struct {
		Case   *domain.FraudCase
		Origin string
	}{caseRef, r.URL.Query().Get("origin")})
}

// POST /admin/fraud/submit — creates a claim from hand-entered fields. No
// LLM: the reviewer IS the extractor here, so the structured payload is
// synthesised from the form and flows through the normal review queue
// (verify -> promote) like any connector claim. Nothing skips review.
func (h *Handlers) adminFraudSubmitPost(w http.ResponseWriter, r *http.Request) {
	const base = "/admin/fraud/submit"
	if err := r.ParseForm(); err != nil {
		h.flashRedirect(w, r, "error", "bad form", base)
		return
	}
	ctx := r.Context()
	get := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }

	headline := get("headline")
	text := get("text")
	if headline == "" || text == "" {
		h.flashRedirect(w, r, "error", "Headline and supporting text are both required — the text is the evidence the reviewer verifies against.", base)
		return
	}
	stage := get("stage")
	srcType := domain.FraudSourceType(get("source_type"))
	switch srcType {
	case domain.FraudSourceManual, domain.FraudSourceJudgment, domain.FraudSourceNews, domain.FraudSourceAgencyRelease:
	default:
		srcType = domain.FraudSourceManual
	}

	// Synthesised extraction: the same shape connector claims carry, so the
	// promote path maps it identically.
	ex := fraudagency.Extraction{
		IsFraudCase:  true,
		CaseStage:    stage,
		CaseNameHint: get("outcome_detail"),
		Court:        get("court"),
		EventDate:    get("event_date"),
		ExactExcerpt: text,
		Confidence:   1, // human-entered
	}
	if cit := get("citation"); cit != "" {
		ex.CourtIdentifiers = append(ex.CourtIdentifiers, cit)
	}
	suppressed := r.PostForm.Get("suppression") == "1"
	if hit, _ := suppression.Detect(text); hit {
		suppressed = true
	}
	ex.SuppressionIndicators.NameSuppressionMentioned = suppressed
	payload, err := json.Marshal(ex)
	if err != nil {
		h.flashRedirect(w, r, "error", "internal error", base)
		return
	}

	sum := sha256.Sum256(append([]byte(headline), []byte(text)...))
	hash := "manualsubmit:" + hex.EncodeToString(sum[:])
	key := hash
	if u := get("url"); u != "" {
		key = "url:" + u // future connector fetches of the same item dedupe against this
	}
	conf := 1.0
	claim := domain.FraudClaim{
		Payload:         payload,
		Headline:        headline,
		ConfidenceScore: &conf,
		SourceType:      srcType,
		SourceURL:       get("url"),
		SourcePublisher: get("publisher"),
		LicenceBasis:    "manual-submission (reviewer-entered)",
		Excerpt:         text,
		ContentHash:     hash,
		DedupeKey:       &key,
		SuppressionFlag: suppressed,
	}

	rid, label := h.actor(r)
	var caseID int64
	if v := get("case_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			// Pre-attachment here is a HUMAN decision (the submitter chose
			// the case), so it doesn't violate the never-auto-confirm rule.
			caseID = id
			claim.CaseID = &caseID
			claim.MatchStatus = domain.MatchConfirmed
		}
	}

	created, wasNew, err := h.st.CreateFraudClaim(ctx, claim, rid, label)
	if err != nil {
		h.flashRedirect(w, r, "error", err.Error(), base)
		return
	}
	if !wasNew {
		h.flashRedirect(w, r, "info", fmt.Sprintf("Already submitted — see claim #%d.", created.ID),
			fmt.Sprintf("/admin/fraud/claims/%d", created.ID))
		return
	}
	if suppressed {
		if _, err := h.st.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: created.ID, To: domain.ReviewQuarantined,
			ReviewerID: rid, ReviewerLabel: label,
			Note: "auto-quarantined: suppression indicated on manual submission",
		}); err != nil {
			h.log.Warn("manual submit quarantine", "claim", created.ID, "err", err)
		}
	}
	if caseID == 0 {
		// No case chosen: run the usual suggestions so attachment is a click.
		if _, err := h.st.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
			ClaimID: created.ID, CourtIDs: ex.CourtIdentifiers,
		}); err != nil {
			h.log.Warn("manual submit suggestions", "claim", created.ID, "err", err)
		}
	} else if r.PostForm.Get("origin") == "appeals" {
		// Came from the appeal worklist: log the check so the diligence
		// trail shows the finding and the row leaves the worklist.
		if err := h.st.RecordFraudAppealCheck(ctx, caseID, rid, label, "finding_recorded",
			"manual submission: "+headline); err != nil {
			h.log.Warn("appeal check record", "err", err)
		}
	}
	h.flashRedirect(w, r, "success",
		fmt.Sprintf("Submitted as claim #%d — it now goes through the normal review queue.", created.ID),
		fmt.Sprintf("/admin/fraud/claims/%d", created.ID))
}

// POST /admin/fraud/appeals/checked — "I ran the searches, no appeal found."
// Logs the check and starts the cooling window that removes the row.
func (h *Handlers) adminFraudAppealChecked(w http.ResponseWriter, r *http.Request) {
	const back = "/admin/fraud/appeals"
	if err := r.ParseForm(); err != nil {
		h.flashRedirect(w, r, "error", "bad form", back)
		return
	}
	caseID, err := strconv.ParseInt(r.PostForm.Get("case_id"), 10, 64)
	if err != nil || caseID <= 0 {
		h.flashRedirect(w, r, "error", "bad case id", back)
		return
	}
	rid, label := h.actor(r)
	if err := h.st.RecordFraudAppealCheck(r.Context(), caseID, rid, label,
		"no_appeal_found", strings.TrimSpace(r.PostForm.Get("note"))); err != nil {
		h.flashRedirect(w, r, "error", err.Error(), back)
		return
	}
	h.flashRedirect(w, r, "success",
		fmt.Sprintf("Check recorded for case #%d — it returns to the worklist in %d days (or sooner if its status changes).", caseID, fraudAppealCoolDays), back)
}
