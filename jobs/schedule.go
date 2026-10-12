// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package jobs holds the tracker's job schedule as data. The standalone
// scheduler runs it in-process; alitheia's launchd rows in
// deploy/launchd/jobs.tsv are checked against it by a drift test. See
// design §9.2.
package jobs

// Job is one scheduled tracker run.
type Job struct {
	// Name is the job's identity, equal to its jobs.tsv name.
	Name string
	// Schedule uses the jobs.tsv grammar:
	// calendar:HH:MM[/dow=N | /dom=N[/months=A,B,..]] or interval:<seconds>.
	// Calendar times are in host.Settings.Timezone().
	Schedule string
	// Lock is "none" or "llm"; llm jobs are serialised against each other.
	Lock string
	// Args are the CLI args after "fraud".
	Args []string
	// Needs lists what must be configured ("llm", "imap"); a job whose
	// needs are absent is skipped, not failed.
	Needs []string
	// Connector names the tracker_connector_state row an operator can
	// disable the job through ("" = none; fraud-poll's agencies are
	// per-source, agency:<slug>, and checked inside the poll).
	Connector string
	// Standalone jobs run only in the standalone scheduler. alitheia's
	// jobs.tsv has no row for them: they exist for what alitheia provides
	// itself (its own news corpus ingestion).
	Standalone bool
}

// Connector names (tracker_connector_state.connector).
const (
	ConnectorClassifyNews = "classify-news"
	ConnectorMail         = "mail"
	ConnectorAgencyPrefix = "agency:"
)

// Lock values.
const (
	LockNone = "none"
	LockLLM  = "llm"
)

// Need values.
const (
	NeedLLM  = "llm"
	NeedIMAP = "imap"
)

// Schedule is the tracker's job schedule. Ordering follows jobs.tsv.
//
//   - poll: agency media-release connectors. After the nightly corpus drains.
//   - news-poll (standalone only): polls the operator's news feeds hourly,
//     so a feed carrying only its latest few items misses none between
//     classify-news runs. alitheia ingests its corpus itself.
//   - classify-news: scans the already-ingested news/commentary corpus
//     (standalone: polls the feeds once more first).
//   - mail-ingest: polls the alerts mailbox; 6-hourly so an appeal decision
//     lands in the queue same-day.
//   - extract-pending: retries extraction over needs_manual_extraction
//     claims, after poll + classify-news have queued the day's items.
//   - recode: machine coding sweep over cases with gaps, after
//     extract-pending so the day's promotions are in.
//   - backup (standalone only): nightly pg_dump + export bundle onto the
//     data volume, with retention. alitheia's database has its own backups.
//   - snapshot: versioned report issues. Weekly Monday (the prior ISO week);
//     quarterly and annual via the last-quarter / last-year tokens.
var Schedule = []Job{
	{Name: "fraud-poll", Schedule: "calendar:07:15", Lock: LockLLM,
		Args: []string{"poll", "--source", "all", "--limit", "25"}, Needs: []string{NeedLLM}},
	{Name: "fraud-news-poll", Schedule: "interval:3600", Lock: LockNone,
		Args: []string{"news-poll"}, Connector: ConnectorClassifyNews, Standalone: true},
	{Name: "fraud-classify-news", Schedule: "calendar:07:30", Lock: LockLLM,
		Args: []string{"classify-news", "--days", "7"}, Needs: []string{NeedLLM}, Connector: ConnectorClassifyNews},
	{Name: "fraud-mail-ingest", Schedule: "interval:21600", Lock: LockLLM,
		Args: []string{"mail-ingest", "--imap"}, Needs: []string{NeedLLM, NeedIMAP}, Connector: ConnectorMail},
	{Name: "fraud-extract-pending", Schedule: "calendar:08:00", Lock: LockLLM,
		Args: []string{"extract-pending", "--limit", "100"}, Needs: []string{NeedLLM}},
	{Name: "fraud-recode", Schedule: "calendar:08:30", Lock: LockLLM,
		Args: []string{"recode", "--limit", "100"}, Needs: []string{NeedLLM}},
	{Name: "fraud-backup", Schedule: "calendar:03:00", Lock: LockNone,
		Args: []string{"backup"}, Standalone: true},
	{Name: "fraud-snapshot-weekly", Schedule: "calendar:06:30/dow=1", Lock: LockNone,
		Args: []string{"snapshot", "build"}},
	{Name: "fraud-snapshot-quarterly", Schedule: "calendar:07:30/dom=1/months=1,4,7,10", Lock: LockNone,
		Args: []string{"snapshot", "build", "--period", "last-quarter"}},
	{Name: "fraud-snapshot-annual", Schedule: "calendar:07:45/dom=2/months=1", Lock: LockNone,
		Args: []string{"snapshot", "build", "--period", "last-year"}},
}
