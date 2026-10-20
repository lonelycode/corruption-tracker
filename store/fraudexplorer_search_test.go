// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// The explorer's keyword search follows the naming rule, like the rows it
// returns: a person whose name a row doesn't show (suppression or Clean
// Slate flag, reviewer block, pre-verdict without an agency release) isn't
// matched by name or profession, while charges still match and a nameable
// case is found by name and summary. With naming off, no name or summary
// matches.
func TestExplorerSearchFollowsTheNamingRule(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	verifier, err := st.EnsureActor(ctx, store.Actor{ExternalRef: "local:1", Label: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	sentenced := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	mk := func(name, charges, summary string, p domain.FraudCaseParticipant, ev domain.FraudEventType, hash string) domain.FraudCase {
		t.Helper()
		p.Role, p.IndividualCharges, p.ProfessionRaw = "defendant", charges, name+" profession"
		var date *time.Time
		if ev == domain.EventSentencing {
			date = &sentenced
		}
		c, err := st.CreateFraudCase(ctx, store.FraudCaseInput{
			Case:         domain.FraudCase{CaseName: name + " case", Summary: summary},
			Participants: []store.FraudParticipantInput{{PersonName: name, Participant: p}},
			Events:       []domain.FraudCaseEvent{{EventType: ev, EventDate: date}},
			Sources:      []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: hash}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if ev == domain.EventSentencing {
			if _, err := st.Pool.Exec(ctx, `UPDATE fraud_cases SET status = 'convicted' WHERE id = $1`, c.ID); err != nil {
				t.Fatal(err)
			}
		} else if _, err := st.Pool.Exec(ctx, `UPDATE fraud_cases SET status = 'charges_laid' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.VerifyFraudCase(ctx, c.ID, verifier); err != nil {
			t.Fatal(err)
		}
		return c
	}
	mk("Sealed Searchperson", "sealedcharge laundering", "Sealed Searchperson laundered funds.",
		domain.FraudCaseParticipant{SuppressionFlag: true}, domain.EventSentencing, "search-1")
	mk("Slate Searchperson", "slatecharge theft", "Slate Searchperson took funds.",
		domain.FraudCaseParticipant{CleanSlatePossible: true}, domain.EventSentencing, "search-2")
	blocked := mk("Blocked Searchperson", "blockedcharge fraud", "Blocked Searchperson defrauded a trust.",
		domain.FraudCaseParticipant{}, domain.EventSentencing, "search-3")
	if err := st.SetFraudCaseNamingApproved(ctx, blocked.ID, false); err != nil {
		t.Fatal(err)
	}
	mk("Alleged Searchperson", "allegedcharge deception", "Alleged Searchperson is charged.",
		domain.FraudCaseParticipant{}, domain.EventChargesLaid, "search-4")
	mk("Visible Searchperson", "visiblecharge fraud", "Visible Searchperson was sentenced.",
		domain.FraudCaseParticipant{}, domain.EventSentencing, "search-5")

	count := func(q string, namingOff bool) int {
		t.Helper()
		_, total, err := st.SearchFraudCases(ctx, store.FraudExplorerFilter{Query: q, ReviewState: "verified", NoIdentitySearch: namingOff})
		if err != nil {
			t.Fatal(err)
		}
		return total
	}
	for _, who := range []string{"Sealed", "Slate", "Blocked", "Alleged"} {
		for _, q := range []string{who + " Searchperson", who + " Searchperson profession"} {
			if n := count(q, false); n != 0 {
				t.Errorf("search %q found %d case(s); a name the row doesn't show must not match", q, n)
			}
		}
		if n := count(strings.ToLower(who)+"charge", false); n != 1 {
			t.Errorf("charges of the %s case: %d matches, want 1 (charges stay searchable)", who, n)
		}
	}
	if n := count("Visible Searchperson", false); n != 1 {
		t.Errorf("a nameable case by name: %d matches, want 1", n)
	}
	if n := count("was sentenced", false); n != 1 {
		t.Errorf("a nameable case by its summary: %d matches, want 1", n)
	}
	// Naming off for the instance: not even the nameable case's name matches.
	if n := count("Visible Searchperson", true); n != 0 {
		t.Errorf("with naming off, a name matched %d case(s)", n)
	}
	if n := count("visiblecharge", true); n != 1 {
		t.Errorf("with naming off, charges: %d matches, want 1", n)
	}
}
