// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// spec is one parsed jobs.Job.Schedule (the jobs.tsv grammar):
//
//	interval:<seconds>
//	calendar:HH:MM
//	calendar:HH:MM/dow=N                (N: 0=Sun..6=Sat)
//	calendar:HH:MM/dom=N                (N: 1..31)
//	calendar:HH:MM/dom=N/months=A,B,..  (only the listed months)
type spec struct {
	interval     time.Duration // > 0 for interval schedules
	hour, minute int
	dow          int // -1 = any day of week
	dom          int // 0 = any day of month
	months       map[time.Month]bool
}

func parseSpec(s string) (spec, error) {
	bad := func(why string) (spec, error) {
		return spec{}, fmt.Errorf("schedule %q: %s", s, why)
	}
	if rest, ok := strings.CutPrefix(s, "interval:"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			return bad("interval must be a positive number of seconds")
		}
		return spec{interval: time.Duration(n) * time.Second}, nil
	}
	rest, ok := strings.CutPrefix(s, "calendar:")
	if !ok {
		return bad("want interval:<s> or calendar:HH:MM[...]")
	}
	parts := strings.Split(rest, "/")
	hh, mm, ok := strings.Cut(parts[0], ":")
	h, herr := strconv.Atoi(hh)
	m, merr := strconv.Atoi(mm)
	if !ok || len(hh) != 2 || len(mm) != 2 || herr != nil || merr != nil || h > 23 || m > 59 {
		return bad("time must be HH:MM")
	}
	sp := spec{hour: h, minute: m, dow: -1}
	for _, p := range parts[1:] {
		key, val, _ := strings.Cut(p, "=")
		switch key {
		case "dow":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 6 || sp.dow >= 0 || sp.dom > 0 {
				return bad("dow must be one 0..6, without dom")
			}
			sp.dow = n
		case "dom":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > 31 || sp.dom > 0 || sp.dow >= 0 {
				return bad("dom must be one 1..31, without dow")
			}
			sp.dom = n
		case "months":
			if sp.dom == 0 || sp.months != nil {
				return bad("months needs a preceding dom")
			}
			sp.months = map[time.Month]bool{}
			for _, v := range strings.Split(val, ",") {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 12 {
					return bad("months must be 1..12")
				}
				sp.months[time.Month(n)] = true
			}
		default:
			return bad(fmt.Sprintf("unknown qualifier %q", key))
		}
	}
	return sp, nil
}

// firesAt reports whether a calendar spec fires at the minute starting at t
// (t already in the schedule's zone).
func (sp spec) firesAt(t time.Time) bool {
	if sp.interval > 0 || t.Hour() != sp.hour || t.Minute() != sp.minute {
		return false
	}
	if sp.dow >= 0 && int(t.Weekday()) != sp.dow {
		return false
	}
	if sp.dom > 0 && t.Day() != sp.dom {
		return false
	}
	if sp.months != nil && !sp.months[t.Month()] {
		return false
	}
	return true
}

// maxCatchUp bounds how far back a tick looks for a missed calendar
// instant (e.g. after the host slept): long enough for a weekly job, short
// enough that a long outage doesn't replay a backlog — at most one run per
// job per tick either way.
const maxCatchUp = 8 * 24 * time.Hour

// calendarDue reports whether a calendar spec has an instant in (after, now].
func (sp spec) calendarDue(after, now time.Time, loc *time.Location) bool {
	if sp.interval > 0 || !now.After(after) {
		return false
	}
	if now.Sub(after) > maxCatchUp {
		after = now.Add(-maxCatchUp)
	}
	for m := after.Truncate(time.Minute).Add(time.Minute); !m.After(now); m = m.Add(time.Minute) {
		if sp.firesAt(m.In(loc)) {
			return true
		}
	}
	return false
}
