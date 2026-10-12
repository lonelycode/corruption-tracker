// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package scheduler_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/scheduler"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// testPool is a fresh, fully migrated private schema on the test database
// (storetest). Advisory locks are database-wide, but every lock key here
// names a schedtest-* job or the llm group, which only this package takes.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return storetest.Open(t).Pool
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func nzTime(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(scheduler.DefaultTimezone)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

type runRow struct {
	Status, Error string
	Fetched, New  int
}

func runsOf(t *testing.T, pool *pgxpool.Pool, job string) []runRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT status, error, COALESCE((counters->>'fetched')::int, 0), COALESCE((counters->>'new')::int, 0)
		  FROM tracker_job_runs WHERE job = $1 ORDER BY id`, job)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.Status, &r.Error, &r.Fetched, &r.New); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func statuses(rs []runRow) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Status
	}
	return out
}

// eventually polls cond (for rows written by job goroutines still running).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type settings struct{ llm, imap bool }

func (s settings) IMAP() (host.IMAPConfig, bool) { return host.IMAPConfig{Addr: "x:993"}, s.imap }
func (s settings) LLMConfigured() bool           { return s.llm }
func (settings) Timezone() *time.Location        { return nil } // default zone

func newScheduler(t *testing.T, pool *pgxpool.Pool, clock *fakeClock, js []jobs.Job, run scheduler.RunFunc, mod func(*scheduler.Config)) *scheduler.Scheduler {
	t.Helper()
	cfg := scheduler.Config{Pool: pool, Jobs: js, Run: run, Now: clock.Now, Settings: settings{llm: true, imap: true}}
	if mod != nil {
		mod(&cfg)
	}
	s, err := scheduler.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tick(t *testing.T, s *scheduler.Scheduler) {
	t.Helper()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Wait()
}

func TestFiresAtConfiguredTimes(t *testing.T) {
	pool := testPool(t)
	clock := &fakeClock{t: nzTime(t, "2026-10-12 07:14:00")}
	var mu sync.Mutex
	var calls []string
	run := func(_ context.Context, args []string) (scheduler.Counters, error) {
		mu.Lock()
		calls = append(calls, args[0]+"@"+clock.Now().Format("01-02 15:04:05"))
		mu.Unlock()
		return scheduler.Counters{Fetched: 3, New: 1}, nil
	}
	s := newScheduler(t, pool, clock, []jobs.Job{
		{Name: "schedtest-cal", Schedule: "calendar:07:15", Lock: jobs.LockNone, Args: []string{"cal"}},
		{Name: "schedtest-weekly", Schedule: "calendar:06:30/dow=1", Lock: jobs.LockNone, Args: []string{"weekly"}},
		{Name: "schedtest-int", Schedule: "interval:3600", Lock: jobs.LockNone, Args: []string{"int"}},
	}, run, nil)

	for _, at := range []string{
		"2026-10-12 07:14:30", // int: never run -> fires
		"2026-10-12 07:15:00", // cal fires
		"2026-10-12 07:15:30", // nothing
		"2026-10-12 08:14:29", // int: 1s short of an hour after 07:14:30
		"2026-10-12 08:14:30", // int fires
		"2026-10-13 06:30:10", // weekly: Tuesday -> no; int fires (overdue)
		"2026-10-13 07:15:20", // cal fires
		"2026-10-19 06:30:20", // Monday: weekly fires, plus int, plus cal caught up once
	} {
		clock.set(nzTime(t, at))
		tick(t, s)
	}
	want := []string{
		"int@10-12 07:14:30",
		"cal@10-12 07:15:00",
		"int@10-12 08:14:30",
		"int@10-13 06:30:10",
		"cal@10-13 07:15:20",
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != len(want)+3 {
		t.Fatalf("calls = %v", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call %d = %s, want %s", i, got[i], w)
		}
	}
	last := strings.Join(got[len(want):], " ")
	for _, w := range []string{"weekly@10-19 06:30:20", "int@10-19 06:30:20", "cal@10-19 06:30:20"} {
		if !strings.Contains(last, w) {
			t.Errorf("last tick calls %q missing %s", last, w)
		}
	}

	rs := runsOf(t, pool, "schedtest-cal")
	if len(rs) != 3 || rs[0] != (runRow{Status: "ok", Fetched: 3, New: 1}) {
		t.Errorf("cal runs = %+v", rs)
	}

	// A restarted scheduler picks the interval up from tracker_job_runs
	// instead of firing immediately.
	s2 := newScheduler(t, pool, clock, []jobs.Job{
		{Name: "schedtest-int", Schedule: "interval:3600", Lock: jobs.LockNone, Args: []string{"int"}},
	}, run, nil)
	clock.add(30 * time.Minute)
	tick(t, s2)
	if n := len(runsOf(t, pool, "schedtest-int")); n != 4 {
		t.Errorf("int runs after restart = %d, want 4 (no immediate re-run)", n)
	}
	clock.add(30 * time.Minute)
	tick(t, s2)
	if n := len(runsOf(t, pool, "schedtest-int")); n != 5 {
		t.Errorf("int runs an interval after the last = %d, want 5", n)
	}
}

func TestOverlappingRunIsSkippedNotDoubled(t *testing.T) {
	pool := testPool(t)
	clock := &fakeClock{t: nzTime(t, "2026-10-12 09:00:00")}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	run := func(context.Context, []string) (scheduler.Counters, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return scheduler.Counters{}, nil
	}
	const job = "schedtest-slow"
	s := newScheduler(t, pool, clock, []jobs.Job{
		{Name: job, Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"slow"}},
	}, run, nil)

	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started

	// Next interval while the first run is still going.
	clock.add(time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "skipped_running row", func() bool {
		for _, r := range runsOf(t, pool, job) {
			if r.Status == scheduler.StatusSkippedRunning {
				return true
			}
		}
		return false
	})

	close(release)
	s.Wait()
	if got := statuses(runsOf(t, pool, job)); strings.Join(got, ",") != "ok,skipped_running" {
		t.Fatalf("runs = %v, want ok then skipped_running", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("job ran %d times, want 1", calls.Load())
	}
}

func TestLockHeldByAnotherProcessSkips(t *testing.T) {
	pool := testPool(t)
	clock := &fakeClock{t: nzTime(t, "2026-10-12 09:00:00")}
	const job = "schedtest-elsewhere"
	var calls atomic.Int32
	s := newScheduler(t, pool, clock, []jobs.Job{
		{Name: job, Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"x"}},
	}, func(context.Context, []string) (scheduler.Counters, error) {
		calls.Add(1)
		return scheduler.Counters{}, nil
	}, nil)

	// Hold the job's lock from another session, as a second scheduler on the
	// same database would while running it.
	ctx := context.Background()
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	var lockedByMe bool
	if err := other.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
		int32(0x46544b52), scheduler.LockID("job:"+job)).Scan(&lockedByMe); err != nil || !lockedByMe {
		t.Fatalf("take lock: %v %v", lockedByMe, err)
	}
	tick(t, s)
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, int32(0x46544b52), scheduler.LockID("job:"+job)); err != nil {
		t.Fatal(err)
	}
	if got := statuses(runsOf(t, pool, job)); len(got) != 1 || got[0] != scheduler.StatusSkippedRunning || calls.Load() != 0 {
		t.Fatalf("runs = %v calls = %d, want one skipped_running and no call", got, calls.Load())
	}
	clock.add(time.Minute)
	tick(t, s)
	if calls.Load() != 1 {
		t.Fatalf("job did not run once the lock was free")
	}
}

func TestLLMGroupSerialises(t *testing.T) {
	pool := testPool(t)
	clock := &fakeClock{t: nzTime(t, "2026-10-12 07:14:50")}
	started := make(chan string, 3)
	release := make(chan struct{})
	var running, maxLLM atomic.Int32
	run := func(_ context.Context, args []string) (scheduler.Counters, error) {
		if args[0] == "plain" {
			started <- "plain"
			return scheduler.Counters{}, nil
		}
		n := running.Add(1)
		defer running.Add(-1)
		for m := maxLLM.Load(); n > m && !maxLLM.CompareAndSwap(m, n); m = maxLLM.Load() {
		}
		started <- args[0]
		<-release
		return scheduler.Counters{}, nil
	}
	s := newScheduler(t, pool, clock, []jobs.Job{
		{Name: "schedtest-llm-a", Schedule: "calendar:07:15", Lock: jobs.LockLLM, Args: []string{"a"}, Needs: []string{jobs.NeedLLM}},
		{Name: "schedtest-llm-b", Schedule: "calendar:07:15", Lock: jobs.LockLLM, Args: []string{"b"}, Needs: []string{jobs.NeedLLM}},
		{Name: "schedtest-plain", Schedule: "calendar:07:15", Lock: jobs.LockNone, Args: []string{"plain"}},
	}, run, nil)

	clock.set(nzTime(t, "2026-10-12 07:15:00"))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One llm job and the lock=none job start; the other llm job queues on
	// the group lock without starting or recording a run.
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case name := <-started:
			got[name] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("started only %v", got)
		}
	}
	if !got["plain"] {
		t.Fatalf("lock=none job held back by the llm group: started %v", got)
	}
	queued := "schedtest-llm-b"
	if got["b"] {
		queued = "schedtest-llm-a"
	}
	select {
	case name := <-started:
		t.Fatalf("llm job %s started while the other held the llm group", name)
	case <-time.After(300 * time.Millisecond):
	}
	if rs := runsOf(t, pool, queued); len(rs) != 0 {
		t.Fatalf("%s recorded %v while queued", queued, statuses(rs))
	}

	close(release)
	s.Wait()
	if maxLLM.Load() != 1 {
		t.Errorf("max concurrent llm jobs = %d, want 1", maxLLM.Load())
	}
	for _, j := range []string{"schedtest-llm-a", "schedtest-llm-b", "schedtest-plain"} {
		if st := statuses(runsOf(t, pool, j)); len(st) != 1 || st[0] != "ok" {
			t.Errorf("%s runs = %v, want [ok]", j, st)
		}
	}
}

func TestPauseStopsWithinOneTickAndResumeContinues(t *testing.T) {
	pool := testPool(t)
	clock := &fakeClock{t: nzTime(t, "2026-10-12 10:00:00")}
	var paused atomic.Bool
	var calls atomic.Int32
	const job = "schedtest-pausable"
	s := newScheduler(t, pool, clock, []jobs.Job{
		{Name: job, Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"p"}},
	}, func(context.Context, []string) (scheduler.Counters, error) {
		calls.Add(1)
		return scheduler.Counters{}, nil
	}, func(c *scheduler.Config) {
		c.Paused = func(context.Context) (bool, error) { return paused.Load(), nil }
	})

	tick(t, s) // runs
	paused.Store(true)
	clock.add(time.Minute)
	tick(t, s) // very next due tick: skipped
	clock.add(30 * time.Second)
	tick(t, s) // not due: nothing recorded
	clock.add(30 * time.Second)
	tick(t, s) // due again, still paused
	paused.Store(false)
	clock.add(time.Minute)
	tick(t, s) // resumed

	want := "ok,skipped_paused,skipped_paused,ok"
	if got := strings.Join(statuses(runsOf(t, pool, job)), ","); got != want {
		t.Fatalf("runs = %s, want %s", got, want)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}

	// A pause state that can't be read runs nothing and records nothing.
	s2 := newScheduler(t, pool, clock, []jobs.Job{
		{Name: job, Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"p"}},
	}, func(context.Context, []string) (scheduler.Counters, error) {
		calls.Add(1)
		return scheduler.Counters{}, nil
	}, func(c *scheduler.Config) {
		c.Paused = func(context.Context) (bool, error) { return false, errors.New("db down") }
	})
	clock.add(time.Minute)
	if err := s2.Tick(context.Background()); err == nil {
		t.Fatal("tick with unreadable pause state did not fail")
	}
	s2.Wait()
	if calls.Load() != 2 || len(runsOf(t, pool, job)) != 4 {
		t.Fatal("tick with unreadable pause state ran or recorded something")
	}
}

func TestFailuresAndUnconfiguredAreRecorded(t *testing.T) {
	pool := testPool(t)
	clock := &fakeClock{t: nzTime(t, "2026-10-12 11:00:00")}
	s := newScheduler(t, pool, clock, []jobs.Job{
		{Name: "schedtest-fails", Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"fails"}},
		{Name: "schedtest-panics", Schedule: "interval:60", Lock: jobs.LockNone, Args: []string{"panics"}},
		{Name: "schedtest-needs-imap", Schedule: "interval:60", Lock: jobs.LockLLM, Args: []string{"mail"},
			Needs: []string{jobs.NeedLLM, jobs.NeedIMAP}},
	}, func(_ context.Context, args []string) (scheduler.Counters, error) {
		switch args[0] {
		case "fails":
			return scheduler.Counters{Fetched: 2}, errors.New("db: connection refused")
		case "panics":
			panic("boom")
		}
		t.Errorf("unconfigured job %v ran", args)
		return scheduler.Counters{}, nil
	}, func(c *scheduler.Config) { c.Settings = settings{llm: true, imap: false} })

	tick(t, s)
	if rs := runsOf(t, pool, "schedtest-fails"); len(rs) != 1 ||
		rs[0] != (runRow{Status: "error", Error: "db: connection refused", Fetched: 2}) {
		t.Errorf("fails runs = %+v", rs)
	}
	if rs := runsOf(t, pool, "schedtest-panics"); len(rs) != 1 || rs[0].Status != "error" || rs[0].Error != "panic: boom" {
		t.Errorf("panics runs = %+v", rs)
	}
	if rs := runsOf(t, pool, "schedtest-needs-imap"); len(rs) != 1 ||
		rs[0].Status != scheduler.StatusSkippedUnconfigured || rs[0].Error != "mailbox not configured (see Settings)" {
		t.Errorf("needs-imap runs = %+v", rs)
	}
}

func TestCLIRunnerReportsCLICounters(t *testing.T) {
	var hooked []string
	run := scheduler.CLIRunner(cli.Deps{
		Open: func(context.Context) (*cli.Conn, error) { return nil, errors.New("no database") },
		RunCompleted: func(source string, _ time.Time, _, _ int, _ error) {
			hooked = append(hooked, source)
		},
	})
	c, err := run(context.Background(), []string{"merge-dupes"})
	if err == nil || err.Error() != "db: no database" {
		t.Fatalf("err = %v", err)
	}
	if c != (scheduler.Counters{}) {
		t.Errorf("counters = %+v", c)
	}
	if len(hooked) != 1 || hooked[0] != "fraud merge-dupes" {
		t.Errorf("host RunCompleted not chained: %v", hooked)
	}
}
