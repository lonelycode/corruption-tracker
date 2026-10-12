// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// While frozen, write subcommands exit 0 with "frozen, skipping" (§11.3),
// so a host's scheduled jobs stay quiet; read-only runs still work.
func TestWriteSubcommandsSkipWhileFrozen(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, err := st.Freeze(ctx, "test", time.Second); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	d := cli.Deps{
		Open: func(context.Context) (*cli.Conn, error) { return &cli.Conn{Store: st}, nil },
		Log:  slog.New(slog.NewTextHandler(&logs, nil)),
	}
	for _, args := range [][]string{
		{"snapshot", "build", "--period", "2026-W01"},
		{"merge-dupes", "--apply"},
		{"rederive-status", "--apply"},
		{"classify-news"},
		{"recode"},
		{"extract-pending"},
		{"mail-ingest", "--dir", "/nonexistent"},
		{"poll", "--source", "sfo"},
	} {
		logs.Reset()
		if err := cli.Run(ctx, d, args); err != nil {
			t.Errorf("%v while frozen = %v, want a quiet skip", args, err)
		}
		if !strings.Contains(logs.String(), `msg="frozen, skipping"`) {
			t.Errorf("%v: no frozen log line:\n%s", args, logs.String())
		}
	}
	// Report-only runs still run.
	logs.Reset()
	if err := cli.Run(ctx, d, []string{"merge-dupes"}); err != nil || strings.Contains(logs.String(), "frozen") {
		t.Errorf("dry-run merge-dupes while frozen = %v\n%s", err, logs.String())
	}
	if err := cli.Run(ctx, d, []string{"handoff", "status"}); err != nil {
		t.Errorf("handoff status = %v", err)
	}
	if err := cli.Run(ctx, d, []string{"handoff", "abort"}); err != nil {
		t.Errorf("handoff abort = %v", err)
	}
	logs.Reset()
	if err := cli.Run(ctx, d, []string{"merge-dupes", "--apply"}); err != nil || strings.Contains(logs.String(), "frozen") {
		t.Errorf("merge-dupes --apply after abort = %v\n%s", err, logs.String())
	}
	// export --handoff refuses an unfrozen tracker.
	if err := cli.Run(ctx, d, []string{"export", "--handoff", "--out", t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "needs a frozen tracker") {
		t.Errorf("export --handoff unfrozen = %v", err)
	}
}
