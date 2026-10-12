// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/fraudrecode"
	"github.com/lonelycode/corruption-tracker/fraudreport"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/ingest/fraudsheet"
	"github.com/lonelycode/corruption-tracker/jobs"
)

// poll polls agency media-release feeds into the fraud-claims review queue.
// Idempotent (URL-keyed); suitable for a daily cron.
func (c cmd) poll(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud poll", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud poll", flag.ExitOnError)
	source := fs.String("source", "all", "sfo | ird | all")
	limit := fs.Int("limit", 25, "max new items per source per run")
	dryRun := fs.Bool("dry-run", false, "fetch + extract, write nothing")
	noLLM := fs.Bool("no-llm", false, "skip extraction; queue items for manual extraction")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var sources []fraudagency.Source
	if *source == "all" {
		sources = fraudagency.Sources
	} else {
		s, ok := fraudagency.SourceBySlug(*source)
		if !ok {
			return fmt.Errorf("unknown fraud poll source %q", *source)
		}
		sources = []fraudagency.Source{s}
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud poll"); err != nil || frozen {
			return err
		}
	}

	log := c.log()
	deps := &fraudagency.Deps{DB: conn.Store, Fetcher: c.fetcher(), Log: log, Limit: *limit, DryRun: *dryRun}
	if !*noLLM {
		deps.LLM = c.llm()
	}

	fetched, newCount, err = pollSources(ctx, conn, deps, sources, *dryRun, log)
	return err
}

// pollSources polls each enabled source in turn. One source failing (its
// site down, robots.txt now refusing us) doesn't stop the others: every
// source is attempted, the run records each source's attempt on its
// connector, and the failures come back together at the end. Only the
// database or a cancelled run stops it early.
func pollSources(ctx context.Context, conn *Conn, deps *fraudagency.Deps, sources []fraudagency.Source,
	dryRun bool, log *slog.Logger) (fetched, newCount int, err error) {
	var failed []string
	attempted := 0
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return fetched, newCount, err
		}
		connector := jobs.ConnectorAgencyPrefix + src.Slug
		enabled, err := conn.Store.ConnectorEnabled(ctx, connector)
		if err != nil {
			return fetched, newCount, err
		}
		if !enabled {
			log.Info("fraud poll source skipped: connector disabled", "source", src.Slug)
			continue
		}
		cu, err := conn.Store.GetConnectorCatchUp(ctx, connector)
		if err != nil {
			return fetched, newCount, err
		}
		deps.CatchUp = cu.CatchUp // after a handoff import: read past --limit until known items
		if cu.CatchUp {
			log.Info("fraud poll source catching up after handoff", "source", src.Slug)
		}
		attempted++
		st, perr := deps.Poll(ctx, src)
		if !dryRun {
			// A failed attempt is recorded too (last_run_at without
			// last_success_at); a catch-up stays on until one succeeds.
			if err := conn.Store.ConnectorRan(ctx, connector, perr == nil); err != nil {
				return fetched, newCount, err
			}
		}
		fetched += st.Fetched
		newCount += st.ClaimsNew
		log.Info("fraud poll source done", "source", src.Slug,
			"index_items", st.IndexItems, "skipped_known", st.Skipped,
			"fetched", st.Fetched, "vocab_rejected", st.VocabRejected,
			"not_fraud_case", st.NotFraudCase, "claims_new", st.ClaimsNew,
			"claims_deduped", st.ClaimsDeduped, "manual_queued", st.ManualQueued,
			"quarantined", st.Quarantined, "match_suggestions", st.Suggestions,
			"errors", st.Errors, "dry_run", dryRun)
		if perr != nil {
			if ctx.Err() != nil {
				return fetched, newCount, ctx.Err()
			}
			log.Error("fraud poll source failed; continuing with the others", "source", src.Slug, "err", perr)
			failed = append(failed, fmt.Sprintf("%s: %v", src.Slug, perr))
		}
	}
	if len(failed) > 0 {
		return fetched, newCount, fmt.Errorf("poll: %d of %d sources failed: %s", len(failed), attempted, strings.Join(failed, "; "))
	}
	return fetched, newCount, nil
}

// classifyNews scans the host's news/commentary corpus for fraud-enforcement
// reporting and queues claims. Read-only over the corpus; idempotent via
// URL-keyed dedupe + auto-rejected negatives.
func (c cmd) classifyNews(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud classify-news", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud classify-news", flag.ExitOnError)
	corpus := fs.String("corpus", "all", "news | commentary | all")
	days := fs.Int("days", 7, "scan items published in the last N days (0 = no bound)")
	limit := fs.Int("limit", 2000, "max corpus items scanned per corpus")
	dryRun := fs.Bool("dry-run", false, "extract, write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud classify-news"); err != nil || frozen {
			return err
		}
	}

	if enabled, err := conn.Store.ConnectorEnabled(ctx, jobs.ConnectorClassifyNews); err != nil {
		return err
	} else if !enabled {
		c.log().Info("fraud classify-news skipped: connector disabled")
		return nil
	}

	var since time.Time
	if *days > 0 {
		since = time.Now().AddDate(0, 0, -*days)
	}
	cu, err := conn.Store.GetConnectorCatchUp(ctx, jobs.ConnectorClassifyNews)
	if err != nil {
		return err
	}
	if cu.CatchUp && cu.ResumeFrom != nil && !since.IsZero() && cu.ResumeFrom.Before(since) {
		since = *cu.ResumeFrom // after a handoff import: reach back to before the cutoff
		c.log().Info("fraud classify-news catching up after handoff", "since", since)
	}
	if conn.News == nil {
		return errors.New("classify-news: no news corpus configured")
	}
	log := c.log()
	// A feed failure doesn't stop the run: what is stored still gets
	// classified, and the run then reports the failure.
	var refreshErr error
	if r, ok := conn.News.(NewsRefresher); ok {
		if *dryRun {
			log.Info("dry-run: news feeds not refreshed")
		} else {
			n, err := r.RefreshNews(ctx)
			if err != nil {
				log.Warn("news feed refresh had failures", "err", err)
				refreshErr = fmt.Errorf("news feeds: %w", err)
			}
			log.Info("news feeds refreshed", "new_items", n)
		}
	}
	items, err := conn.News.ListNewsItems(ctx, *corpus, since, *limit)
	if err != nil {
		return err
	}

	deps := &fraudagency.Deps{DB: conn.Store, Fetcher: c.fetcher(), LLM: c.llm(), Log: log, DryRun: *dryRun}
	st, err := deps.ClassifyCorpus(ctx, items)
	fetched = st.Scanned
	newCount = st.ClaimsNew
	log.Info("fraud classify-news done",
		"scanned", st.Scanned, "vocab_hits", st.VocabHits, "skipped_known", st.SkippedKnown,
		"not_fraud_case", st.NotFraudCase, "claims_new", st.ClaimsNew,
		"quarantined", st.Quarantined, "match_suggestions", st.Suggestions,
		"errors", st.Errors, "dry_run", *dryRun)
	if err != nil {
		return err
	}
	if cu.CatchUp && !*dryRun {
		if err := conn.Store.ConnectorRan(ctx, jobs.ConnectorClassifyNews, true); err != nil {
			return err
		}
	}
	return refreshErr
}

// newsPoll refreshes a feed-based news corpus without classifying (the
// standalone scheduler's hourly fraud-news-poll), so a feed that only
// carries its latest few items is read often enough to miss none. It is
// governed by the classify-news connector: switching that off stops both.
func (c cmd) newsPoll(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var newCount int
	defer func() { c.finish("fraud news-poll", started, newCount, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud news-poll", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if frozen, err := c.frozen(ctx, conn, "fraud news poll"); err != nil || frozen {
		return err
	}

	if enabled, err := conn.Store.ConnectorEnabled(ctx, jobs.ConnectorClassifyNews); err != nil {
		return err
	} else if !enabled {
		c.log().Info("fraud news-poll skipped: classify-news connector disabled")
		return nil
	}
	r, ok := conn.News.(NewsRefresher)
	if !ok {
		return errors.New("news-poll: this host's news corpus is not fed by polling feeds")
	}
	newCount, err = r.RefreshNews(ctx)
	c.log().Info("fraud news-poll done", "new_items", newCount)
	return err
}

// backup takes a backup (the standalone scheduler's nightly fraud-backup,
// or `job run fraud-backup`). Retention runs after every backup.
func (c cmd) backup(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	n := 0
	defer func() { c.finish("fraud backup", started, n, n, rerr) }()

	fs := flag.NewFlagSet("fraud backup", flag.ExitOnError)
	kind := fs.String("kind", "nightly", "nightly | manual")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, err := c.open(ctx) // the database must answer before pg_dump runs
	if err != nil {
		return err
	}
	defer conn.close()
	if c.d.Backup == nil {
		return errors.New("backup: not available here (the host backs up its own database)")
	}
	name, err := c.d.Backup(ctx, *kind)
	if err != nil {
		return err
	}
	n = 1
	c.log().Info("fraud backup done", "backup", name)
	return nil
}

// enrichImported re-fetches the spreadsheet-linked source articles and
// queues extraction claims pre-attached to their cases, so reviewers can
// promote richer events + offending periods onto the imported
// back-catalogue. One-off backfill; idempotent (URL-keyed).
func (c cmd) enrichImported(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud enrich-imported", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud enrich-imported", flag.ExitOnError)
	limit := fs.Int("limit", 250, "max linked sources to process")
	dryRun := fs.Bool("dry-run", false, "fetch + extract, write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud enrich-imported"); err != nil || frozen {
			return err
		}
	}

	targets, err := conn.Store.ListFraudEnrichTargets(ctx, *limit)
	if err != nil {
		return err
	}

	log := c.log()
	deps := &fraudagency.Deps{DB: conn.Store, Fetcher: c.fetcher(), LLM: c.llm(), Log: log, DryRun: *dryRun}
	st, err := deps.EnrichImported(ctx, targets)
	fetched = st.Targets
	newCount = st.ClaimsNew
	log.Info("fraud enrich-imported done",
		"targets", st.Targets, "skipped_known", st.SkippedKnown,
		"robots_blocked", st.RobotsBlocked, "fetch_errors", st.FetchErrors,
		"not_fraud_case", st.NotFraudCase, "claims_new", st.ClaimsNew,
		"errors", st.Errors, "dry_run", *dryRun)
	return err
}

// extractPending retries extraction over pending manual-extraction claims
// (LLM-outage casualties, sheet problem rows, terse alerts): refetch the
// source URL where present, extract, and write the result onto the claim in
// place so bulk approve can process it. Review state untouched. Idempotent;
// suitable for a daily cron after poll/classify-news.
func (c cmd) extractPending(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud extract-pending", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud extract-pending", flag.ExitOnError)
	limit := fs.Int("limit", 100, "max manual-extraction claims to process")
	dryRun := fs.Bool("dry-run", false, "fetch + extract, write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud extract-pending"); err != nil || frozen {
			return err
		}
	}

	log := c.log()
	deps := &fraudagency.Deps{DB: conn.Store, Fetcher: c.fetcher(), LLM: c.llm(), Log: log, DryRun: *dryRun}
	st, err := deps.ExtractPending(ctx, *limit)
	fetched = st.Claims
	newCount = st.Extracted
	log.Info("fraud extract-pending done",
		"claims", st.Claims, "extracted", st.Extracted, "not_fraud_case", st.NotFraudCase,
		"too_thin", st.TooThin, "fetch_errors", st.FetchErrors, "robots_blocked", st.RobotsBlocked,
		"quarantined", st.Quarantined, "match_suggestions", st.Suggestions,
		"errors", st.Errors, "dry_run", *dryRun)
	return err
}

// recode is the machine coding-backfill: closed-set LLM classification of
// uncoded typology/agency/region, offending-period and event-date recovery,
// typology→agency priors, and alias self-heal. Fills NULL columns only —
// human coding always wins. Idempotent via recoded_at; suitable for a daily
// cron after poll/classify-news so newly promoted cases never stay uncoded.
func (c cmd) recode(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud recode", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud recode", flag.ExitOnError)
	limit := fs.Int("limit", 300, "max cases to attempt")
	dryRun := fs.Bool("dry-run", false, "classify, write nothing")
	noFetch := fs.Bool("no-fetch", false, "skip source-article fetches (stored fields only)")
	minConfidence := fs.Float64("min-confidence", 0.75, "typology fill threshold; below it the suggestion is only logged")
	retry := fs.Bool("retry", false, "re-attempt cases already stamped recoded_at")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud recode"); err != nil || frozen {
			return err
		}
	}

	deps := &fraudrecode.Deps{
		DB: conn.Store, LLM: c.llm(), Log: c.log(), Fetcher: c.fetcher(),
		DryRun: *dryRun, NoFetch: *noFetch, MinConfidence: *minConfidence,
	}
	st, err := deps.Run(ctx, *limit, *retry)
	fetched = st.Targets
	newCount = st.TypologyCoded + st.AgencyStated + st.AgencyInferred + st.RegionCoded + st.SectorCoded + st.PeriodsFilled + st.EventDatesFilled
	c.log().Info("fraud recode done",
		"targets", st.Targets, "llm_calls", st.LLMCalls,
		"typology_coded", st.TypologyCoded, "typology_low_confidence", st.TypologyLowConf,
		"agency_stated", st.AgencyStated, "agency_inferred", st.AgencyInferred,
		"region_coded", st.RegionCoded, "sector_coded", st.SectorCoded, "periods_filled", st.PeriodsFilled,
		"event_dates_filled", st.EventDatesFilled, "charges_recovered", st.ChargesRecovered,
		"aliases_added", st.AliasesAdded,
		"fetch_errors", st.FetchErrors, "robots_blocked", st.RobotsBlocked,
		"errors", st.Errors, "dry_run", *dryRun)
	return err
}

// mergeDupes finds near-certain duplicate case records — the same matter
// ingested from multiple outlets or agency feeds — and folds each group down
// to one winner. Dry-run by default: it prints the groups it would merge and
// changes nothing until --apply. Detection is high-precision (identical
// participant set + shared amount or shared source URL); everything else is
// left for a human. Idempotent — re-running after --apply finds nothing.
func (c cmd) mergeDupes(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud merge-dupes", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud merge-dupes", flag.ExitOnError)
	apply := fs.Bool("apply", false, "perform the merges (default: dry-run, report only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if *apply {
		if frozen, err := c.frozen(ctx, conn, "fraud merge-dupes"); err != nil || frozen {
			return err
		}
	}

	log := c.log()
	groups, err := conn.Store.FindDuplicateFraudCaseGroups(ctx)
	if err != nil {
		return err
	}
	fetched = len(groups)

	for _, g := range groups {
		names := make([]string, 0, len(g.Cases))
		for _, fc := range g.Cases {
			names = append(names, fmt.Sprintf("#%d(%s)", fc.ID, fc.ReviewState))
		}
		if !*apply {
			log.Info("dry-run: would merge duplicate group",
				"winner", g.WinnerID, "losers", g.LoserIDs, "reason", g.Reason, "cases", names)
			continue
		}
		res, err := conn.Store.MergeFraudCases(ctx, g.WinnerID, g.LoserIDs)
		if err != nil {
			log.Error("merge group failed", "winner", g.WinnerID, "losers", g.LoserIDs, "err", err)
			continue
		}
		newCount += res.LosersMerged
		log.Info("merged duplicate group",
			"winner", g.WinnerID, "losers_merged", res.LosersMerged, "reason", g.Reason,
			"participants", res.Participants, "sources", res.Sources, "events", res.Events,
			"claims", res.Claims, "collisions_discarded", res.CollisionsDiscarded)
	}
	log.Info("fraud merge-dupes done", "groups", len(groups), "applied", *apply, "cases_merged", newCount)
	return nil
}

// rederiveStatus re-runs case-status derivation over every case and reports
// the ones whose stored status is stale (stored status is otherwise only
// recomputed on an event write — e.g. after the appeal-ordering derivation
// fix). Dry-run by default: it logs each case's old→new status and changes
// nothing until --apply. Manually overridden statuses are never touched;
// every applied flip writes a status revision. Idempotent.
func (c cmd) rederiveStatus(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud rederive-status", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud rederive-status", flag.ExitOnError)
	apply := fs.Bool("apply", false, "write the re-derived statuses (default: dry-run, report only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if *apply {
		if frozen, err := c.frozen(ctx, conn, "fraud rederive-status"); err != nil || frozen {
			return err
		}
	}

	log := c.log()
	st, err := conn.Store.RederiveFraudCaseStatuses(ctx, *apply, "status re-derived from events by `fraud rederive-status`")
	fetched = st.Checked
	newCount = st.Applied
	for _, ch := range st.Changed {
		msg := "dry-run: would re-derive case status"
		if *apply {
			msg = "re-derived case status"
		}
		log.Info(msg, "case", ch.CaseID, "from", string(ch.From), "to", string(ch.To))
	}
	log.Info("fraud rederive-status done", "checked", st.Checked, "overridden_skipped", st.Overridden,
		"changed", len(st.Changed), "applied", st.Applied, "dry_run", !*apply)
	return err
}

// mailIngest processes alert emails (Courts of NZ notifications, Capital
// Letter digests) exported as .eml files, or polled from the configured IMAP
// mailbox, into the review queue. Licensed mail to our own inbox — compliant
// where scraping isn't.
func (c cmd) mailIngest(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud mail-ingest", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud mail-ingest", flag.ExitOnError)
	dir := fs.String("dir", "", "directory of .eml files to ingest")
	archive := fs.String("archive", "", "move processed files here (with --dir)")
	useIMAP := fs.Bool("imap", false, "poll the configured IMAP mailbox (FRAUD_MAIL_IMAP_* env) instead of a directory")
	dryRun := fs.Bool("dry-run", false, "parse + extract, write nothing")
	noLLM := fs.Bool("no-llm", false, "skip extraction; matched alerts queue for manual review")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" && !*useIMAP {
		return errors.New("usage: ingester fraud mail-ingest (--dir PATH [--archive PATH] | --imap) [--dry-run]")
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud mail-ingest"); err != nil || frozen {
			return err
		}
	}

	log := c.log()
	mailbox, haveMailbox := c.imap()
	if *useIMAP && !haveMailbox {
		// Graceful skip: lets the job ship in the schedule before every
		// environment has mailbox credentials.
		log.Info("fraud mail-ingest skipped: FRAUD_MAIL_IMAP_ADDR not configured")
		return nil
	}
	if enabled, err := conn.Store.ConnectorEnabled(ctx, jobs.ConnectorMail); err != nil {
		return err
	} else if !enabled {
		log.Info("fraud mail-ingest skipped: connector disabled")
		return nil
	}
	deps := &fraudagency.Deps{DB: conn.Store, Fetcher: c.fetcher(), Log: log, DryRun: *dryRun}
	if !*noLLM {
		deps.LLM = c.llm()
	}
	var st fraudagency.MailStats
	if *useIMAP {
		st, err = deps.IngestIMAP(ctx, mailbox)
	} else {
		st, err = deps.IngestMailDir(ctx, *dir, *archive)
	}
	fetched = st.Files
	newCount = st.ClaimsNew
	log.Info("fraud mail-ingest done",
		"files", st.Files, "parse_errors", st.ParseErrors, "duplicates", st.Duplicates,
		"irrelevant", st.Irrelevant, "not_fraud_case", st.NotFraudCase,
		"claims_new", st.ClaimsNew, "manual_queued", st.ManualQueued,
		"quarantined", st.Quarantined, "match_suggestions", st.Suggestions,
		"errors", st.Errors, "dry_run", *dryRun)
	return err
}

func (c cmd) snapshotBuild(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud snapshot build", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud snapshot build", flag.ExitOnError)
	period := fs.String("period", "", "period: YYYY (annual), YYYY-Qn (quarter) or YYYY-Www (week); default last week")
	week := fs.String("week", "", "alias for --period with an ISO week")
	note := fs.String("note", "", "revision note (required when re-issuing a period)")
	includeUnverified := fs.Bool("include-unverified", false, "dev build including unverified cases")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *period == "" {
		*period = *week
	}
	*period = resolvePeriod(*period, time.Now().UTC())

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if frozen, err := c.frozen(ctx, conn, "fraud snapshot build"); err != nil || frozen {
		return err
	}

	b := &fraudreport.Builder{DB: conn.Store, Log: c.log(), IncludeUnverified: *includeUnverified}
	payload, version, err := b.Build(ctx, *period, *note)
	if err != nil {
		return err
	}
	fetched, newCount = 1, 1
	c.log().Info("fraud snapshot built",
		"period", *period, "version", version,
		"charges_laid", len(payload.NewInPeriod.ChargesLaid),
		"convictions", len(payload.NewInPeriod.Convictions),
		"appeal_outcomes", len(payload.NewInPeriod.AppealOutcomes),
		"trend_cells", len(payload.TypologyTrend),
		"suppressed_cells", payload.SuppressedCells,
		"verified_only", payload.VerifiedOnly)
	return nil
}

// resolvePeriod expands the scheduler-friendly tokens (a schedule can't
// compute dates): "" is the ISO week that just closed, "last-quarter" and
// "last-year" the previous calendar quarter / year. Anything else passes
// through unchanged.
func resolvePeriod(period string, now time.Time) string {
	switch period {
	case "":
		return fraudreport.ISOWeekOf(now.AddDate(0, 0, -7))
	case "last-quarter":
		q := (int(now.Month())-1)/3 + 1 // current quarter 1..4
		y := now.Year()
		if q == 1 {
			q, y = 4, y-1
		} else {
			q--
		}
		return fmt.Sprintf("%d-Q%d", y, q)
	case "last-year":
		return fmt.Sprintf("%d", now.Year()-1)
	}
	return period
}

func (c cmd) importSheet(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud import", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud import", flag.ExitOnError)
	file := fs.String("file", "sources/fraud_sheet_normalized.json", "normalized spreadsheet JSON")
	dryRun := fs.Bool("dry-run", false, "report what would be imported without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read %s (run corruption-tracker/tools/sheetnormalize/fraud_sheet_normalize.py first): %w", *file, err)
	}
	var rows []fraudsheet.Row
	if err := json.Unmarshal(raw, &rows); err != nil {
		return fmt.Errorf("parse %s: %w", *file, err)
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if !*dryRun {
		if frozen, err := c.frozen(ctx, conn, "fraud import"); err != nil || frozen {
			return err
		}
	}

	log := c.log()
	deps := &fraudsheet.Deps{DB: conn.Store, Log: log, DryRun: *dryRun}
	stats, err := deps.Import(ctx, rows)
	fetched = stats.Rows
	newCount = stats.CasesCreated + stats.ClaimsCreated
	if err != nil {
		return err
	}

	log.Info("fraud sheet import complete",
		"rows", stats.Rows, "clean", stats.CleanRows, "problem", stats.ProblemRows,
		"cases_created", stats.CasesCreated, "rows_merged", stats.RowsMerged,
		"claims_created", stats.ClaimsCreated, "claims_deduped", stats.ClaimsDeduped,
		"skipped_existing", stats.SkippedExisting, "dry_run", *dryRun)
	for _, a := range stats.AmbiguousGroups {
		log.Warn("ambiguous cluster (not auto-merged)", "detail", a)
	}
	return nil
}
