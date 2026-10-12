// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package scheduler

import (
	"context"
	"time"

	"github.com/lonelycode/corruption-tracker/cli"
)

// CLIRunner runs jobs through the tracker CLI in-process, exactly as
// `fraud <args>` would, and reports the counters the CLI emits through
// RunCompleted (which still reaches d's own hook, if any).
func CLIRunner(d cli.Deps) RunFunc {
	return func(ctx context.Context, args []string) (Counters, error) {
		var c Counters
		dd := d
		dd.RunCompleted = func(source string, started time.Time, fetched, newCount int, err error) {
			c = Counters{Fetched: fetched, New: newCount}
			if d.RunCompleted != nil {
				d.RunCompleted(source, started, fetched, newCount, err)
			}
		}
		err := cli.Run(ctx, dd, args)
		return c, err
	}
}
