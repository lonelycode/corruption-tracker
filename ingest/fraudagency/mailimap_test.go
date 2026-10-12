// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"

	"github.com/lonelycode/corruption-tracker/domain"
)

// In-memory IMAP server round-trip: unseen alert -> claim -> marked seen ->
// second poll is a no-op.
func TestIngestIMAP(t *testing.T) {
	d := storetest.Open(t)
	ctx := context.Background()

	if _, err := d.CreateFraudCase(ctx, store.FraudCaseInput{
		Case:    domain.FraudCase{CaseName: "IMAP Target", CourtIdentifiers: []string{"CA88/2026"}},
		Sources: []domain.FraudCaseSource{{SourceType: domain.FraudSourceManual, ContentHash: "imap-seed"}},
	}); err != nil {
		t.Fatal(err)
	}

	// The memory backend ships with user "username"/"password" and an INBOX
	// containing one sample (seen) message.
	be := memory.New()
	srv := server.New(be)
	srv.AllowInsecureAuth = true
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { _ = srv.Serve(l) }()

	user, err := be.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	mbox, err := user.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	alert := strings.ReplaceAll(terseAlertEml, "CA77/2026", "CA88/2026")
	if err := mbox.CreateMessage([]string{}, time.Now(), bytes.NewBufferString(alert)); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{DB: d, Log: slog.Default()} // no LLM: terse path
	cfg := IMAPConfig{Addr: l.Addr().String(), Username: "username", Password: "password", Insecure: true}

	st, err := deps.IngestIMAP(ctx, cfg)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if st.ClaimsNew != 1 || st.Errors != 0 {
		t.Fatalf("stats: %+v", st)
	}
	claims, err := d.ListFraudClaims(ctx, store.FraudClaimFilter{State: "pending", Limit: 10})
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %d err=%v", len(claims), err)
	}
	cands, _ := d.ListFraudCaseMatchCandidates(ctx, claims[0].ID)
	if len(cands) != 1 || cands[0].MatchMethod != "court_id" {
		t.Fatalf("alert should pre-match the tracked case: %+v", cands)
	}

	// Second poll: the message is now \Seen, nothing to do.
	st, err = deps.IngestIMAP(ctx, cfg)
	if err != nil || st.Files != 0 {
		t.Fatalf("second poll should find nothing unseen: %+v err=%v", st, err)
	}
}
