// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/lonelycode/corruption-tracker/store"
)

// Duplicate-case review + merge for the corruption & fraud tracker. The same
// matter ingested from multiple outlets or agency feeds produces separate
// fraud_cases rows that surface as repeats in the explorer/CSV. This admin
// surface lists the high-precision duplicate groups (the same detection the
// `ingester fraud merge-dupes` pass uses) and lets a reviewer fold a group —
// or all of them — down to one winner. Same db layer, so the ingester and the
// API stay in lockstep.

// GET /admin/fraud/duplicates — the review worklist.
func (h *Handlers) adminFraudDuplicates(w http.ResponseWriter, r *http.Request) {
	groups, err := h.st.FindDuplicateFraudCaseGroups(r.Context())
	if err != nil {
		h.log.Error("fraud duplicates", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.auditAdmin(r, "admin_duplicate_worklist", fmt.Sprintf("%d groups", len(groups)))

	// Pre-format the loser id list so the merge form can post it back verbatim.
	type groupView struct {
		store.FraudDuplicateGroup
		GroupCSV string // every case id in the group, for the merge form
	}
	views := make([]groupView, 0, len(groups))
	for _, g := range groups {
		ids := make([]int64, len(g.Cases))
		for i, c := range g.Cases {
			ids[i] = c.ID
		}
		views = append(views, groupView{FraudDuplicateGroup: g, GroupCSV: joinInts(ids)})
	}
	h.page(w, r, "duplicates", struct{ Groups []groupView }{views})
}

// POST /admin/fraud/duplicates/merge — merge one group (winner + losers) or,
// with action=merge-all, every detected group in one pass.
func (h *Handlers) adminFraudDuplicatesMerge(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.o.Auth.Principal(r); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if r.FormValue("action") == "merge-all" {
		groups, err := h.st.FindDuplicateFraudCaseGroups(r.Context())
		if err != nil {
			h.fraudDuplicatesRedirect(w, r, err, "")
			return
		}
		merged, failed := 0, 0
		for _, g := range groups {
			res, err := h.st.MergeFraudCases(r.Context(), g.WinnerID, g.LoserIDs)
			if err != nil {
				h.log.Error("merge-all group", "winner", g.WinnerID, "err", err)
				failed++
				continue
			}
			merged += res.LosersMerged
		}
		h.auditAdmin(r, "admin_duplicate_merge_all",
			fmt.Sprintf("%d groups, %d cases merged, %d failed", len(groups), merged, failed))
		msg := fmt.Sprintf("Merged %d duplicate case(s) across %d group(s)", merged, len(groups))
		if failed > 0 {
			msg += fmt.Sprintf(" (%d group(s) failed — see logs)", failed)
		}
		h.fraudDuplicatesRedirect(w, r, nil, msg)
		return
	}

	winner, err := strconv.ParseInt(r.FormValue("winner"), 10, 64)
	if err != nil {
		h.fraudDuplicatesRedirect(w, r, fmt.Errorf("winner case id required"), "")
		return
	}
	// The form posts every id in the group plus the chosen winner; the losers
	// are the rest, so the reviewer can promote any member to winner.
	group, err := parseInts(r.FormValue("group"))
	if err != nil || len(group) < 2 {
		h.fraudDuplicatesRedirect(w, r, fmt.Errorf("group must list at least two case ids"), "")
		return
	}
	var losers []int64
	for _, id := range group {
		if id != winner {
			losers = append(losers, id)
		}
	}
	if len(losers) == 0 {
		h.fraudDuplicatesRedirect(w, r, fmt.Errorf("winner must be one of the group's cases"), "")
		return
	}
	res, err := h.st.MergeFraudCases(r.Context(), winner, losers)
	if err != nil {
		h.fraudDuplicatesRedirect(w, r, err, "")
		return
	}
	h.auditAdmin(r, "admin_duplicate_merge",
		fmt.Sprintf("winner #%d, merged %d, discarded %d collisions", winner, res.LosersMerged, res.CollisionsDiscarded))
	h.fraudDuplicatesRedirect(w, r, nil,
		fmt.Sprintf("Merged %d case(s) into #%d", res.LosersMerged, winner))
}

func (h *Handlers) fraudDuplicatesRedirect(w http.ResponseWriter, r *http.Request, err error, okMsg string) {
	if err != nil {
		h.o.Layout.SetFlash(w, "error", err.Error())
	} else if okMsg != "" {
		h.o.Layout.SetFlash(w, "success", okMsg)
	}
	http.Redirect(w, r, "/admin/fraud/duplicates", http.StatusSeeOther)
}

func joinInts(ns []int64) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(parts, ",")
}

func parseInts(csv string) ([]int64, error) {
	var out []int64
	for p := range strings.SplitSeq(csv, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
