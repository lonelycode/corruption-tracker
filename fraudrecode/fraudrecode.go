// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package fraudrecode is the machine coding-backfill pass for the corruption
// & fraud case tracker. The imported back-catalogue left large coding gaps —
// typology raw text that the deterministic resolver can't map ("false gst
// returns" is a sentence, not an alias), prosecuting agencies the source
// spreadsheet never recorded, missing offending periods — and every new case
// promoted with an unmapped hint lands in the same hole. This pass closes
// those gaps without a reviewer in the loop:
//
//  1. cheap path: re-run the alias resolver over the stored raw text (alias
//     promotion since import may resolve it for free);
//  2. LLM path: one closed-set classification call per case over the case's
//     stored fields plus (robots-permitting) the text of its linked source
//     article — the model picks taxonomy SLUGS from enums, so there is no
//     free-text-to-taxonomy mapping step left to fail;
//  3. inference path: when no source states the prosecuting agency, a
//     conservative typology→agency prior (tax→IRD, benefit→MSD, ...) fills
//     it as second-class data (agency_basis='inferred').
//
// Self-healing: every high-confidence LLM mapping of a SHORT raw term is
// promoted to a taxonomy alias, so the deterministic resolver catches that
// variant on all future ingestion without LLM spend.
//
// Legal posture: this pass codes categorical metadata and dates on cases
// that already exist; it never creates cases, never touches names,
// suppression flags, naming approval, or review state, and never overwrites
// a value a human (or the importer) already set — fills are NULL-only,
// enforced in the DB layer (ApplyFraudRecode).
package fraudrecode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/llm"
)

// agencyPriors maps a typology slug to the agency that prosecutes that
// offending in NZ when the mapping is near-deterministic. Deliberately
// absent: covid-subsidy-fraud (MSD wage subsidy vs IRD CSSP — genuinely
// split), bribery-corruption and public-sector-procurement (SFO vs Police),
// and everything else where guessing would be coding fiction.
var agencyPriors = map[string]string{
	"tax-fraud":               "ird",
	"benefit-fraud":           "msd",
	"cartel-conduct":          "comcom",
	"insider-trading-markets": "fma",
	"immigration-fraud":       "inz",
	"acc-health-fraud":        "acc",
}

// aliasMaxWords bounds alias self-heal: only short raw terms become aliases.
// "false gst returns" generalises; a forty-word charge narrative does not.
const aliasMaxWords = 4

const maxDossierChars = 20000

// Stats summarises one recode run.
type Stats struct {
	Targets          int
	LLMCalls         int
	TypologyCoded    int
	TypologyLowConf  int // suggestion logged, not applied
	AgencyStated     int
	AgencyInferred   int
	RegionCoded      int
	SectorCoded      int
	PeriodsFilled    int
	EventDatesFilled int
	ChargesRecovered int // participant charge texts recovered from sources
	AliasesAdded     int
	FetchErrors      int
	RobotsBlocked    int
	Errors           int
}

// Deps wires a recode run.
type Deps struct {
	DB      *store.Store
	LLM     llm.Completer
	Log     *slog.Logger
	Fetcher *fraudagency.Fetcher
	DryRun  bool
	// NoFetch skips source-article fetches (stored fields only). Fetching is
	// the default: agency statements and offence dates mostly live in the
	// article body, not in what the spreadsheet captured.
	NoFetch bool
	// MinConfidence gates the typology fill (default 0.75). Below it the
	// suggestion is logged for the coverage dashboard's residual tail.
	MinConfidence float64
	Delay         time.Duration // politeness between page fetches
}

// taxonomies bundles the loaded terms the pass classifies against.
type taxonomies struct {
	typBySlug, agBySlug, regBySlug, secBySlug map[string]domain.FraudTaxonomyTerm
	typResolver, agResolver                   *fraudtracker.TermResolver
	schema                                    json.RawMessage
}

// Run executes the recode pass over up to limit cases needing coding.
func (d *Deps) Run(ctx context.Context, limit int, retry bool) (Stats, error) {
	var st Stats
	if d.LLM == nil {
		return st, fmt.Errorf("fraud recode requires an LLM completer")
	}
	if d.Fetcher == nil {
		d.Fetcher = fraudagency.NewFetcher()
	}
	if d.MinConfidence <= 0 {
		d.MinConfidence = 0.75
	}
	if d.Delay == 0 {
		d.Delay = 2 * time.Second
	}

	tax, err := d.loadTaxonomies(ctx)
	if err != nil {
		return st, err
	}

	ids, err := d.DB.ListFraudRecodeTargets(ctx, limit, retry)
	if err != nil {
		return st, err
	}
	st.Targets = len(ids)

	robotsCache := map[string]string{}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if err := d.recodeOne(ctx, id, tax, robotsCache, &st); err != nil {
			st.Errors++
			d.Log.Warn("fraud recode case failed", "case", id, "err", err)
		}
	}
	return st, nil
}

func (d *Deps) loadTaxonomies(ctx context.Context) (*taxonomies, error) {
	t := &taxonomies{
		typBySlug: map[string]domain.FraudTaxonomyTerm{},
		agBySlug:  map[string]domain.FraudTaxonomyTerm{},
		regBySlug: map[string]domain.FraudTaxonomyTerm{},
		secBySlug: map[string]domain.FraudTaxonomyTerm{},
	}
	for kind, dst := range map[string]map[string]domain.FraudTaxonomyTerm{
		"typology": t.typBySlug, "agency": t.agBySlug, "region": t.regBySlug, "sector": t.secBySlug,
	} {
		terms, err := d.DB.LoadFraudTaxonomy(ctx, kind)
		if err != nil {
			return nil, err
		}
		var active []domain.FraudTaxonomyTerm
		for _, term := range terms {
			if term.Active {
				dst[term.Slug] = term
				active = append(active, term)
			}
		}
		switch kind {
		case "typology":
			t.typResolver = fraudtracker.NewTermResolver(active)
		case "agency":
			t.agResolver = fraudtracker.NewTermResolver(active)
		}
	}
	t.schema = BuildSchema(slugs(t.typBySlug), slugs(t.agBySlug), slugs(t.regBySlug), slugs(t.secBySlug))
	return t, nil
}

func slugs(m map[string]domain.FraudTaxonomyTerm) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	return out
}

// needs captures which gaps a case actually has — the LLM is asked for (and
// its answers applied to) only these.
type needs struct {
	typology, agency, region, sector, period bool
	undatedConviction, undatedCharge         bool
	participantCharges                       bool
}

func caseNeeds(c domain.FraudCase, parts []domain.FraudCaseParticipant, events []domain.FraudCaseEvent) needs {
	n := needs{
		typology: c.TypologyID == nil,
		agency:   c.ProsecutingAgencyID == nil,
		region:   c.RegionID == nil,
		sector:   c.SectorID == nil,
		period:   c.OffendingPeriodStart == nil,
	}
	for _, p := range parts {
		if strings.TrimSpace(p.IndividualCharges) == "" {
			n.participantCharges = true
			break
		}
	}
	for _, e := range events {
		if e.EventDate != nil {
			continue
		}
		switch e.EventType {
		case domain.EventVerdict, domain.EventSentencing:
			n.undatedConviction = true
		case domain.EventChargesLaid:
			n.undatedCharge = true
		}
	}
	return n
}

func (n needs) any() bool {
	return n.typology || n.agency || n.region || n.sector || n.period ||
		n.undatedConviction || n.undatedCharge || n.participantCharges
}

func (d *Deps) recodeOne(ctx context.Context, caseID int64, tax *taxonomies, robotsCache map[string]string, st *Stats) error {
	c, parts, events, sources, err := d.DB.GetFraudCase(ctx, caseID)
	if err != nil {
		return err
	}
	n := caseNeeds(c, parts, events)
	if !n.any() {
		// Raced with another fill; just stamp the attempt.
		if !d.DryRun {
			_, err = d.DB.ApplyFraudRecode(ctx, caseID, store.FraudRecodePatch{})
		}
		return err
	}

	var patch store.FraudRecodePatch

	// Cheap path: the deterministic resolver over stored raw text. Aliases
	// promoted since import (including by this pass) resolve for free.
	if n.typology {
		if t, ok := tax.typResolver.Resolve(c.TypologyRaw); ok {
			patch.TypologyID = &t.ID
			n.typology = false
			st.TypologyCoded++
		}
	}
	if n.agency {
		if t, ok := tax.agResolver.Resolve(c.AgencyRaw); ok && t.Slug != "other" {
			patch.AgencyID = &t.ID
			patch.AgencyBasis = "stated" // the sheet's own agency column
			n.agency = false
			st.AgencyStated++
		}
	}

	var res *Result
	if n.any() {
		text, pageFetched := d.dossier(ctx, c, parts, events, sources, robotsCache, st)
		res, err = d.classify(ctx, tax, text)
		if err != nil {
			return fmt.Errorf("classify: %w", err)
		}
		st.LLMCalls++
		_ = pageFetched
		d.applyResult(c, n, res, tax, &patch, st)
	}

	// Inference path: prior on the (possibly just-filled) primary typology.
	if c.ProsecutingAgencyID == nil && patch.AgencyID == nil {
		typSlug := primaryTypologySlug(c, patch, tax)
		if agSlug, ok := agencyPriors[typSlug]; ok {
			if t, ok := tax.agBySlug[agSlug]; ok {
				patch.AgencyID = &t.ID
				patch.AgencyBasis = "inferred"
				st.AgencyInferred++
			}
		}
	}

	if d.DryRun {
		d.Log.Info("dry-run: would recode", "case", caseID,
			"typology", patch.TypologyID != nil, "agency", patch.AgencyID != nil,
			"agency_basis", patch.AgencyBasis, "region", patch.RegionID != nil,
			"period", patch.OffendingStart != nil,
			"conviction_date", patch.ConvictionDate != nil, "charges_date", patch.ChargesDate != nil)
		return nil
	}
	if _, err := d.DB.ApplyFraudRecode(ctx, caseID, patch); err != nil {
		return err
	}
	d.selfHealAlias(ctx, c, patch, res, tax, st)
	return nil
}

// applyResult turns a classification result into patch fields, respecting
// the per-case needs and the confidence gate.
func (d *Deps) applyResult(c domain.FraudCase, n needs, res *Result, tax *taxonomies, patch *store.FraudRecodePatch, st *Stats) {
	if n.typology && res.TypologySlug != "" {
		if t, ok := tax.typBySlug[res.TypologySlug]; ok {
			if res.TypologyConfidence >= d.MinConfidence {
				patch.TypologyID = &t.ID
				st.TypologyCoded++
			} else {
				st.TypologyLowConf++
				d.Log.Info("recode: typology below confidence floor — left uncoded",
					"case", c.ID, "suggestion", res.TypologySlug,
					"confidence", res.TypologyConfidence, "raw", c.TypologyRaw)
			}
		}
	}
	// Secondary typologies only ever ride on a coded primary — "Uncoded
	// typology + Money laundering" is not a coding, it's a contradiction.
	if c.TypologyID != nil || patch.TypologyID != nil {
		for _, s := range res.TypologySecondary {
			if t, ok := tax.typBySlug[s]; ok && s != res.TypologySlug {
				patch.SecondaryTypologyIDs = append(patch.SecondaryTypologyIDs, t.ID)
			}
		}
	}

	// Agency from the LLM requires an evidence quote — the prompt forbids
	// guessing, and an empty quote means it guessed anyway.
	if n.agency && res.AgencySlug != "" && strings.TrimSpace(res.AgencyEvidence) != "" {
		if t, ok := tax.agBySlug[res.AgencySlug]; ok && t.Slug != "other" {
			patch.AgencyID = &t.ID
			patch.AgencyBasis = "stated"
			st.AgencyStated++
		}
	}

	if n.region && res.RegionSlug != "" {
		if t, ok := tax.regBySlug[res.RegionSlug]; ok {
			patch.RegionID = &t.ID
			st.RegionCoded++
		}
	}

	if n.sector && res.SectorSlug != "" {
		if t, ok := tax.secBySlug[res.SectorSlug]; ok && t.Slug != "other" {
			patch.SectorID = &t.ID
			st.SectorCoded++
		}
	}

	if n.period {
		if start := fraudagency.ParseExtractionDate(res.OffendingPeriodStart); start != nil {
			patch.OffendingStart = start
			patch.OffendingEnd = fraudagency.ParseExtractionDate(res.OffendingPeriodEnd)
			p := fraudagency.DatePrecision(res.OffendingPeriodStart)
			patch.OffendingPrecision = &p
			st.PeriodsFilled++
		}
	}
	if n.undatedConviction {
		if t := fraudagency.ParseExtractionDate(res.ConvictionDate); t != nil {
			patch.ConvictionDate = t
			patch.ConvictionPrecision = fraudagency.DatePrecision(res.ConvictionDate)
			st.EventDatesFilled++
		}
	}
	if n.undatedCharge {
		if t := fraudagency.ParseExtractionDate(res.ChargesLaidDate); t != nil {
			patch.ChargesDate = t
			patch.ChargesPrecision = fraudagency.DatePrecision(res.ChargesLaidDate)
			st.EventDatesFilled++
		}
	}

	// Charge descriptions per named participant. Application matches stored
	// person names exactly (case-insensitive) and fills empty columns only —
	// "Mr Quenby"-style partial references simply don't match, which is the
	// conservative outcome.
	if n.participantCharges {
		for _, pc := range res.ParticipantCharges {
			if strings.TrimSpace(pc.Name) == "" || strings.TrimSpace(pc.Charges) == "" {
				continue
			}
			patch.ParticipantCharges = append(patch.ParticipantCharges,
				store.FraudChargeFill{Name: pc.Name, Charges: pc.Charges})
			st.ChargesRecovered++
		}
	}
}

func primaryTypologySlug(c domain.FraudCase, patch store.FraudRecodePatch, tax *taxonomies) string {
	id := c.TypologyID
	if id == nil {
		id = patch.TypologyID
	}
	if id == nil {
		return ""
	}
	for slug, t := range tax.typBySlug {
		if t.ID == *id {
			return slug
		}
	}
	return ""
}

// selfHealAlias promotes a short raw term the LLM mapped with high
// confidence to a taxonomy alias, so the deterministic resolver catches the
// variant on all future ingestion.
func (d *Deps) selfHealAlias(ctx context.Context, c domain.FraudCase, patch store.FraudRecodePatch, res *Result, tax *taxonomies, st *Stats) {
	if res == nil || patch.TypologyID == nil || c.TypologyID != nil {
		return // alias only when the LLM (not the cheap path) did the mapping
	}
	alias := fraudtracker.Normalize(c.TypologyRaw)
	if alias == "" || len(strings.Fields(alias)) > aliasMaxWords {
		return
	}
	if _, taken := tax.typResolver.Resolve(alias); taken {
		return
	}
	if err := d.DB.AddFraudTaxonomyAlias(ctx, "typology", *patch.TypologyID, alias); err != nil {
		d.Log.Warn("alias self-heal failed", "case", c.ID, "alias", alias, "err", err)
		return
	}
	st.AliasesAdded++
	d.Log.Info("alias promoted", "alias", alias, "typology_id", *patch.TypologyID)
}

// dossier assembles the classification input: every stored field that can
// carry a typology/agency/date signal, then (robots permitting) the text of
// the case's linked source articles, newest-attached first.
func (d *Deps) dossier(ctx context.Context, c domain.FraudCase, parts []domain.FraudCaseParticipant, events []domain.FraudCaseEvent, sources []domain.FraudCaseSource, robotsCache map[string]string, st *Stats) (string, bool) {
	var b strings.Builder
	add := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, v)
		}
	}
	add("Case", c.CaseName)
	add("Summary", c.Summary)
	add("Offending type (raw)", c.TypologyRaw)
	add("Prosecuting agency (raw)", c.AgencyRaw)
	add("Region (raw)", c.RegionRaw)
	add("Court", c.CourtName)
	add("Victim detail", c.VictimDetail)
	if c.TotalAmountNZD != nil {
		fmt.Fprintf(&b, "Amount NZD: %.0f\n", *c.TotalAmountNZD)
	}
	for _, p := range parts {
		var bits []string
		for _, s := range []string{p.ProfessionRaw, p.IndividualCharges, p.SentenceDetail} {
			if strings.TrimSpace(s) != "" {
				bits = append(bits, strings.TrimSpace(s))
			}
		}
		if p.Outcome != nil && *p.Outcome != "" {
			bits = append(bits, *p.Outcome)
		}
		if len(bits) > 0 {
			fmt.Fprintf(&b, "Participant: %s\n", strings.Join(bits, " | "))
		}
	}
	for _, e := range events {
		date := "undated"
		if e.EventDate != nil {
			date = e.EventDate.Format("2006-01-02")
		}
		fmt.Fprintf(&b, "Event: %s (%s) %s\n", e.EventType, date, strings.TrimSpace(e.OutcomeDetail))
	}
	for _, s := range sources {
		add("Source excerpt", s.Excerpt)
	}

	pageFetched := false
	if !d.NoFetch {
		for _, s := range sources {
			if s.URL == "" {
				continue
			}
			allowed, err := fraudagency.RobotsAllowedForURL(ctx, d.Fetcher, robotsCache, s.URL)
			if err != nil || !allowed {
				st.RobotsBlocked++
				continue
			}
			html, err := d.Fetcher.Get(ctx, s.URL)
			if err != nil {
				st.FetchErrors++
				d.Log.Debug("recode fetch failed", "case", c.ID, "url", s.URL, "err", err)
				continue
			}
			fmt.Fprintf(&b, "\nLinked source article (%s):\n%s\n", s.URL, fraudagency.HTMLToText(html))
			pageFetched = true
			select {
			case <-ctx.Done():
			case <-time.After(d.Delay):
			}
			break // one article is plenty; the rest are usually duplicates
		}
	}

	text := b.String()
	if len(text) > maxDossierChars {
		text = firstChars(text, maxDossierChars)
	}
	return text, pageFetched
}

func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // back up to a rune boundary
		n--
	}
	return s[:n]
}
