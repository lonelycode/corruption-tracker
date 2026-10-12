// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
)

// Case is one case's org-facing view. Only sanitized fields exist: no
// internal case_name, no source excerpts, and names, professions, the
// summary, sentence free text, per-event outcome detail and court
// identifiers are empty unless NamesShown.
type Case struct {
	ID               int64
	Status           string
	Alleged          bool // pre-verdict: render the ALLEGED badge
	Unverified       bool // only ever true at the operator tier
	NamesShown       bool
	NamingOff        bool // names withheld because naming is off for this tracker
	Names            []string
	Summary          string
	ParticipantCount int

	Labels              Labels
	SecondaryTypologies []string
	Participants        []Participant
	Amount              *float64
	AmountBasis         *string
	CourtName           string
	CourtIdentifiers    []string
	VictimClass         *string
	CovidRelated        bool
	OffendingStart      *time.Time
	OffendingEnd        *time.Time
	Lag                 *Lag
	Orgs                []Org
	Events              []Event // storage order; presentation orders them
	Sources             []SourceLink
}

// Labels are the case's taxonomy labels.
type Labels struct {
	Typology, Region, Agency, Sector string
	AgencyInferred                   bool // coded from the offence type, not stated by a source
}

// Participant carries charges and outcomes (analytical substance, no
// identity) always; name and profession only under the naming rule.
type Participant struct {
	Name       string // "" when withheld
	Profession string // "" when withheld
	Charges    string
	Outcome    string
	Sentence   string // type · months · free text (free text only when named)
}

// Lag is the offence→charge→conviction interval view for one case.
type Lag struct {
	OffenceToConvictionDays *int
	ChargeToConvictionDays  *int
	Approximate             bool // month/year-precision inputs
}

// Org is an organisation linked to the case.
type Org struct {
	Name string
	NZBN *string
	Role string
}

// Event is one court-lifecycle step.
type Event struct {
	Type      string
	Date      *time.Time
	Precision string
	Court     string
	Detail    string // "" when naming is withheld (outcome text embeds names)
	SourceURL string
	Publisher string
}

// SourceLink is a provenance link (URLs and publishers only; excerpts never
// leave core).
type SourceLink struct {
	URL, Publisher, Type string
	PublishedDate        *time.Time
}

// Case returns the case at this tier: verified cases from the viewer tier
// up, unverified ones to operators only. Anything else is ErrNotFound so the
// id's existence is never confirmed. The view is audited ("case_view").
func (v *view) Case(ctx context.Context, id int64) (Case, error) {
	if !v.atLeast(TierViewer) {
		return Case{}, ErrForbidden
	}
	st := v.o.Store
	c, parts, events, sources, err := st.GetFraudCase(ctx, id)
	if errors.Is(err, store.ErrRowMissing) {
		return Case{}, ErrNotFound
	}
	if err != nil {
		return Case{}, err
	}
	if c.ReviewState != "verified" && !v.operator() {
		return Case{}, ErrNotFound
	}
	labels, err := st.GetFraudCaseLabels(ctx, id)
	if err != nil {
		return Case{}, fmt.Errorf("case labels: %w", err)
	}
	orgs, err := st.ListFraudCaseOrganisations(ctx, id)
	if err != nil {
		return Case{}, fmt.Errorf("case orgs: %w", err)
	}
	secondary, err := st.ListFraudCaseSecondaryTypologies(ctx, id)
	if err != nil {
		return Case{}, fmt.Errorf("case secondary typologies: %w", err)
	}
	lags, err := st.GetFraudCaseLags(ctx, []int64{id})
	if err != nil {
		return Case{}, fmt.Errorf("case lags: %w", err)
	}

	on := v.namingOn(ctx)
	naming := on && NamingAllowed(c, parts, sources)
	out := Case{
		ID: c.ID, Status: string(c.Status), Alleged: c.Status.PreVerdict(),
		Unverified: c.ReviewState != "verified", NamesShown: naming, NamingOff: !on,
		ParticipantCount: len(parts),
		Labels: Labels{
			Typology: labels.Typology, Region: labels.Region, Agency: labels.Agency,
			Sector: labels.Sector, AgencyInferred: labels.AgencyInferred,
		},
		SecondaryTypologies: secondary,
		Amount:              c.TotalAmountNZD, AmountBasis: c.AmountBasis,
		CourtName: c.CourtName, VictimClass: c.VictimClass, CovidRelated: c.CovidRelated,
		OffendingStart: c.OffendingPeriodStart, OffendingEnd: c.OffendingPeriodEnd,
	}
	if l, ok := lags[id]; ok && (l.OffenceToConvictionDays != nil || l.ChargeToConvictionDays != nil) {
		out.Lag = &Lag{
			OffenceToConvictionDays: l.OffenceToConvictionDays,
			ChargeToConvictionDays:  l.ChargeToConvictionDays,
			Approximate:             l.Approximate,
		}
	}
	if naming {
		out.Summary = c.Summary
		// Identifiers follow the naming rule: a citation locates the
		// judgment and thereby the name of an otherwise-unnamed case.
		out.CourtIdentifiers = c.CourtIdentifiers
		for _, p := range parts {
			out.Names = append(out.Names, p.PersonName)
		}
	}
	for _, p := range parts {
		out.Participants = append(out.Participants, participantView(p, naming))
	}
	for _, o := range orgs {
		out.Orgs = append(out.Orgs, Org{Name: o.Name, NZBN: o.NZBN, Role: o.Role})
	}
	srcByID := map[int64]domain.FraudCaseSource{}
	for _, src := range sources {
		srcByID[src.ID] = src
		out.Sources = append(out.Sources, SourceLink{
			URL: src.URL, Publisher: src.Publisher,
			Type: string(src.SourceType), PublishedDate: src.PublishedDate,
		})
	}
	for _, e := range events {
		ev := Event{Type: string(e.EventType), Date: e.EventDate, Court: e.Court}
		if e.EventDatePrecision != nil {
			ev.Precision = *e.EventDatePrecision
		}
		if naming {
			ev.Detail = e.OutcomeDetail
		}
		if e.SourceID != nil {
			if src, ok := srcByID[*e.SourceID]; ok {
				ev.SourceURL, ev.Publisher = src.URL, src.Publisher
			}
		}
		out.Events = append(out.Events, ev)
	}

	v.audit(ctx, host.EntitlementView, "case_view", fmt.Sprintf("case %d", id))
	return out, nil
}

// participantView applies the naming rule to one participant. Charges and
// outcomes carry no identity and always render; name and profession
// (identifying in small markets) and sentence free text (which routinely
// embeds names) follow the rule.
func participantView(p domain.FraudCaseParticipant, naming bool) Participant {
	out := Participant{Charges: p.IndividualCharges}
	if naming {
		out.Name = p.PersonName
		out.Profession = p.ProfessionRaw
	}
	if p.Outcome != nil {
		out.Outcome = *p.Outcome
	}
	var bits []string
	if p.SentenceType != nil && *p.SentenceType != "" {
		bits = append(bits, strings.ReplaceAll(*p.SentenceType, "_", " "))
	}
	if p.SentenceMonths != nil {
		bits = append(bits, fmt.Sprintf("%d months", *p.SentenceMonths))
	}
	if naming && p.SentenceDetail != "" {
		bits = append(bits, p.SentenceDetail)
	}
	out.Sentence = strings.Join(bits, " · ")
	return out
}
