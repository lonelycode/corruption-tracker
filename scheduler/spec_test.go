// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package scheduler

import (
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/jobs"
)

func nz(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(DefaultTimezone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestEveryScheduledJobParses(t *testing.T) {
	for _, j := range jobs.Schedule {
		if _, err := parseSpec(j.Schedule); err != nil {
			t.Errorf("%s: %v", j.Name, err)
		}
	}
}

func TestParseSpecRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"", "cron:* * * * *", "interval:0", "interval:-5", "interval:x",
		"calendar:7:15", "calendar:24:00", "calendar:07:60", "calendar:07:15/dow=7",
		"calendar:07:15/dom=0", "calendar:07:15/dom=32", "calendar:07:15/months=1",
		"calendar:07:15/dom=1/months=13", "calendar:07:15/dow=1/dom=1", "calendar:07:15/tz=UTC",
	} {
		if _, err := parseSpec(s); err == nil {
			t.Errorf("parseSpec(%q) accepted a malformed schedule", s)
		}
	}
}

func TestCalendarDue(t *testing.T) {
	loc := nz(t)
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, tc := range []struct {
		schedule, after, now string
		want                 bool
	}{
		{"calendar:07:15", "2026-10-12 07:14:40", "2026-10-12 07:15:10", true},
		{"calendar:07:15", "2026-10-12 07:15:00", "2026-10-12 07:15:30", false}, // instant belongs to the previous window
		{"calendar:07:15", "2026-10-12 07:14:30", "2026-10-12 07:15:00", true},
		{"calendar:07:15", "2026-10-12 07:15:10", "2026-10-12 07:15:40", false},
		{"calendar:07:15", "2026-10-12 06:00:00", "2026-10-12 07:00:00", false},
		{"calendar:07:15", "2026-10-11 23:00:00", "2026-10-12 23:00:00", true}, // missed while asleep: caught up once
		// 12 Oct 2026 is a Monday.
		{"calendar:06:30/dow=1", "2026-10-12 06:29:50", "2026-10-12 06:30:20", true},
		{"calendar:06:30/dow=1", "2026-10-13 06:29:50", "2026-10-13 06:30:20", false},
		{"calendar:07:30/dom=1/months=1,4,7,10", "2026-10-01 07:29:50", "2026-10-01 07:30:20", true},
		{"calendar:07:30/dom=1/months=1,4,7,10", "2026-11-01 07:29:50", "2026-11-01 07:30:20", false},
		{"calendar:07:45/dom=2/months=1", "2027-01-02 07:44:50", "2027-01-02 07:45:20", true},
		{"calendar:07:45/dom=2/months=1", "2026-10-02 07:44:50", "2026-10-02 07:45:20", false},
		{"interval:60", "2026-10-12 07:00:00", "2026-10-12 08:00:00", false}, // not a calendar spec
	} {
		sp, err := parseSpec(tc.schedule)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.calendarDue(at(tc.after), at(tc.now), loc); got != tc.want {
			t.Errorf("%s in (%s, %s] = %v, want %v", tc.schedule, tc.after, tc.now, got, tc.want)
		}
	}
}

// Times are NZ wall-clock even when the process clock is UTC.
func TestCalendarDueUsesScheduleZone(t *testing.T) {
	sp, _ := parseSpec("calendar:07:15")
	// 07:15 NZDT on 12 Oct 2026 is 18:15 UTC on 11 Oct.
	after := time.Date(2026, 10, 11, 18, 14, 50, 0, time.UTC)
	now := time.Date(2026, 10, 11, 18, 15, 20, 0, time.UTC)
	if !sp.calendarDue(after, now, nz(t)) {
		t.Error("07:15 NZ not due at 18:15 UTC")
	}
	if sp.calendarDue(after, now, time.UTC) {
		t.Error("07:15 UTC due at 18:15 UTC")
	}
}

func TestLockIDsAvoidReservedRange(t *testing.T) {
	for _, j := range jobs.Schedule {
		if id := lockID("job:" + j.Name); id < 1<<30 {
			t.Errorf("lockID(%s) = %d, inside the reserved low range", j.Name, id)
		}
	}
	if lockID("job:a") == lockID("job:b") {
		t.Error("lock ids collide")
	}
}

func TestNextCalendar(t *testing.T) {
	loc := nz(t)
	from := time.Date(2026, 10, 12, 8, 0, 0, 0, loc) // a Monday
	for _, tc := range []struct{ schedule, want string }{
		{"calendar:07:15", "2026-10-13 07:15"},
		{"calendar:08:30", "2026-10-12 08:30"},
		{"calendar:06:30/dow=1", "2026-10-19 06:30"},
		{"calendar:07:30/dom=1/months=1,4,7,10", "2027-01-01 07:30"},
		{"calendar:07:45/dom=2/months=1", "2027-01-02 07:45"},
	} {
		sp, err := parseSpec(tc.schedule)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := sp.nextCalendar(from, loc)
		if !ok || got.In(loc).Format("2006-01-02 15:04") != tc.want {
			t.Errorf("next %s after %s = %v %v, want %s", tc.schedule, from, got.In(loc), ok, tc.want)
		}
	}
}
