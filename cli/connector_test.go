// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// A connector an operator disabled stays off for every run, scheduled or
// manual: the subcommand returns before touching its source.
func TestDisabledConnectorsAreSkipped(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, c := range []string{jobs.ConnectorClassifyNews, jobs.ConnectorMail} {
		if err := st.SetConnectorEnabled(ctx, c, false); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	d := cli.Deps{
		// No News source and no mail dir: reaching either would fail.
		Open: func(context.Context) (*cli.Conn, error) { return &cli.Conn{Store: st}, nil },
		Log:  slog.New(slog.NewTextHandler(&logs, nil)),
	}
	for _, args := range [][]string{
		{"classify-news", "--dry-run"},
		{"mail-ingest", "--dir", "/nonexistent/maildir"},
	} {
		if err := cli.Run(ctx, d, args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, want := range []string{"fraud classify-news skipped: connector disabled", "fraud mail-ingest skipped: connector disabled"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing log %q:\n%s", want, logs.String())
		}
	}

	// Re-enabled, the same run reaches its source again.
	if err := st.SetConnectorEnabled(ctx, jobs.ConnectorClassifyNews, true); err != nil {
		t.Fatal(err)
	}
	if err := cli.Run(ctx, d, []string{"classify-news", "--dry-run"}); err == nil ||
		!strings.Contains(err.Error(), "no news corpus configured") {
		t.Errorf("enabled classify-news = %v, want it to reach the (missing) news source", err)
	}
}
