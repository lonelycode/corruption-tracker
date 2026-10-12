// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Command mailpeek lists the fraud-tracker alert mailbox read-only (PEEK —
// flags untouched): sender, subject, seen-state, and any links, so an
// operator can check subscriptions/confirmations without a mail client.
//
// The mailbox comes from the FRAUD_MAIL_IMAP_* environment variables (the
// same ones `fraud mail-ingest --imap` uses); export them first, e.g. from
// alitheia's .env with `set -a; . ./.env; set +a`:
//
//	go run ./corruption-tracker/tools/mailpeek [-n 20]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
)

func main() {
	n := flag.Int("n", 20, "show the most recent N messages")
	flag.Parse()
	if err := run(*n); err != nil {
		fmt.Fprintln(os.Stderr, "mailpeek:", err)
		os.Exit(1)
	}
}

func run(n int) error {
	addr := os.Getenv("FRAUD_MAIL_IMAP_ADDR")
	folder := os.Getenv("FRAUD_MAIL_IMAP_FOLDER")
	if folder == "" {
		folder = "INBOX"
	}
	if addr == "" {
		return fmt.Errorf("FRAUD_MAIL_IMAP_ADDR not set")
	}
	if !strings.Contains(addr, ":") {
		addr += ":993"
	}
	c, err := client.DialTLS(addr, nil)
	if err != nil {
		return err
	}
	defer c.Logout()
	if err := c.Login(os.Getenv("FRAUD_MAIL_IMAP_USER"), os.Getenv("FRAUD_MAIL_IMAP_PASSWORD")); err != nil {
		return err
	}
	mbox, err := c.Select(folder, true) // read-only select
	if err != nil {
		return err
	}
	fmt.Printf("Folder %s: %d messages, %d recent, %d unseen-from\n\n",
		folder, mbox.Messages, mbox.Recent, mbox.UnseenSeqNum)
	if mbox.Messages == 0 {
		return nil
	}

	from := uint32(1)
	if int(mbox.Messages) > n {
		from = mbox.Messages - uint32(n) + 1
	}
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)
	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchFlags, section.FetchItem()}, messages)
	}()

	for msg := range messages {
		seen := ""
		for _, f := range msg.Flags {
			if f == imap.SeenFlag {
				seen = " [seen]"
			}
		}
		body := msg.GetBody(section)
		if body == nil {
			fmt.Printf("#%d%s (no body)\n", msg.SeqNum, seen)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(body, 4<<20))
		em, err := fraudagency.ParseEmail(strings.NewReader(string(raw)))
		if err != nil {
			fmt.Printf("#%d%s parse error: %v\n", msg.SeqNum, seen, err)
			continue
		}
		date := ""
		if em.Date != nil {
			date = em.Date.Format("2 Jan 15:04")
		}
		fmt.Printf("#%d%s %s\n  From: %s\n  Subj: %s\n", msg.SeqNum, seen, date, em.From, em.Subject)
		for _, l := range em.Links {
			fmt.Printf("  Link: %s\n", l)
		}
		preview := strings.Join(strings.Fields(em.Text), " ")
		if len(preview) > 220 {
			preview = preview[:220] + "…"
		}
		if preview != "" {
			fmt.Printf("  Text: %s\n", preview)
		}
		fmt.Println()
	}
	return <-done
}
