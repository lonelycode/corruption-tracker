// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package cli is the tracker's subcommand dispatch, shared by alitheia's
// `ingester fraud ...` and the standalone binary. Hosts supply a Deps; the
// commands themselves never read host config. See
// design §5.8.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/llm"
	"github.com/lonelycode/corruption-tracker/store"
)

// Deps wires the CLI to its host.
type Deps struct {
	// Open connects to the tracker database. Each subcommand calls it once,
	// after its flags parse, so --help and usage errors need no database.
	Open func(ctx context.Context) (*Conn, error)
	// LLM builds the text completer; a nil result means no LLM configured.
	LLM func() llm.Completer
	// Settings may be nil, which reads as "nothing configured".
	Settings host.Settings
	// Log defaults to slog.Default().
	Log *slog.Logger
	// RunCompleted, when set, receives every run's outcome: the job source
	// (e.g. "fraud poll"), start time, units touched, units that produced
	// new state, and the returned error.
	RunCompleted func(source string, started time.Time, fetched, newCount int, err error)
	// Secrets decrypts stored secret settings for `export
	// --include-secrets` (standalone only; nil elsewhere).
	Secrets bundle.SecretOpener
	// Backup takes a backup of the given kind (standalone only, package
	// backup) and returns its name; nil where the host backs up its own
	// database.
	Backup func(ctx context.Context, kind string) (string, error)
	// SourceKind is what `export` records as the bundle's source kind:
	// bundle.KindAlitheiaEmbedded or bundle.KindStandalone.
	SourceKind string
}

// NewsRefresher is a news source that fetches its own items (standalone's
// feeds, ingest/newsfeed). classify-news refreshes it before listing, so
// one scheduled run polls and classifies. alitheia's corpus adapter isn't
// one: alitheia ingests news on its own schedule.
type NewsRefresher interface {
	RefreshNews(ctx context.Context) (newItems int, err error)
}

// Conn is an open database connection and what hangs off it.
type Conn struct {
	Store *store.Store
	// News is classify-news's corpus; it may also be a NewsRefresher.
	News host.NewsItemSource
	// Close releases the connection; may be nil.
	Close func()
}

func (c *Conn) close() {
	if c.Close != nil {
		c.Close()
	}
}

// cmd carries Deps into the subcommands.
type cmd struct{ d Deps }

func (c cmd) log() *slog.Logger {
	if c.d.Log != nil {
		return c.d.Log
	}
	return slog.Default()
}

func (c cmd) finish(source string, started time.Time, fetched, newCount int, err error) {
	if c.d.RunCompleted != nil {
		c.d.RunCompleted(source, started, fetched, newCount, err)
	}
}

func (c cmd) open(ctx context.Context) (*Conn, error) {
	conn, err := c.d.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	return conn, nil
}

func (c cmd) llm() llm.Completer {
	if c.d.LLM == nil {
		return nil
	}
	return c.d.LLM()
}

// fetcher identifies as the operator's crawler when the settings name one
// (host.CrawlerSettings); otherwise as host.DefaultCrawler.
func (c cmd) fetcher() *fraudagency.Fetcher {
	var cr host.Crawler
	if cs, ok := c.d.Settings.(host.CrawlerSettings); ok {
		cr = cs.Crawler()
	}
	return fraudagency.NewFetcherFor(cr)
}

func (c cmd) imap() (host.IMAPConfig, bool) {
	if c.d.Settings == nil {
		return host.IMAPConfig{}, false
	}
	return c.d.Settings.IMAP()
}

// Run dispatches one `fraud` subcommand; args exclude the "fraud" word.
func Run(ctx context.Context, d Deps, args []string) error {
	c := cmd{d: d}
	if len(args) < 1 {
		return errors.New(`usage: ingester fraud <subcommand>
  import [flags]       One-off migration of the Fraud/Bribery/Corruption
                       spreadsheet. Reads the normalized JSON produced by
                       corruption-tracker/tools/sheetnormalize/
                       fraud_sheet_normalize.py; clean rows become
                       unverified fraud_cases, problem rows go to the
                       fraud-claims review queue. Idempotent (content-hash
                       keyed) — safe to re-run after a partial failure.
                       Flags: --file PATH (default sources/fraud_sheet_normalized.json)
                              --dry-run`)
	}
	switch args[0] {
	case "import":
		return c.importSheet(ctx, args[1:])
	case "poll":
		return c.poll(ctx, args[1:])
	case "classify-news":
		return c.classifyNews(ctx, args[1:])
	case "news-poll":
		return c.newsPoll(ctx, args[1:])
	case "backup":
		return c.backup(ctx, args[1:])
	case "enrich-imported":
		return c.enrichImported(ctx, args[1:])
	case "extract-pending":
		return c.extractPending(ctx, args[1:])
	case "recode":
		return c.recode(ctx, args[1:])
	case "merge-dupes":
		return c.mergeDupes(ctx, args[1:])
	case "rederive-status":
		return c.rederiveStatus(ctx, args[1:])
	case "export":
		return c.export(ctx, args[1:])
	case "handoff":
		return c.handoff(ctx, args[1:])
	case "verify-bundle":
		return c.verifyBundle(ctx, args[1:])
	case "mail-ingest":
		return c.mailIngest(ctx, args[1:])
	case "snapshot":
		if len(args) < 2 || args[1] != "build" {
			return errors.New(`usage: ingester fraud snapshot build [flags]
  --period P            issue period: YYYY (annual), YYYY-Qn (quarter),
                        YYYY-Www (ISO week), or the scheduler tokens
                        last-quarter / last-year; default: the week that
                        just closed. --week is an alias.
  --note TEXT           revision note — REQUIRED when re-issuing a period
                        (corrections produce a new visible version, never a
                        silent rewrite)
  --include-unverified  dev/preview build including unverified cases; such
                        snapshots are flagged and never served to non-admins

Cron suggestions: Monday early AM NZT covering the prior ISO week; the
first Monday of January covering the prior calendar year.`)
		}
		return c.snapshotBuild(ctx, args[2:])
	default:
		return fmt.Errorf("unknown fraud subcommand %q", args[0])
	}
}
