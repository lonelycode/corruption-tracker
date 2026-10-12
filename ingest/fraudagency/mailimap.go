// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"github.com/lonelycode/corruption-tracker/host"
)

// IMAP front-end for mail ingestion: the ingester IS the mail client. It
// polls a dedicated mailbox (the one subscribed to Courts of NZ / Capital
// Letter alerts), feeds each unseen message through the same ParseEmail ->
// relevance gate -> review queue pipeline as the .eml-directory path, and
// marks messages \Seen once processed. Message-id dedupe in the claims
// table is the real idempotency; the Seen flag just keeps polls cheap.
// No mail client, no export rules, no playbook — a mailbox, four env vars,
// and a cron line.

// IMAPConfig connects one poll run. It is the host's settings type, so the
// mailbox reaches IngestIMAP unchanged from host.Settings.IMAP.
type IMAPConfig = host.IMAPConfig

// openIMAP dials, logs in and opens the folder: read-write for ingestion,
// which marks messages \Seen, or read-only (EXAMINE) for a connection test.
func openIMAP(cfg IMAPConfig, readOnly bool) (*client.Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("imap: no address configured (FRAUD_MAIL_IMAP_ADDR)")
	}
	if cfg.Folder == "" {
		cfg.Folder = "INBOX"
	}
	// Bare hostname → standard IMAPS port, so the env var can be just
	// "imap.example.org".
	if !strings.Contains(cfg.Addr, ":") {
		cfg.Addr += ":993"
	}

	var c *client.Client
	var err error
	if cfg.Insecure {
		c, err = client.Dial(cfg.Addr)
	} else {
		c, err = client.DialTLS(cfg.Addr, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", cfg.Addr, err)
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	if _, err := c.Select(cfg.Folder, readOnly); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("imap select %q: %w", cfg.Folder, err)
	}
	return c, nil
}

// CheckIMAP connects, logs in and opens the folder read-only, then
// disconnects, and reports how many messages it holds. It reads nothing and changes nothing
// (the settings page's "Test connection").
func CheckIMAP(cfg IMAPConfig) (messages uint32, err error) {
	c, err := openIMAP(cfg, true)
	if err != nil {
		return 0, err
	}
	defer c.Logout()
	if mb := c.Mailbox(); mb != nil {
		messages = mb.Messages
	}
	return messages, nil
}

// IngestIMAP processes all unseen messages in the folder.
func (d *Deps) IngestIMAP(ctx context.Context, cfg IMAPConfig) (MailStats, error) {
	var st MailStats
	c, err := openIMAP(cfg, false)
	if err != nil {
		return st, err
	}
	defer c.Logout()

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	ids, err := c.Search(criteria)
	if err != nil {
		return st, fmt.Errorf("imap search: %w", err)
	}
	st.Files = len(ids)
	if len(ids) == 0 {
		return st, nil
	}

	gate, err := d.loadMailGate(ctx)
	if err != nil {
		return st, err
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)
	// Peek: a plain BODY[] fetch implicitly sets \Seen server-side, which
	// would make --dry-run destructive. We mark \Seen explicitly after a
	// successful ingest instead.
	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, 10)
	fetchErr := make(chan error, 1)
	go func() {
		fetchErr <- c.Fetch(seqset, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	processed := new(imap.SeqSet)
	for msg := range messages {
		body := msg.GetBody(section)
		if body == nil {
			st.ParseErrors++
			continue
		}
		// Buffer: the literal is only valid while this message is current.
		raw, err := io.ReadAll(io.LimitReader(body, 4<<20))
		if err != nil {
			st.ParseErrors++
			continue
		}
		em, err := ParseEmail(bytes.NewReader(raw))
		if err != nil {
			st.ParseErrors++
			d.Log.Warn("imap parse failed", "seq", msg.SeqNum, "err", err)
			continue
		}
		ok, err := d.ingestEmail(ctx, gate, em, &st)
		if err != nil {
			st.Errors++
			d.Log.Warn("imap ingest failed", "subject", em.Subject, "err", err)
			continue
		}
		if ok {
			processed.AddNum(msg.SeqNum)
		}
	}
	if err := <-fetchErr; err != nil {
		return st, fmt.Errorf("imap fetch: %w", err)
	}

	// Mark processed messages seen so the next poll skips them. Dedupe on
	// message-id protects against a crash between ingest and store.
	if !processed.Empty() && !d.DryRun {
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		if err := c.Store(processed, item, []interface{}{imap.SeenFlag}, nil); err != nil {
			d.Log.Warn("imap mark seen failed (dedupe will cover the re-poll)", "err", err)
		}
	}
	return st, nil
}
