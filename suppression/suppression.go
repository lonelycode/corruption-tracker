// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package suppression detects name-suppression signals in source text.
//
// It is the tracker's own copy of alitheia's convictions.DetectSuppression
// (same regex bank, same behaviour). alitheia keeps its copy for the
// convictions pipeline; a drift test on the alitheia side runs both over a
// shared fixture list so the two cannot silently diverge.
package suppression

import (
	"regexp"
	"strings"
)

// signals are the phrases that indicate a source may be reporting on a
// matter subject to name suppression. Word-boundary anchored where the term
// could otherwise match inside an unrelated word. Matching is
// case-insensitive.
var signals = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bname suppression\b`),
	regexp.MustCompile(`(?i)\binterim suppression\b`),
	regexp.MustCompile(`(?i)\bsuppression order\b`),
	regexp.MustCompile(`(?i)\bsuppress(?:ed|ion)\b`),
	regexp.MustCompile(`(?i)\bcannot be named\b`),
	regexp.MustCompile(`(?i)\bcan't be named\b`),
	regexp.MustCompile(`(?i)\bhas name suppression\b`),
	// Redaction markers commonly left in published/processed judgments.
	regexp.MustCompile(`(?i)\[\s*name\s+(?:deleted|redacted|suppressed|withheld)\s*\]`),
	regexp.MustCompile(`(?i)\[\s*details?\s+(?:deleted|redacted|suppressed)\s*\]`),
}

// Detect reports whether the text contains any suppression signal, and
// returns the verbatim matched fragments (for the reviewer note / audit). A
// hit routes the claim to the quarantined_suppression state, which a single
// reviewer cannot release.
func Detect(text string) (bool, []string) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	var hits []string
	seen := map[string]bool{}
	for _, re := range signals {
		for _, m := range re.FindAllString(text, -1) {
			key := strings.ToLower(strings.TrimSpace(m))
			if seen[key] {
				continue
			}
			seen[key] = true
			hits = append(hits, strings.TrimSpace(m))
		}
	}
	return len(hits) > 0, hits
}
