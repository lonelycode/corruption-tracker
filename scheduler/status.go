// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lonelycode/corruption-tracker/jobs"
)

// Run is one tracker_job_runs row.
type Run struct {
	ID        int64
	Status    string
	StartedAt time.Time
	EndedAt   *time.Time
	Error     string
	Counters  Counters
}

// JobStatus is what the ingestion status page shows for one job.
type JobStatus struct {
	Job         jobs.Job
	LastRun     *Run // most recent attempt, run or skipped
	LastSuccess *Run // most recent ok run
	LastFailure *Run // most recent error run
	Running     bool
	// NextRun is when the schedule next makes the job due (nil if the
	// schedule never fires again within a year).
	NextRun *time.Time
	// New7d sums the runs' "new" counters (claims created, cases coded, …)
	// over the last 7 days.
	New7d int
}

// Status reports every scheduled job's recent history and next due time.
func (s *Scheduler) Status(ctx context.Context) ([]JobStatus, error) {
	now := s.cfg.Now()
	out := make([]JobStatus, 0, len(s.cfg.Jobs))
	for i, j := range s.cfg.Jobs {
		st := JobStatus{Job: j}
		var err error
		if st.LastRun, err = s.lastRun(ctx, j.Name, ""); err != nil {
			return nil, err
		}
		if st.LastSuccess, err = s.lastRun(ctx, j.Name, StatusOK); err != nil {
			return nil, err
		}
		if st.LastFailure, err = s.lastRun(ctx, j.Name, StatusError); err != nil {
			return nil, err
		}
		if err := s.cfg.Pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM tracker_job_runs WHERE job = $1 AND status = 'running'),
			       COALESCE(sum((counters->>'new')::int), 0)
			  FROM tracker_job_runs WHERE job = $1 AND started_at > $2`,
			j.Name, now.Add(-7*24*time.Hour)).Scan(&st.Running, &st.New7d); err != nil {
			return nil, fmt.Errorf("job %s status: %w", j.Name, err)
		}
		if next, ok := s.nextRun(i, st.LastRun, now); ok {
			st.NextRun = &next
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Scheduler) lastRun(ctx context.Context, job, status string) (*Run, error) {
	var r Run
	err := s.cfg.Pool.QueryRow(ctx, `
		SELECT id, status, started_at, ended_at, error,
		       COALESCE((counters->>'fetched')::int, 0), COALESCE((counters->>'new')::int, 0)
		  FROM tracker_job_runs
		 WHERE job = $1 AND ($2 = '' OR status = $2)
		 ORDER BY started_at DESC, id DESC LIMIT 1`, job, status).
		Scan(&r.ID, &r.Status, &r.StartedAt, &r.EndedAt, &r.Error, &r.Counters.Fetched, &r.Counters.New)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("last run of %s: %w", job, err)
	}
	return &r, nil
}

// nextRun is when job i next becomes due after now.
func (s *Scheduler) nextRun(i int, last *Run, now time.Time) (time.Time, bool) {
	sp := s.specs[i]
	if sp.interval > 0 {
		s.mu.Lock()
		at, ok := s.lastAttempt[s.cfg.Jobs[i].Name]
		s.mu.Unlock()
		if !ok && last != nil {
			at, ok = last.StartedAt, true
		}
		if !ok {
			return now, true // due on the next tick
		}
		if next := at.Add(sp.interval); next.After(now) {
			return next, true
		}
		return now, true
	}
	return sp.nextCalendar(now, s.loc)
}

// nextCalendar is the first calendar instant strictly after t, looking at
// most a year (and a day) ahead.
func (sp spec) nextCalendar(t time.Time, loc *time.Location) (time.Time, bool) {
	end := t.Add(367 * 24 * time.Hour)
	for m := t.Truncate(time.Minute).Add(time.Minute); m.Before(end); m = m.Add(time.Minute) {
		if sp.firesAt(m.In(loc)) {
			return m, true
		}
	}
	return time.Time{}, false
}
