// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/host"
)

// DigestRow is one verified case prepared for the period digest export
// under the page privacy rules: no internal labels, no excerpts; names,
// summary and court identifiers only under the naming rule.
type DigestRow struct {
	ID             int64
	Status         string
	Alleged        bool
	Labels         Labels
	CourtName      string
	CourtID        string // "; "-joined identifiers, "" unless named
	Amount         *float64
	AmountBasis    *string
	VictimClass    *string
	CovidRelated   bool
	OffendingStart *time.Time
	OffendingEnd   *time.Time
	NParts         int
	Names          []string      // empty unless named
	Summary        string        // ditto
	Events         []DigestEvent // events inside the period window
	Sources        []SourceLink
}

// DigestEvent is one dated event inside the digest period.
type DigestEvent struct {
	Date time.Time
	Type string
}

// Digest returns the period's verified cases (exports never include
// unverified cases, operators included), ordered by case id, and audits the
// export ("export", detail "<period> <format> (<n> cases)").
func (v *view) Digest(ctx context.Context, period, format string) ([]DigestRow, error) {
	if !v.atLeast(TierExport) {
		return nil, ErrForbidden
	}
	_, start, end, err := fraudreport.PeriodBounds(period)
	if err != nil {
		return nil, err
	}
	st := v.o.Store
	ids, err := st.ListFraudCaseIDsWithEventsIn(ctx, start, end)
	if err != nil {
		return nil, fmt.Errorf("digest case ids: %w", err)
	}
	naming := v.namingOn(ctx)
	rows := make([]DigestRow, 0, len(ids))
	for _, id := range ids {
		c, parts, events, sources, err := st.GetFraudCase(ctx, id)
		if err != nil {
			v.o.Log.Error("fraud export case", "case", id, "err", err)
			continue
		}
		if c.ReviewState != "verified" {
			continue
		}
		labels, err := st.GetFraudCaseLabels(ctx, id)
		if err != nil {
			v.o.Log.Error("fraud export labels", "case", id, "err", err)
			continue
		}
		row := DigestRow{
			ID: c.ID, Status: string(c.Status), Alleged: c.Status.PreVerdict(),
			Labels: Labels{
				Typology: labels.Typology, Region: labels.Region, Agency: labels.Agency,
				Sector: labels.Sector, AgencyInferred: labels.AgencyInferred,
			},
			CourtName: c.CourtName, Amount: c.TotalAmountNZD, AmountBasis: c.AmountBasis,
			VictimClass: c.VictimClass, CovidRelated: c.CovidRelated,
			OffendingStart: c.OffendingPeriodStart, OffendingEnd: c.OffendingPeriodEnd,
			NParts: len(parts),
		}
		for _, src := range sources {
			row.Sources = append(row.Sources, SourceLink{
				URL: src.URL, Publisher: src.Publisher,
				Type: string(src.SourceType), PublishedDate: src.PublishedDate,
			})
		}
		for _, e := range events {
			if e.EventDate != nil && !e.EventDate.Before(start) && e.EventDate.Before(end) {
				row.Events = append(row.Events, DigestEvent{Date: *e.EventDate, Type: string(e.EventType)})
			}
		}
		if naming && NamingAllowed(c, parts, sources) {
			for _, p := range parts {
				row.Names = append(row.Names, p.PersonName)
			}
			row.Summary = c.Summary
			row.CourtID = strings.Join(c.CourtIdentifiers, "; ")
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	// Audit: exports are the highest-leverage access there is.
	v.audit(ctx, host.EntitlementExport, "export", fmt.Sprintf("%s %s (%d cases)", period, format, len(rows)))
	return rows, nil
}
