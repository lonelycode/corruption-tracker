// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
)

// PeriodRef is one published issue in the picker.
type PeriodRef struct {
	Period  string // YYYY | YYYY-Qn | YYYY-Www
	Version int
	Revised bool
}

// Kind classifies the period: "year" | "quarter" | "week".
func (p PeriodRef) Kind() string {
	return store.FraudReportPeriod{Period: p.Period}.Kind()
}

// Snapshot is one issue as the tier may see it.
type Snapshot struct {
	Period       string
	Version      int
	RevisionNote string
	// DevBuild marks a snapshot built with unverified cases (operators only).
	DevBuild bool
	Report   fraudreport.Payload
}

// Periods lists the issues visible at this tier, newest first. Dev builds
// appear for operators only.
func (v *view) Periods(ctx context.Context) ([]PeriodRef, error) {
	if !v.atLeast(TierPublic) {
		return nil, ErrForbidden
	}
	ps, err := v.o.Store.ListFraudReportPeriods(ctx, v.operator())
	if err != nil {
		return nil, err
	}
	out := make([]PeriodRef, 0, len(ps))
	for _, p := range ps {
		out = append(out, PeriodRef{Period: p.Period, Version: p.Version, Revised: p.Revised})
	}
	return out, nil
}

// Snapshot returns the latest visible version of period. The view is audited
// (surface "report_view", detail = period) even when no issue exists, so the
// access log records every report open.
func (v *view) Snapshot(ctx context.Context, period string) (Snapshot, error) {
	if !v.atLeast(TierPublic) {
		return Snapshot{}, ErrForbidden
	}
	var out Snapshot
	var err error
	if period == "" {
		err = ErrNotFound
	} else {
		out, err = v.snapshot(ctx, period)
	}
	if err == nil || errors.Is(err, ErrNotFound) {
		v.audit(ctx, host.EntitlementView, "report_view", period)
	}
	return out, err
}

func (v *view) snapshot(ctx context.Context, period string) (Snapshot, error) {
	snap, err := v.o.Store.GetFraudReportSnapshot(ctx, period, v.operator())
	if errors.Is(err, store.ErrRowMissing) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	var p fraudreport.Payload
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot %s: %w", period, err)
	}
	if v.tier == TierPublic || !v.namingOn(ctx) {
		stripPayload(&p)
	}
	return Snapshot{
		Period: snap.Period, Version: snap.Version, RevisionNote: snap.RevisionNote,
		DevBuild: snap.IncludeUnverified, Report: p,
	}, nil
}

// stripPayload removes everything person-level from a snapshot for the
// public tier: the naming rule's justification (every reader is under an
// access agreement) does not hold for an anonymous audience. Aggregates are
// already k-suppressed at build and pass through verbatim.
func stripPayload(p *fraudreport.Payload) {
	for _, list := range []*[]fraudreport.CaseEntry{
		&p.NewInPeriod.ChargesLaid, &p.NewInPeriod.Convictions, &p.NewInPeriod.AppealOutcomes,
	} {
		for i := range *list {
			e := &(*list)[i]
			e.Names, e.Summary, e.EventDetail = nil, "", ""
		}
	}
}
