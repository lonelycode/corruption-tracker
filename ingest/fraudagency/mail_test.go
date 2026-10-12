// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/lonelycode/corruption-tracker/domain"
)

const terseAlertEml = `From: Courts of New Zealand <noreply@courtsofnz.govt.nz>
To: alerts@tracker.example
Subject: Judgment published: Mailcase v R
Message-Id: <terse-1@courtsofnz.govt.nz>
Date: Tue, 10 Jun 2026 09:00:00 +1200
Content-Type: text/plain; charset=utf-8

The decision in Mailcase v R (CA77/2026) has been published.
https://www.courtsofnz.govt.nz/the-courts/court-of-appeal/judgments/mailcase
`

const richDigestEml = `From: Capital Letter <digest@capitalletter.co.nz>
To: alerts@tracker.example
Subject: =?utf-8?q?Daily_digest_=E2=80=94_judgments?=
Message-Id: <digest-1@capitalletter.co.nz>
Date: Tue, 10 Jun 2026 08:00:00 +1200
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="b1"

--b1
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Court of Appeal =E2=80=94 criminal. The appellant had been sentenced for w=
age subsidy fraud totalling $480,000. The appeal against sentence was dism=
issed. The court held the starting point was available. Citation: [2026] N=
ZCA 555. Padding so the body comfortably exceeds the minimum extractable l=
ength for the pipeline: the judgment discusses deception, dishonesty offen=
ces and the approach to COVID relief fraud sentencing at some length, incl=
uding comparisons with earlier wage subsidy fraud appeals and restitution =
considerations relevant to reparations orders made at sentencing.

--b1
Content-Type: text/html; charset=utf-8

<html><body><p>HTML twin</p></body></html>
--b1--
`

const irrelevantEml = `From: Courts of New Zealand <noreply@courtsofnz.govt.nz>
To: alerts@tracker.example
Subject: Judgment published: Body Corporate 12345 v Council
Message-Id: <irrelevant-1@courtsofnz.govt.nz>
Date: Tue, 10 Jun 2026 09:05:00 +1200
Content-Type: text/plain; charset=utf-8

The decision in Body Corporate 12345 v Council (leaky building, weathertightness) has been published.
`

func TestParseEmail(t *testing.T) {
	em, err := ParseEmail(strings.NewReader(richDigestEml))
	if err != nil {
		t.Fatal(err)
	}
	if em.Subject != "Daily digest — judgments" {
		t.Errorf("subject = %q", em.Subject)
	}
	if em.MessageID != "digest-1@capitalletter.co.nz" {
		t.Errorf("message id = %q", em.MessageID)
	}
	if !strings.Contains(em.Text, "wage subsidy fraud totalling $480,000") {
		t.Errorf("quoted-printable body not decoded: %q", em.Text[:120])
	}

	em, err = ParseEmail(strings.NewReader(terseAlertEml))
	if err != nil {
		t.Fatal(err)
	}
	if len(em.Links) != 1 || !strings.Contains(em.Links[0], "courtsofnz.govt.nz") {
		t.Errorf("links = %v", em.Links)
	}
}

func TestIngestMailDir(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	// A tracked case whose CA number the terse alert cites.
	if _, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case: domain.FraudCase{CaseName: "Mail Target", CourtIdentifiers: []string{"CA77/2026"}},
		Participants: []store.FraudParticipantInput{{
			PersonName:  "Mailcase Person",
			Participant: domain.FraudCaseParticipant{Role: "defendant"},
		}},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: "mail-seed"}},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archive := t.TempDir()
	for name, body := range map[string]string{
		"terse.eml":      terseAlertEml,
		"digest.eml":     richDigestEml,
		"irrelevant.eml": irrelevantEml,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	deps := &Deps{DB: d, Log: slog.Default(),
		LLM: stubCompleter{payload: `{"is_fraud_case":true,"case_stage":"appeal_outcome",
			"case_name_hint":"appeal against sentence dismissed",
			"court_identifiers":["[2026] NZCA 555"],
			"event_date":"2026-06-09","typology_hint":"wage subsidy fraud",
			"exact_excerpt":"The appeal against sentence was dismissed.","confidence":0.9}`}}

	st, err := deps.IngestMailDir(ctx, dir, archive)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if st.Files != 3 || st.Irrelevant != 1 || st.ClaimsNew != 2 || st.ManualQueued != 1 || st.Errors != 0 {
		t.Fatalf("stats: %+v", st)
	}

	// Terse alert: queued manual (nothing to extract), pre-suggested onto
	// the tracked case via its CA number.
	claims, err := d.ListFraudClaims(ctx, store.FraudClaimFilter{State: "pending", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var terse, rich *domain.FraudClaim
	for i := range claims {
		if strings.Contains(claims[i].Headline, "Mailcase") {
			terse = &claims[i]
		}
		if strings.Contains(claims[i].Headline, "Daily digest") {
			rich = &claims[i]
		}
	}
	if terse == nil || !terse.NeedsManualExtraction {
		t.Fatalf("terse alert claim wrong: %+v", terse)
	}
	cands, _ := d.ListFraudCaseMatchCandidates(ctx, terse.ID)
	if len(cands) != 1 || cands[0].MatchMethod != "court_id" {
		t.Fatalf("terse alert should match the tracked case by citation: %+v", cands)
	}
	if rich == nil || rich.NeedsManualExtraction || len(rich.Payload) == 0 {
		t.Fatalf("digest claim should carry an extraction: %+v", rich)
	}

	// All three processed files were archived; re-running the (now empty)
	// dir is a no-op, and re-ingesting the same messages dedupes.
	left, _ := os.ReadDir(dir)
	if len(left) != 0 {
		t.Errorf("dir should be drained, %d left", len(left))
	}
	if err := os.WriteFile(filepath.Join(dir, "terse.eml"), []byte(terseAlertEml), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = deps.IngestMailDir(ctx, dir, "")
	if err != nil || st.Duplicates != 1 || st.ClaimsNew != 0 {
		t.Fatalf("rerun should dedupe: %+v err=%v", st, err)
	}
}
