// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package scheduler runs the tracker's job schedule (jobs.Schedule)
// in-process, for hosts without an external scheduler (the standalone
// server; alitheia keeps launchd). See design §9.2
// and §9.3.
//
// Every 30 s it looks for due jobs. A due job runs in its own goroutine on a
// dedicated connection holding a session advisory lock on the job, so a job
// still running (here or in another process on the same database) is
// recorded as skipped_running rather than started twice. Jobs with
// Lock=llm also take a shared group lock, which serialises them the way
// llm-lock.sh does under launchd. Every attempt, run or skipped, is a
// tracker_job_runs row.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	_ "time/tzdata" // the schedule zone must load in minimal images

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/jobs"
)

// TickInterval is how often Start looks for due jobs.
const TickInterval = 30 * time.Second

// DefaultTimezone is the schedule zone when the host doesn't name one.
const DefaultTimezone = "Pacific/Auckland"

// Run statuses written to tracker_job_runs.status.
const (
	StatusRunning             = "running"
	StatusOK                  = "ok"
	StatusError               = "error"
	StatusSkippedRunning      = "skipped_running"
	StatusSkippedPaused       = "skipped_paused"
	StatusSkippedUnconfigured = "skipped_unconfigured"
	StatusSkippedDisabled     = "skipped_disabled"
)

// Advisory locks use the tracker's class id ("FTKR", shared with the
// migrations package) so they can't collide with a host's own locks in a
// shared database. Object ids are a hash with bit 30 set, which keeps them
// clear of the small fixed ids reserved for schema adoption and freeze.
const advisoryClass = 0x46544b52

func lockID(key string) int32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int32(h.Sum32()&0x3fffffff | 0x40000000)
}

// Counters are a run's stats as the CLI reports them.
type Counters struct {
	Fetched int `json:"fetched"`
	New     int `json:"new"`
}

// RunFunc executes one job's CLI args (jobs.Job.Args) in-process.
type RunFunc func(ctx context.Context, args []string) (Counters, error)

// Config wires a Scheduler.
type Config struct {
	// Pool holds the advisory locks and tracker_job_runs. Each running job
	// uses one connection for its whole run.
	Pool *pgxpool.Pool
	// Jobs defaults to jobs.Schedule.
	Jobs []jobs.Job
	Run  RunFunc
	// Settings answers Needs and the schedule zone; nil means nothing is
	// configured and the zone is DefaultTimezone.
	Settings host.Settings
	// Paused reports the global ingestion pause (§9.3); nil never pauses.
	Paused func(ctx context.Context) (bool, error)
	// ConnectorEnabled answers jobs.Job.Connector; nil means every
	// connector is enabled.
	ConnectorEnabled func(ctx context.Context, connector string) (bool, error)
	// Now defaults to time.Now; tests pass a fake clock.
	Now func() time.Time
	Log *slog.Logger
}

// Scheduler runs Config.Jobs on their schedules.
type Scheduler struct {
	cfg   Config
	specs []spec
	loc   *time.Location
	log   *slog.Logger

	mu          sync.Mutex
	lastTick    time.Time
	lastAttempt map[string]time.Time // interval jobs launched or skipped by this process
	wg          sync.WaitGroup
}

// New validates the schedule and returns a scheduler whose first tick looks
// for calendar instants after now: missed runs from before it started are
// not replayed.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Pool == nil || cfg.Run == nil {
		return nil, errors.New("scheduler: Pool and Run are required")
	}
	if cfg.Jobs == nil {
		cfg.Jobs = jobs.Schedule
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Scheduler{cfg: cfg, log: cfg.Log, lastAttempt: map[string]time.Time{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, j := range cfg.Jobs {
		sp, err := parseSpec(j.Schedule)
		if err != nil {
			return nil, fmt.Errorf("job %s: %w", j.Name, err)
		}
		if j.Lock != jobs.LockNone && j.Lock != jobs.LockLLM {
			return nil, fmt.Errorf("job %s: unknown lock %q", j.Name, j.Lock)
		}
		s.specs = append(s.specs, sp)
	}
	if cfg.Settings != nil {
		s.loc = cfg.Settings.Timezone()
	}
	if s.loc == nil {
		loc, err := time.LoadLocation(DefaultTimezone)
		if err != nil {
			return nil, fmt.Errorf("scheduler: load %s: %w", DefaultTimezone, err)
		}
		s.loc = loc
	}
	s.lastTick = cfg.Now()
	return s, nil
}

// Start ticks every TickInterval until ctx is done, then waits for running
// jobs (which see the cancelled ctx) to finish.
func (s *Scheduler) Start(ctx context.Context) {
	t := time.NewTicker(TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Wait()
			return
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				s.log.Error("scheduler tick failed", "err", err)
			}
		}
	}
}

// Wait blocks until every job started so far has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Tick starts (or records as skipped) every job due at Now(). Due means a
// calendar instant passed since the previous tick, or an interval elapsed
// since the job's last recorded attempt. It returns once due jobs are
// launched; Wait for them to finish.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.mu.Lock()
	now := s.cfg.Now()
	prev := s.lastTick
	s.lastTick = now
	s.mu.Unlock()

	var due []int
	for i, j := range s.cfg.Jobs {
		ok, err := s.isDue(ctx, i, prev, now)
		if err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
		if ok {
			due = append(due, i)
		}
	}
	if len(due) == 0 {
		return nil
	}

	paused := false
	if s.cfg.Paused != nil {
		p, err := s.cfg.Paused(ctx)
		if err != nil {
			// Fail safe: run nothing and record nothing; the next tick retries.
			return fmt.Errorf("read pause state: %w", err)
		}
		paused = p
	}
	for _, i := range due {
		j := s.cfg.Jobs[i]
		s.markAttempt(i, now)
		missing := s.missingNeeds(j)
		enabled, err := s.connectorEnabled(ctx, j)
		if err != nil {
			s.log.Error("read connector state", "job", j.Name, "err", err)
			continue // fail safe: neither run nor record; the next due tick retries
		}
		switch {
		case paused:
			s.recordSkip(ctx, j, now, StatusSkippedPaused, "ingestion paused")
		case !enabled:
			s.recordSkip(ctx, j, now, StatusSkippedDisabled, "connector "+j.Connector+" disabled")
		case len(missing) > 0:
			s.recordSkip(ctx, j, now, StatusSkippedUnconfigured, notConfigured(missing))
		default:
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				_, _ = s.run(ctx, j) // outcome is recorded and logged
			}()
		}
	}
	return nil
}

func (s *Scheduler) isDue(ctx context.Context, i int, prev, now time.Time) (bool, error) {
	sp := s.specs[i]
	if sp.interval == 0 {
		return sp.calendarDue(prev, now, s.loc), nil
	}
	s.mu.Lock()
	last, ok := s.lastAttempt[s.cfg.Jobs[i].Name]
	s.mu.Unlock()
	if !ok {
		// Survive restarts: the last attempt any process recorded.
		var dbLast *time.Time
		if err := s.cfg.Pool.QueryRow(ctx,
			`SELECT max(started_at) FROM tracker_job_runs WHERE job = $1`, s.cfg.Jobs[i].Name).
			Scan(&dbLast); err != nil {
			return false, fmt.Errorf("last run: %w", err)
		}
		if dbLast == nil {
			return true, nil
		}
		last = *dbLast
	}
	return !now.Before(last.Add(sp.interval)), nil
}

func (s *Scheduler) markAttempt(i int, now time.Time) {
	if s.specs[i].interval == 0 {
		return
	}
	s.mu.Lock()
	s.lastAttempt[s.cfg.Jobs[i].Name] = now
	s.mu.Unlock()
}

func (s *Scheduler) connectorEnabled(ctx context.Context, j jobs.Job) (bool, error) {
	if j.Connector == "" || s.cfg.ConnectorEnabled == nil {
		return true, nil
	}
	return s.cfg.ConnectorEnabled(ctx, j.Connector)
}

func (s *Scheduler) missingNeeds(j jobs.Job) []string {
	var missing []string
	for _, n := range j.Needs {
		ok := false
		if st := s.cfg.Settings; st != nil {
			switch n {
			case jobs.NeedLLM:
				ok = st.LLMConfigured()
			case jobs.NeedIMAP:
				_, ok = st.IMAP()
			}
		}
		if !ok {
			missing = append(missing, n)
		}
	}
	slices.Sort(missing)
	return missing
}

// needLabels name a missing need the way the operator fixes it.
var needLabels = map[string]string{
	jobs.NeedLLM:  "LLM not configured",
	jobs.NeedIMAP: "mailbox not configured",
}

// notConfigured is the status-page reason for a skipped job, e.g.
// "LLM not configured (see Settings)".
func notConfigured(missing []string) string {
	parts := make([]string, 0, len(missing))
	for _, n := range missing {
		if l, ok := needLabels[n]; ok {
			parts = append(parts, l)
		} else {
			parts = append(parts, n+" not configured")
		}
	}
	return strings.Join(parts, ", ") + " (see Settings)"
}

func (s *Scheduler) recordSkip(ctx context.Context, j jobs.Job, at time.Time, status, why string) {
	s.log.Info("scheduled job skipped", "job", j.Name, "status", status, "reason", why)
	if _, err := s.cfg.Pool.Exec(ctx, `
		INSERT INTO tracker_job_runs (job, started_at, ended_at, status, error)
		VALUES ($1, $2, $2, $3, $4)`, j.Name, at, status, why); err != nil {
		s.log.Error("record skipped job run", "job", j.Name, "err", err)
	}
}

// run executes one job under its locks and records the outcome. It returns
// the recorded status and, for status error, the job's error; infrastructure
// failures (no connection, lock or bookkeeping errors) return status "" and
// the error.
func (s *Scheduler) run(ctx context.Context, j jobs.Job) (string, error) {
	conn, err := s.cfg.Pool.Acquire(ctx)
	if err != nil {
		s.log.Error("scheduled job: acquire connection", "job", j.Name, "err", err)
		return "", err
	}
	defer conn.Release()
	// Unlock on a fresh context: ctx may be cancelled by now, and a session
	// lock left on a pooled connection would block the job forever.
	unlock := func(key string) {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`,
			int32(advisoryClass), lockID(key)); err != nil {
			s.log.Error("scheduled job: advisory unlock", "job", j.Name, "lock", key, "err", err)
		}
	}

	jobKey := "job:" + j.Name
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
		int32(advisoryClass), lockID(jobKey)).Scan(&got); err != nil {
		s.log.Error("scheduled job: advisory lock", "job", j.Name, "err", err)
		return "", err
	}
	if !got {
		s.recordSkip(ctx, j, s.cfg.Now(), StatusSkippedRunning, "previous run still in progress")
		return StatusSkippedRunning, nil
	}
	defer unlock(jobKey)

	if j.Lock == jobs.LockLLM {
		groupKey := "group:" + jobs.LockLLM
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`,
			int32(advisoryClass), lockID(groupKey)); err != nil {
			s.log.Error("scheduled job: llm group lock", "job", j.Name, "err", err)
			return "", err
		}
		defer unlock(groupKey)
	}

	var runID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO tracker_job_runs (job, started_at, status) VALUES ($1, $2, $3) RETURNING id`,
		j.Name, s.cfg.Now(), StatusRunning).Scan(&runID); err != nil {
		s.log.Error("record job start", "job", j.Name, "err", err)
		return "", err
	}
	s.log.Info("scheduled job started", "job", j.Name, "run", runID)

	counters, runErr := s.safeRun(ctx, j)
	status, errText := StatusOK, ""
	if runErr != nil {
		status, errText = StatusError, runErr.Error()
	}
	cj, _ := json.Marshal(counters)
	// Record the outcome even if ctx was cancelled mid-run.
	if _, err := conn.Exec(context.Background(), `
		UPDATE tracker_job_runs SET ended_at = $2, status = $3, error = $4, counters = $5 WHERE id = $1`,
		runID, s.cfg.Now(), status, errText, cj); err != nil {
		s.log.Error("record job end", "job", j.Name, "run", runID, "err", err)
	}
	s.log.Info("scheduled job finished", "job", j.Name, "run", runID, "status", status,
		"fetched", counters.Fetched, "new", counters.New, "err", errText)
	return status, runErr
}

// RunNow runs the named job immediately, under the same locks and with the
// same tracker_job_runs record as a scheduled run (`job run <name>`). Like
// any manual run it ignores the ingestion pause (§9.3), but a disabled
// connector or missing configuration still refuse it.
func (s *Scheduler) RunNow(ctx context.Context, name string) (string, error) {
	for _, j := range s.cfg.Jobs {
		if j.Name != name {
			continue
		}
		enabled, err := s.connectorEnabled(ctx, j)
		if err != nil {
			return "", err
		}
		if !enabled {
			return "", fmt.Errorf("job %s: connector %s is disabled", name, j.Connector)
		}
		if missing := s.missingNeeds(j); len(missing) > 0 {
			return "", fmt.Errorf("job %s: not configured: %v", name, missing)
		}
		return s.run(ctx, j)
	}
	return "", fmt.Errorf("unknown job %q", name)
}

// safeRun turns a panicking job into an error so it can't take the server
// down or leave its run row 'running'.
func (s *Scheduler) safeRun(ctx context.Context, j jobs.Job) (c Counters, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.cfg.Run(ctx, j.Args)
}
