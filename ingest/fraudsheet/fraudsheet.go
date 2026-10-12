// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package fraudsheet is the one-off migration of the Fraud/Bribery/
// Corruption spreadsheet into the fraud case tracker. It consumes the
// normalized JSON produced by
// corruption-tracker/tools/sheetnormalize/fraud_sheet_normalize.py (the xlsx
// itself is never parsed in Go), clusters person-rows into cases, and
// writes via the fraud DB layer:
//
//   - clean rows   -> fraud_cases (+participants/orgs/events/sources),
//     review_state=unverified, awaiting the reviewer sign-off pass.
//   - problem rows -> fraud_claims with needs_manual_extraction, into the
//     standard review queue. Nothing is dropped, nothing ambiguous is
//     auto-merged.
//
// Clustering: rows sharing (amount, typology text, year) are candidate
// co-accused of one case, but merge ONLY with corroboration — a co-accused
// note, a shared (non-agency) organisation, or a shared region. Anything
// else stays separate and is reported for the reviewer. The Organisation
// column sometimes holds the prosecuting/victim agency ("Inland Revenue"),
// so agency-resolvable names corroborate nothing and are not imported as
// case organisations.
package fraudsheet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

// Row mirrors one object of
// corruption-tracker/tools/sheetnormalize/fraud_sheet_normalize.py output.
type Row struct {
	Row                     int      `json:"row"`
	FullName                string   `json:"full_name"`
	Gender                  string   `json:"gender"`
	YearConvicted           *int     `json:"year_convicted"`
	ConvictionDate          *string  `json:"conviction_date"`
	ConvictionDatePrecision *string  `json:"conviction_date_precision"`
	AmountNZD               *float64 `json:"amount_nzd"`
	AgencyRaw               string   `json:"agency_raw"`
	RegionRaw               string   `json:"region_raw"`
	PenaltyRaw              string   `json:"penalty_raw"`
	ProfessionRaw           string   `json:"profession_raw"`
	AreaRaw                 string   `json:"area_raw"`
	ConvictionDesc          string   `json:"conviction_desc"`
	AppealRaw               string   `json:"appeal_raw"`
	CompanyRaw              string   `json:"company_raw"`
	OrganisationRaw         string   `json:"organisation_raw"`
	VictimsRaw              string   `json:"victims_raw"`
	Details                 string   `json:"details"`
	Link                    string   `json:"link"`
	PrevConvictionsRaw      string   `json:"prev_convictions_raw"`
	AliasesRaw              string   `json:"aliases_raw"`
	CoAccusedRaw            string   `json:"co_accused_raw"`
	Problems                []string `json:"problems"`
}

// Stats summarises an import run.
type Stats struct {
	Rows            int
	CleanRows       int
	ProblemRows     int
	CasesCreated    int
	RowsMerged      int // clean rows folded into a multi-participant case
	ClaimsCreated   int
	ClaimsDeduped   int
	SkippedExisting int // rows whose content hash is already imported
	AmbiguousGroups []string
}

// Deps wires the importer.
type Deps struct {
	DB     *store.Store
	Log    *slog.Logger
	DryRun bool
}

// resolvers bundles the five taxonomy resolvers.
type resolvers struct {
	typology, agency, region, profession *fraudtracker.TermResolver
}

func (d *Deps) loadResolvers(ctx context.Context) (*resolvers, error) {
	r := &resolvers{}
	for kind, dst := range map[string]**fraudtracker.TermResolver{
		"typology":            &r.typology,
		"agency":              &r.agency,
		"region":              &r.region,
		"profession_category": &r.profession,
	} {
		terms, err := d.DB.LoadFraudTaxonomy(ctx, kind)
		if err != nil {
			return nil, err
		}
		*dst = fraudtracker.NewTermResolver(terms)
	}
	return r, nil
}

// Import runs the migration. Idempotent: a row whose content hash already
// exists in fraud_sources or fraud_claims is skipped, so re-running after a
// partial failure never duplicates.
func (d *Deps) Import(ctx context.Context, rows []Row) (Stats, error) {
	var st Stats
	st.Rows = len(rows)
	res, err := d.loadResolvers(ctx)
	if err != nil {
		return st, err
	}

	var clean, problems []Row
	for _, r := range rows {
		if len(r.Problems) == 0 {
			clean = append(clean, r)
		} else {
			problems = append(problems, r)
		}
	}
	st.CleanRows, st.ProblemRows = len(clean), len(problems)

	groups, ambiguous := clusterRows(clean, res)
	st.AmbiguousGroups = ambiguous

	for _, g := range groups {
		imported, err := d.importGroup(ctx, g, res, &st)
		if err != nil {
			return st, fmt.Errorf("import group (sheet row %d): %w", g[0].Row, err)
		}
		if imported && len(g) > 1 {
			st.RowsMerged += len(g) - 1
		}
	}

	for _, r := range problems {
		created, deduped, err := d.importProblemRow(ctx, r)
		if err != nil {
			return st, fmt.Errorf("import problem row %d: %w", r.Row, err)
		}
		if created {
			st.ClaimsCreated++
		} else if deduped {
			st.ClaimsDeduped++
		} else {
			st.SkippedExisting++
		}
	}
	return st, nil
}

// rowHash is the idempotency key: a stable digest of the normalized row.
func rowHash(r Row) string {
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return "sheet:" + hex.EncodeToString(sum[:])
}

// clusterRows groups clean rows into cases. Returns the groups plus a note
// per candidate cluster that was split for lack of corroboration.
func clusterRows(rows []Row, res *resolvers) ([][]Row, []string) {
	byKey := map[string][]Row{}
	var order []string
	for _, r := range rows {
		k := clusterKey(r)
		if k == "" { // no amount or year: never clustered
			k = fmt.Sprintf("solo-%d", r.Row)
		}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], r)
	}

	var groups [][]Row
	var ambiguous []string
	for _, k := range order {
		g := byKey[k]
		if len(g) == 1 || corroborated(g, res) {
			groups = append(groups, g)
			continue
		}
		ambiguous = append(ambiguous,
			fmt.Sprintf("rows %v share (amount, typology, year) %q but lack corroboration; imported as separate cases", rowNums(g), k))
		for _, r := range g {
			groups = append(groups, []Row{r})
		}
	}
	return groups, ambiguous
}

func rowNums(g []Row) []int {
	out := make([]int, len(g))
	for i, r := range g {
		out[i] = r.Row
	}
	sort.Ints(out)
	return out
}

func clusterKey(r Row) string {
	if r.AmountNZD == nil {
		return ""
	}
	y := rowYear(r)
	if y == "" {
		return ""
	}
	return fmt.Sprintf("%.2f|%s|%s", *r.AmountNZD, fraudtracker.Normalize(r.AreaRaw), y)
}

func rowYear(r Row) string {
	if r.ConvictionDate != nil && len(*r.ConvictionDate) >= 4 {
		return (*r.ConvictionDate)[:4]
	}
	if r.YearConvicted != nil {
		return fmt.Sprintf("%d", *r.YearConvicted)
	}
	return ""
}

// corroborated reports whether a same-key group has evidence it is one case:
// an explicit co-accused note, a Details/Conviction cross-reference to
// another member's surname, one shared non-agency organisation, or one
// shared region.
func corroborated(g []Row, res *resolvers) bool {
	if crossReferenced(g) {
		return true
	}
	orgs := map[string]bool{}
	regions := map[string]bool{}
	for _, r := range g {
		if r.CoAccusedRaw != "" {
			return true
		}
		for _, o := range caseOrgNames(r, res) {
			orgs[fraudtracker.Normalize(o)] = true
		}
		if reg := fraudtracker.Normalize(r.RegionRaw); reg != "" {
			regions[reg] = true
		}
	}
	if len(orgs) == 1 {
		return true
	}
	return len(regions) == 1 && len(regions) > 0 && allHaveRegion(g)
}

// crossReferenced reports whether any member's free text mentions another
// member's surname — e.g. "alongside business partner Hollis Brandt" on one
// row when Brandt is another row of the same (amount, typology, year) group.
func crossReferenced(g []Row) bool {
	for i, r := range g {
		text := strings.ToLower(r.Details + " " + r.ConvictionDesc + " " + r.CoAccusedRaw)
		if text == "  " {
			continue
		}
		for j, other := range g {
			if i == j {
				continue
			}
			name, _ := splitNameAliases(other.FullName, "")
			fields := strings.Fields(strings.ToLower(name))
			if len(fields) == 0 {
				continue
			}
			surname := fields[len(fields)-1]
			if len(surname) >= 4 && strings.Contains(text, surname) {
				return true
			}
		}
	}
	return false
}

func allHaveRegion(g []Row) bool {
	for _, r := range g {
		if fraudtracker.Normalize(r.RegionRaw) == "" {
			return false
		}
	}
	return true
}

// caseOrgNames returns the organisation names a row contributes to the case
// — excluding anything that resolves as an agency (the Organisation column
// often holds "Inland Revenue" etc., which is the agency, not a vehicle).
func caseOrgNames(r Row, res *resolvers) []string {
	var out []string
	for _, n := range []string{r.CompanyRaw, r.OrganisationRaw} {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, isAgency := res.agency.Resolve(n); isAgency {
			continue
		}
		out = append(out, n)
	}
	return out
}

// importGroup writes one case (possibly multi-participant). Returns false
// when every row was already imported (idempotent rerun).
func (d *Deps) importGroup(ctx context.Context, g []Row, res *resolvers, st *Stats) (bool, error) {
	// Idempotency: skip the group if ANY of its rows is already imported
	// (groups are deterministic, so "any" == "all" on a clean rerun).
	for _, r := range g {
		exists, err := d.contentHashExists(ctx, rowHash(r))
		if err != nil {
			return false, err
		}
		if exists {
			st.SkippedExisting += len(g)
			return false, nil
		}
	}

	in := buildCaseInput(g, res)
	if d.DryRun {
		d.Log.Info("dry-run: would create case", "name", in.Case.CaseName,
			"participants", len(in.Participants), "events", len(in.Events))
		st.CasesCreated++
		return true, nil
	}
	c, err := d.DB.CreateFraudCase(ctx, in)
	if err != nil {
		return false, err
	}
	st.CasesCreated++
	d.Log.Debug("imported case", "id", c.ID, "name", c.CaseName, "status", string(c.Status), "rows", rowNums(g))
	return true, nil
}

// buildCaseInput assembles the composite case payload from a row group.
func buildCaseInput(g []Row, res *resolvers) store.FraudCaseInput {
	lead := g[0]
	var in store.FraudCaseInput

	// Typology from the first row whose area resolves; raw text preserved.
	// Combined values ("covid fraud, money laundering") resolve segment-wise:
	// first resolved segment is primary, the rest secondary.
	typRaw := firstNonEmpty(rowsField(g, func(r Row) string { return r.AreaRaw }))
	primaryTyp, secondaryTyps, typLabel := resolveTypologies(typRaw, res.typology)
	in.Case.TypologyID = primaryTyp
	in.SecondaryTypologyIDs = secondaryTyps
	if typLabel == "" {
		typLabel = typRaw
	}
	in.Case.TypologyRaw = typRaw

	regRaw := firstNonEmpty(rowsField(g, func(r Row) string { return r.RegionRaw }))
	if t, ok := res.region.Resolve(regRaw); ok {
		in.Case.RegionID = &t.ID
	}
	in.Case.RegionRaw = regRaw

	agRaw := firstNonEmpty(rowsField(g, func(r Row) string { return r.AgencyRaw }))
	primary, co := resolveAgencies(agRaw, res.agency)
	in.Case.ProsecutingAgencyID = primary
	in.CoAgencyIDs = co
	in.Case.AgencyRaw = agRaw

	in.Case.TotalAmountNZD = lead.AmountNZD
	if lead.AmountNZD != nil {
		basis := "proven"
		in.Case.AmountBasis = &basis
		in.Case.AmountNote = "imported from spreadsheet; basis unconfirmed"
	}

	victims := firstNonEmpty(rowsField(g, func(r Row) string { return r.VictimsRaw }))
	in.Case.VictimClass = victimClass(victims)
	in.Case.VictimDetail = victims

	in.Case.CovidRelated = strings.Contains(strings.ToLower(typRaw), "covid")
	if t, ok := res.typology.Resolve(typRaw); ok && t.Slug == "covid-subsidy-fraud" {
		in.Case.CovidRelated = true
	}

	in.Case.Summary = buildSummary(g)
	in.Case.CaseName = buildCaseName(g, typLabel)

	// Participants — one per row; the case total stays at case level so
	// co-accused never multi-count (individual_amount_nzd left null). The
	// sheet is MOSTLY a convictions list, but rows whose penalty cell reads
	// "not yet"/"pending"/"awaiting trial" are matters still before the
	// courts — those get no outcome (pre-verdict) rather than a fabricated
	// conviction.
	for _, r := range g {
		name, aliases := splitNameAliases(r.FullName, r.AliasesRaw)
		sentType, months := ParseSentence(r.PenaltyRaw)
		p := domain.FraudCaseParticipant{
			Role: "defendant", Gender: r.Gender,
			ProfessionRaw: r.ProfessionRaw,
			SentenceType:  sentType, SentenceDetail: detailOrEmpty(r.PenaltyRaw), SentenceMonths: months,
		}
		if !penaltyPending(r.PenaltyRaw) {
			outcome := "convicted"
			p.Outcome = &outcome
		}
		if t, ok := res.profession.Resolve(r.ProfessionRaw); ok {
			p.ProfessionCategoryID = &t.ID
		}
		in.Participants = append(in.Participants, store.FraudParticipantInput{
			PersonName: name, PersonAliases: aliases, Participant: p,
		})
	}

	// Organisations (agency-resolvable names excluded).
	seenOrg := map[string]bool{}
	for _, r := range g {
		for _, n := range caseOrgNames(r, res) {
			k := fraudtracker.Normalize(n)
			if !seenOrg[k] {
				seenOrg[k] = true
				in.Organisations = append(in.Organisations, store.FraudOrgInput{Name: n, Role: "vehicle"})
			}
		}
	}

	in.Events = buildEvents(g)

	// Provenance: one source per row — its Link when present, otherwise the
	// spreadsheet itself. The row hash doubles as the idempotency key.
	for _, r := range g {
		s := domain.FraudCaseSource{
			ContentHash:  rowHash(r),
			Excerpt:      strings.TrimSpace(r.ConvictionDesc + " " + r.Details),
			LicenceBasis: "manual-research-spreadsheet",
		}
		if r.Link != "" {
			s.URL, s.SourceType, s.Publisher = r.Link, domain.FraudSourceNews, "spreadsheet link"
		} else {
			s.SourceType, s.Publisher = domain.FraudSourceManual, "Fraud/Bribery/Corruption spreadsheet"
		}
		if r.ConvictionDate != nil {
			if t, err := time.Parse("2006-01-02", *r.ConvictionDate); err == nil {
				s.PublishedDate = &t
			}
		}
		in.Sources = append(in.Sources, s)
	}
	return in
}

// penaltyPending reports whether a penalty cell describes a matter still
// PRE-VERDICT ("not yet", "not yet heard", "pending", "awaiting trial").
// "not yet sentenced" / "to be sentenced" are NOT pending — they mean
// convicted and awaiting sentence, which is a conviction.
// placeholderDetail reports whether a sheet cell is a pure placeholder —
// text that only restates an absence the schema already encodes (nil
// outcome, no sentence, undated event). Such values must not be stored:
// "Charges laid — not yet" on a timeline is the sheet leaking through.
// Substantive pending notes ("awaiting trial March 2027") are NOT
// placeholders and are kept.
func placeholderDetail(raw string) bool {
	switch fraudtracker.Normalize(raw) {
	case "not yet", "not yet heard", "not yet sentenced", "pending",
		"awaiting trial", "awaiting sentencing", "awaiting sentence",
		"tbc", "tba", "n/a", "na", "unknown", "?", "-", "none", "nil":
		return true
	}
	return false
}

// detailOrEmpty blanks placeholder cells, keeping substantive text.
func detailOrEmpty(raw string) string {
	if placeholderDetail(raw) {
		return ""
	}
	return raw
}

func penaltyPending(raw string) bool {
	low := strings.ToLower(raw)
	if strings.Contains(low, "sentenc") {
		return false
	}
	for _, w := range []string{"not yet", "pending", "awaiting", "trial set", "trial commencing", "trial date"} {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// buildEvents derives the lifecycle spine available from the sheet: a
// sentencing event for sentenced rows (a charges_laid event instead when
// every penalty in the group is still pending — those matters are before
// the courts, not convictions) and, where the Appeal? cell records a
// decided appeal, an appeal_outcome event. Offending-period dates only
// exist inside Details prose, so they are left for reviewer/LLM passes.
func buildEvents(g []Row) []domain.FraudCaseEvent {
	var events []domain.FraudCaseEvent
	date, prec := earliestDate(g)
	detail := strings.Join(compact(rowsField(g, func(r Row) string { return detailOrEmpty(r.PenaltyRaw) })), "; ")
	evType := domain.EventChargesLaid
	for _, r := range g {
		if !penaltyPending(r.PenaltyRaw) {
			evType = domain.EventSentencing // at least one participant sentenced
			break
		}
	}
	events = append(events, domain.FraudCaseEvent{
		EventType: evType, EventDate: date, EventDatePrecision: prec,
		OutcomeDetail: detail,
	})
	for _, r := range g {
		low := strings.ToLower(r.AppealRaw)
		if strings.Contains(low, "lost appeal") || strings.Contains(low, "appeal dismissed") {
			events = append(events, domain.FraudCaseEvent{
				EventType:     domain.EventAppealOutcome,
				OutcomeDetail: "appeal dismissed — " + r.AppealRaw,
			})
		}
	}
	return events
}

func earliestDate(g []Row) (*time.Time, *string) {
	var best *time.Time
	var prec *string
	for _, r := range g {
		var t time.Time
		var p string
		switch {
		case r.ConvictionDate != nil:
			parsed, err := time.Parse("2006-01-02", *r.ConvictionDate)
			if err != nil {
				continue
			}
			t = parsed
			p = "day"
			if r.ConvictionDatePrecision != nil {
				p = *r.ConvictionDatePrecision
			}
		case r.YearConvicted != nil:
			t = time.Date(*r.YearConvicted, 1, 1, 0, 0, 0, 0, time.UTC)
			p = "year"
		default:
			continue
		}
		if best == nil || t.Before(*best) {
			tt, pp := t, p
			best, prec = &tt, &pp
		}
	}
	return best, prec
}

func buildSummary(g []Row) string {
	var parts []string
	for _, r := range g {
		var s []string
		if r.ConvictionDesc != "" {
			s = append(s, r.ConvictionDesc)
		}
		if r.Details != "" {
			s = append(s, r.Details)
		}
		if r.AppealRaw != "" {
			s = append(s, "Appeal note: "+r.AppealRaw)
		}
		if r.PrevConvictionsRaw != "" {
			s = append(s, "Previous/subsequent: "+r.PrevConvictionsRaw)
		}
		if r.CoAccusedRaw != "" {
			s = append(s, "Co-accused note: "+r.CoAccusedRaw)
		}
		if len(s) > 0 {
			parts = append(parts, r.FullName+": "+strings.Join(s, " | "))
		}
	}
	return strings.Join(parts, "\n")
}

func buildCaseName(g []Row, typLabel string) string {
	var surnames []string
	for _, r := range g {
		name, _ := splitNameAliases(r.FullName, "")
		fields := strings.Fields(name)
		if len(fields) > 0 {
			surnames = append(surnames, fields[len(fields)-1])
		}
	}
	y := rowYear(g[0])
	label := typLabel
	if label == "" {
		label = "fraud"
	}
	return fmt.Sprintf("%s — %s (%s)", strings.Join(surnames, " & "), label, y)
}

// splitNameAliases handles "Marla Fenning aka Quist" plus the Aliases
// column (comma-separated).
func splitNameAliases(fullName, aliasesRaw string) (string, []string) {
	name := strings.TrimSpace(fullName)
	var aliases []string
	if i := strings.Index(strings.ToLower(name), " aka "); i >= 0 {
		aliases = append(aliases, strings.TrimSpace(name[i+5:]))
		name = strings.TrimSpace(name[:i])
	}
	for a := range strings.SplitSeq(aliasesRaw, ",") {
		if a = strings.TrimSpace(a); a != "" {
			aliases = append(aliases, a)
		}
	}
	return name, aliases
}

// resolveTypologies maps an Area-of-Fraud string onto taxonomy terms. The
// whole string is tried first; otherwise it is split on commas, " and ",
// and spaced dashes, and each segment resolved independently — the first
// hit is primary, later distinct hits are secondary.
func resolveTypologies(raw string, r *fraudtracker.TermResolver) (*int64, []int64, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil, ""
	}
	if t, ok := r.Resolve(raw); ok {
		return &t.ID, nil, t.Label
	}
	segments := regexp.MustCompile(`,| and | - |&`).Split(raw, -1)
	var primary *int64
	var label string
	var secondary []int64
	seen := map[int64]bool{}
	for _, seg := range segments {
		t, ok := r.Resolve(seg)
		if !ok || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		if primary == nil {
			id := t.ID
			primary, label = &id, t.Label
		} else {
			secondary = append(secondary, t.ID)
		}
	}
	return primary, secondary, label
}

// resolveAgencies splits a possibly multi-agency string ("msd/ird",
// "IRD MSD SFO", "Police, ACC") and resolves each token. The first resolved
// agency is primary; the rest co-prosecute.
func resolveAgencies(raw string, r *fraudtracker.TermResolver) (*int64, []int64) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, ok := r.Resolve(raw); ok {
		return &t.ID, nil
	}
	tokens := regexp.MustCompile(`[/,]+| and `).Split(raw, -1)
	// All-caps space runs like "IRD MSD SFO": split further on spaces when
	// the whole string didn't resolve and tokens still don't.
	var flat []string
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if _, ok := r.Resolve(tok); ok {
			flat = append(flat, tok)
			continue
		}
		flat = append(flat, strings.Fields(tok)...)
	}
	var ids []int64
	seen := map[int64]bool{}
	for _, tok := range flat {
		if t, ok := r.Resolve(tok); ok && !seen[t.ID] {
			seen[t.ID] = true
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return &ids[0], ids[1:]
}

// sentence parsing -----------------------------------------------------------

var (
	yearsRe  = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:years?|yrs?)`)
	monthsRe = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*months?`)
	halfRe   = regexp.MustCompile(`(\d+)\s+1/2`)
)

// ParseSentence maps free-text penalties onto (sentence_type, custodial
// months). Months are extracted only for imprisonment — "11 months home
// detention" is home_detention with no custodial months. Unrecognised text
// yields (nil, nil); the raw is always kept in sentence_detail.
func ParseSentence(raw string) (*string, *int) {
	low := strings.ToLower(strings.TrimSpace(raw))
	if low == "" {
		return nil, nil
	}
	typ := ""
	switch {
	case strings.Contains(low, "home detention"):
		typ = "home_detention"
	case strings.Contains(low, "community detention"):
		typ = "community_detention"
	case strings.Contains(low, "community work"), strings.Contains(low, "community service"),
		strings.Contains(low, "periodic detention"):
		typ = "community_work"
	case strings.Contains(low, "imprison"), strings.Contains(low, "prison"),
		strings.Contains(low, "jail"), yearsRe.MatchString(low), monthsRe.MatchString(low):
		typ = "imprisonment"
	case strings.Contains(low, "fine"):
		typ = "fine"
	case strings.Contains(low, "reparation"):
		typ = "reparation_only"
	case strings.Contains(low, "discharge"):
		typ = "discharge"
	case strings.Contains(low, "deport"):
		typ = "deportation_followed"
	default:
		return nil, nil
	}
	if typ != "imprisonment" {
		return &typ, nil
	}
	// "8 1/2 years" -> "8.5 years" before numeric extraction.
	norm := halfRe.ReplaceAllString(low, "$1.5")
	months := 0
	if m := yearsRe.FindStringSubmatch(norm); m != nil {
		months += int(parseF(m[1])*12 + 0.5)
	}
	if m := monthsRe.FindStringSubmatch(norm); m != nil {
		months += int(parseF(m[1]) + 0.5)
	}
	if months == 0 {
		return &typ, nil
	}
	return &typ, &months
}

func parseF(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}

// victimClass maps the free-text Victims column onto the controlled enum.
// Multiple distinct classes -> mixed; nothing recognised -> nil.
func victimClass(raw string) *string {
	low := strings.ToLower(raw)
	if strings.TrimSpace(low) == "" {
		return nil
	}
	classes := map[string][]string{
		"taxpayer_central_govt": {"taxpayer", "nz government", "central govt", "inland revenue", "ird", "msd", "acc", "crown", "government agency", "mbie"},
		"local_govt":            {"council", "local gov"},
		"employer":              {"employer"},
		"investors":             {"investor", "shareholder"},
		"consumers":             {"consumer", "customer", "client"},
		"charity":               {"charit", "church"},
		"iwi":                   {"iwi", "kiingitanga", "hapu", "hapū"},
		"individuals":           {"migrant", "worker", "individual", "people", "community", "victims", "family"},
	}
	var hits []string
	for class, kws := range classes {
		for _, kw := range kws {
			if strings.Contains(low, kw) {
				hits = append(hits, class)
				break
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil
	case 1:
		return &hits[0]
	default:
		mixed := "mixed"
		return &mixed
	}
}

// importProblemRow routes one unimportable row to the review queue.
func (d *Deps) importProblemRow(ctx context.Context, r Row) (created, deduped bool, err error) {
	hash := rowHash(r)
	exists, err := d.contentHashExists(ctx, hash)
	if err != nil {
		return false, false, err
	}
	if exists {
		return false, false, nil
	}

	headline := r.FullName
	if headline == "" {
		headline = firstNonEmpty([]string{r.ConvictionDesc, r.Details, r.AreaRaw, "unnamed spreadsheet row"})
	}
	if len(headline) > 120 {
		headline = headline[:120]
	}
	payload, _ := json.Marshal(r)
	key := fmt.Sprintf("sheet-row-%d", r.Row)

	claim := domain.FraudClaim{
		Payload:               payload,
		Headline:              fmt.Sprintf("[sheet row %d] %s", r.Row, headline),
		NeedsManualExtraction: true,
		SourceType:            domain.FraudSourceManual,
		SourcePublisher:       "Fraud/Bribery/Corruption spreadsheet",
		SourceURL:             r.Link,
		LicenceBasis:          "manual-research-spreadsheet",
		Excerpt:               strings.TrimSpace(r.ConvictionDesc + " " + r.Details),
		ContentHash:           hash,
		DedupeKey:             &key,
	}
	if d.DryRun {
		d.Log.Info("dry-run: would queue problem row", "row", r.Row, "problems", r.Problems)
		return true, false, nil
	}
	_, wasCreated, err := d.DB.CreateFraudClaim(ctx, claim, nil, "fraudsheet-import")
	if err != nil {
		return false, false, err
	}
	return wasCreated, !wasCreated, nil
}

func (d *Deps) contentHashExists(ctx context.Context, hash string) (bool, error) {
	var exists bool
	err := d.DB.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM fraud_sources WHERE content_hash = $1)
		    OR EXISTS (SELECT 1 FROM fraud_claims WHERE content_hash = $1)`, hash).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("hash lookup: %w", err)
	}
	return exists, nil
}

// helpers ---------------------------------------------------------------------

func rowsField(g []Row, f func(Row) string) []string {
	out := make([]string, 0, len(g))
	for _, r := range g {
		out = append(out, f(r))
	}
	return out
}

func compact(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmpty(in []string) string {
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
