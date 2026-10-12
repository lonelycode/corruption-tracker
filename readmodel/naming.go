// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package readmodel

import "github.com/lonelycode/corruption-tracker/domain"

// NamingAllowed is THE naming rule, shared by the case page and the digest
// export (the snapshot builder and the explorer's SQL apply the same logic):
//
//   - not reviewer-blocked (naming_approved defaults TRUE since 0128 — every
//     tracker surface is org-gated under an access agreement, so the
//     editorial gate is a block-list, not an allow-list);
//   - no participant suppression/Clean Slate flag — absolute, one flagged
//     participant withholds the whole case;
//   - pre-verdict cases are named ONLY when the case carries an
//     agency_release source: the prosecuting agency itself published the
//     matter, so we mirror the prosecutor's own naming (SFO/IRD releases
//     name charged persons routinely). News-sourced or manually-entered
//     pre-verdict cases stay unnamed — interim suppression at first
//     appearance is exactly the case a reporter may have missed.
//
// A suppression order made AFTER the agency release is the residual risk;
// the suppression screen quarantines follow-up coverage that mentions it,
// and the Notice puts pre-publication vetting on the receiving org.
func NamingAllowed(c domain.FraudCase, parts []domain.FraudCaseParticipant, sources []domain.FraudCaseSource) bool {
	if !c.NamingApproved {
		return false
	}
	for _, p := range parts {
		if p.SuppressionFlag || p.CleanSlatePossible {
			return false
		}
	}
	if c.Status.PreVerdict() && !hasAgencyRelease(sources) {
		return false
	}
	return true
}

// hasAgencyRelease reports whether any case source is an official agency
// media release (the prosecutor went public, names included).
func hasAgencyRelease(sources []domain.FraudCaseSource) bool {
	for _, s := range sources {
		if s.SourceType == domain.FraudSourceAgencyRelease {
			return true
		}
	}
	return false
}

// NoticeFor builds the suppression / derivative-work disclaimer naming the
// operator. Embedded in alitheia, with that host's operator name, it is
// byte-identical to the notice alitheia shipped before the split.
func NoticeFor(operator string) string {
	return "Name suppression orders may be in force in any matter. " +
		operator + " screens for suppression on a best-efforts basis and withholds names where a signal is found, " +
		"but suppression decisions are not comprehensively published and orders can be made or varied after our " +
		"sources were captured. It is your organisation's responsibility to vet and verify any derivative work " +
		"based on data supplied by " + operator + " — including the current suppression status of any named person — " +
		"before publication."
}
