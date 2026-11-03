// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/jobs"
)

var errOpenCalled = errors.New("open called")

type runRecord struct {
	source string
	err    error
}

// stubDeps fails Open with errOpenCalled, so a command that parses its args
// and reaches the database stops there without touching anything.
func stubDeps(runs *[]runRecord) Deps {
	return Deps{
		Open: func(context.Context) (*Conn, error) { return nil, errOpenCalled },
		RunCompleted: func(source string, _ time.Time, _, _ int, err error) {
			*runs = append(*runs, runRecord{source, err})
		},
	}
}

// Every scheduled job's args must parse: a flag the CLI no longer accepts
// would otherwise only surface as a failed launchd run in prod. (A bad flag
// exits the test binary via flag.ExitOnError, which also fails the test.)
func TestScheduledJobArgsReachTheDatabase(t *testing.T) {
	for _, j := range jobs.Schedule {
		t.Run(j.Name, func(t *testing.T) {
			var runs []runRecord
			err := Run(context.Background(), stubDeps(&runs), j.Args)
			if !errors.Is(err, errOpenCalled) {
				t.Fatalf("args %v: got %v, want the Open error", j.Args, err)
			}
			if !strings.HasPrefix(err.Error(), "db: ") {
				t.Errorf("Open error not wrapped as before: %q", err)
			}
			if len(runs) != 1 || !errors.Is(runs[0].err, errOpenCalled) {
				t.Fatalf("RunCompleted calls = %+v, want one carrying the error", runs)
			}
			// The metric source is "fraud <subcommand>", as the dashboard
			// already groups it.
			want := "fraud " + j.Args[0]
			if j.Args[0] == "snapshot" {
				want = "fraud snapshot build"
			}
			if runs[0].source != want {
				t.Errorf("RunCompleted source = %q, want %q", runs[0].source, want)
			}
		})
	}
}

func TestUsageErrorsNeedNoDatabase(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "usage: ingester fraud <subcommand>"},
		{[]string{"bogus"}, `unknown fraud subcommand "bogus"`},
		{[]string{"snapshot"}, "usage: ingester fraud snapshot build [flags]"},
		{[]string{"snapshot", "list"}, "usage: ingester fraud snapshot build [flags]"},
		{[]string{"mail-ingest"}, "usage: ingester fraud mail-ingest (--dir PATH [--archive PATH] | --imap) [--dry-run]"},
		{[]string{"poll", "--source", "nope"}, `unknown fraud poll source "nope"`},
		{[]string{"import", "--file", "/nonexistent/fraud.json"}, "read /nonexistent/fraud.json (run corruption-tracker/tools/sheetnormalize/fraud_sheet_normalize.py first)"},
		{[]string{"export"}, "usage: ingester fraud export --out DIR"},
		{[]string{"verify-bundle"}, "usage: ingester fraud verify-bundle FILE"},
		{[]string{"verify-bundle", "/nonexistent/bundle.tar.gz"}, "open /nonexistent/bundle.tar.gz"},
	} {
		var runs []runRecord
		err := Run(context.Background(), stubDeps(&runs), tc.args)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("Run(%v) = %v, want prefix %q", tc.args, err, tc.want)
		}
		if errors.Is(err, errOpenCalled) {
			t.Errorf("Run(%v) opened the database", tc.args)
		}
	}
}

type settings struct {
	imap host.IMAPConfig
	ok   bool
}

func (s settings) IMAP() (host.IMAPConfig, bool) { return s.imap, s.ok }
func (settings) LLMConfigured() bool             { return true }
func (settings) Timezone() *time.Location        { return time.UTC }

func TestMailIngestIMAPSkipsWhenUnconfigured(t *testing.T) {
	for name, s := range map[string]host.Settings{"nil settings": nil, "no mailbox": settings{}} {
		var runs []runRecord
		closed := false
		d := Deps{
			Open: func(context.Context) (*Conn, error) {
				return &Conn{Close: func() { closed = true }}, nil
			},
			Settings: s,
			RunCompleted: func(source string, _ time.Time, fetched, newCount int, err error) {
				runs = append(runs, runRecord{source, err})
			},
		}
		if err := Run(context.Background(), d, []string{"mail-ingest", "--imap"}); err != nil {
			t.Fatalf("%s: err = %v, want graceful skip", name, err)
		}
		if !closed {
			t.Errorf("%s: connection not closed", name)
		}
		if len(runs) != 1 || runs[0].source != "fraud mail-ingest" || runs[0].err != nil {
			t.Errorf("%s: RunCompleted = %+v", name, runs)
		}
	}
}

func TestResolvePeriod(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, tc := range []struct{ period, now, want string }{
		{"", "2026-10-12", "2026-W41"}, // Monday 12 Oct: the week that just closed
		{"last-quarter", "2026-10-12", "2026-Q3"},
		{"last-quarter", "2026-01-01", "2025-Q4"},
		{"last-quarter", "2026-04-01", "2026-Q1"},
		{"last-year", "2026-01-02", "2025"},
		{"2025-W07", "2026-10-12", "2025-W07"},
		{"2024", "2026-10-12", "2024"},
	} {
		if got := resolvePeriod(tc.period, at(tc.now)); got != tc.want {
			t.Errorf("resolvePeriod(%q, %s) = %q, want %q", tc.period, tc.now, got, tc.want)
		}
	}
}

// The schedule fires in NZ local time while the container clock is UTC:
// periods must resolve in the schedule zone.
func TestResolvePeriodInScheduleZone(t *testing.T) {
	t.Setenv("TRACKER_TIMEZONE", "")
	loc := cmd{d: stubDeps(nil)}.reportLocation()
	if loc.String() != "Pacific/Auckland" {
		t.Fatalf("default zone = %s", loc)
	}
	for _, tc := range []struct{ utc, period, want string }{
		// 07:30 NZDT 1 Oct 2026 == 18:30Z 30 Sep: the quarter that closed is Q3.
		{"2026-09-30T18:30:00Z", "last-quarter", "2026-Q3"},
		// 07:30 NZDT 1 Jan 2027 == 18:30Z 31 Dec 2026.
		{"2026-12-31T18:30:00Z", "last-quarter", "2026-Q4"},
		// Monday 06:30 NZDT 2 Nov == Sunday 17:30Z 1 Nov: W44 just closed.
		{"2026-11-01T17:30:00Z", "", "2026-W44"},
		// 07:45 NZDT 2 Jan 2027 (annual).
		{"2027-01-01T18:45:00Z", "last-year", "2026"},
	} {
		now, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := resolvePeriod(tc.period, now.In(loc)); got != tc.want {
			t.Errorf("resolvePeriod(%q, %s in %s) = %q, want %q", tc.period, tc.utc, loc, got, tc.want)
		}
	}
	t.Setenv("TRACKER_TIMEZONE", "UTC")
	if got := (cmd{d: stubDeps(nil)}).reportLocation().String(); got != "UTC" {
		t.Errorf("TRACKER_TIMEZONE override ignored: %s", got)
	}
}
