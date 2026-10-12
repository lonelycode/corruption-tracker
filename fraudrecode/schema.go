// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudrecode

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/lonelycode/corruption-tracker/llm"
)

// The recode schema is built at runtime from the live taxonomies rather than
// embedded JSON: the whole point of closed-set classification is that the
// model picks from the slugs that actually exist, including any added after
// this binary shipped.

const systemPrompt = `You classify an already-recorded New Zealand fraud/bribery/corruption court case against fixed taxonomies and recover missing dates. You are given the case's stored fields (raw free-text coding, summary, participant charges and sentences, lifecycle events, source excerpts) and sometimes the full text of a linked news article or agency release about the case.

Rules:
- typology_slug: pick the single slug that best describes the PRIMARY offending. Use "" only when the text genuinely supports no choice. Where several kinds of offending appear (e.g. bribery plus money laundering), the primary is the one the prosecution centred on; list the others in typology_secondary.
- typology_confidence: your confidence (0..1) in the primary choice.
- prosecuting_agency_slug: set ONLY when the text explicitly states which agency investigated or brought the prosecution, and copy that statement verbatim into prosecuting_agency_evidence. If no agency is stated, both must be empty — NEVER derive the agency from the offence type.
- region_slug: the NZ region of the offending or court if the text states a locality; "" otherwise.
- sector_slug: the economic sector the offending occurred in (the offender's industry or the defrauded domain), classified onto the fixed list; "" when unclear.
- Dates must come from the text, ISO 8601 (YYYY-MM-DD / YYYY-MM / YYYY), empty when not stated. offending_period is when the offending occurred; charges_laid_date when charges were filed; conviction_date when the person was convicted, pleaded guilty, or was sentenced.
- participant_charges: for each person the text NAMES as a defendant/accused, the charges against them as described, verbatim where possible (e.g. "18 charges of false accounting (six have an alternate charge of obtaining by deception)"). Use the person's name exactly as the text writes it. Omit unnamed persons and persons with no stated charges.
- Extract only what the text states. Never infer, guess, or reconcile conflicts — prefer empty.
This data routes to a coding backfill, not publication.`

// Result mirrors the runtime schema.
type Result struct {
	TypologySlug         string   `json:"typology_slug"`
	TypologySecondary    []string `json:"typology_secondary"`
	TypologyConfidence   float64  `json:"typology_confidence"`
	AgencySlug           string   `json:"prosecuting_agency_slug"`
	AgencyEvidence       string   `json:"prosecuting_agency_evidence"`
	RegionSlug           string   `json:"region_slug"`
	SectorSlug           string   `json:"sector_slug"`
	OffendingPeriodStart string   `json:"offending_period_start"`
	OffendingPeriodEnd   string   `json:"offending_period_end"`
	ChargesLaidDate      string   `json:"charges_laid_date"`
	ConvictionDate       string   `json:"conviction_date"`
	ParticipantCharges   []struct {
		Name    string `json:"name"`
		Charges string `json:"charges"`
	} `json:"participant_charges"`
}

// BuildSchema assembles the JSON schema with live-taxonomy enums. Every enum
// includes "" (unknown) — forcing a choice is how classifiers hallucinate.
func BuildSchema(typologySlugs, agencySlugs, regionSlugs, sectorSlugs []string) json.RawMessage {
	enum := func(slugs []string) []string {
		out := append([]string{""}, slugs...)
		sort.Strings(out[1:])
		return out
	}
	date := func(what string) map[string]any {
		return map[string]any{
			"type":        "string",
			"description": what + ", ISO 8601 (YYYY-MM-DD / YYYY-MM / YYYY) if the text states it. Empty otherwise.",
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"typology_slug", "typology_confidence", "prosecuting_agency_slug", "prosecuting_agency_evidence"},
		"properties": map[string]any{
			"typology_slug": map[string]any{
				"type": "string", "enum": enum(typologySlugs),
				"description": "The primary offending typology, or \"\" when none fits.",
			},
			"typology_secondary": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "enum": enum(typologySlugs)},
				"description": "Additional typologies genuinely present beyond the primary. Usually empty.",
			},
			"typology_confidence": map[string]any{
				"type": "number", "minimum": 0, "maximum": 1,
				"description": "Confidence (0..1) in the primary typology choice.",
			},
			"prosecuting_agency_slug": map[string]any{
				"type": "string", "enum": enum(agencySlugs),
				"description": "The prosecuting agency ONLY if the text states it; \"\" otherwise.",
			},
			"prosecuting_agency_evidence": map[string]any{
				"type":        "string",
				"description": "The verbatim sentence stating the prosecuting agency. Empty when no agency is stated.",
			},
			"region_slug": map[string]any{
				"type": "string", "enum": enum(regionSlugs),
				"description": "The NZ region of the offending/court if stated; \"\" otherwise.",
			},
			"sector_slug": map[string]any{
				"type": "string", "enum": enum(sectorSlugs),
				"description": "The economic sector of the offending (offender's industry or defrauded domain); \"\" when unclear.",
			},
			"offending_period_start": date("When the offending began"),
			"offending_period_end":   date("When the offending ended"),
			"charges_laid_date":      date("When charges were laid/filed"),
			"conviction_date":        date("When convicted, pleaded guilty, or was sentenced"),
			"participant_charges": map[string]any{
				"type":        "array",
				"description": "Per named defendant: the charges against them as the text states. Omit unnamed persons and persons with no stated charges.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"name", "charges"},
					"properties": map[string]any{
						"name":    map[string]any{"type": "string", "description": "The person's name exactly as the text writes it."},
						"charges": map[string]any{"type": "string", "description": "The charges against this person, verbatim where possible."},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("fraudrecode schema marshal: %v", err)) // static shape; cannot fail
	}
	return raw
}

// classify runs one closed-set classification call.
func (d *Deps) classify(ctx context.Context, tax *taxonomies, text string) (*Result, error) {
	resp, err := d.LLM.Complete(ctx, llm.Request{
		System:        systemPrompt,
		User:          text,
		SchemaName:    "fraud_recode_v1",
		Schema:        tax.schema,
		SchemaVersion: "fraud_recode_v1",
		MaxTokens:     1024,
		Temperature:   0,
	})
	if err != nil {
		return nil, err
	}
	var res Result
	if err := json.Unmarshal(resp.Content, &res); err != nil {
		return nil, fmt.Errorf("parse recode result: %w", err)
	}
	return &res, nil
}
