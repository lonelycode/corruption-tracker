// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/store"
)

// frozen reports, and logs, that the tracker is read-only for a handoff,
// so a write subcommand should exit 0 without doing anything (§11.3): it
// keeps a host's scheduled jobs quiet instead of failing at the database
// guard every run.
func (c cmd) frozen(ctx context.Context, conn *Conn, sub string) (bool, error) {
	if conn.Store == nil {
		return false, nil
	}
	st, err := conn.Store.GetInstanceState(ctx)
	if err != nil {
		return false, err
	}
	if st.Writable() {
		return false, nil
	}
	c.log().Info("frozen, skipping", "run", sub, "mode", st.Mode) // "subcommand" is the host logger's key
	return true, nil
}

// Connectors lists every connector a handoff resume sets catching up.
func Connectors() []string {
	out := make([]string, 0, len(fraudagency.Sources)+2)
	for _, s := range fraudagency.Sources {
		out = append(out, jobs.ConnectorAgencyPrefix+s.Slug)
	}
	return append(out, jobs.ConnectorClassifyNews, jobs.ConnectorMail)
}

const handoffUsage = `usage: ingester fraud handoff <begin|status|complete|abort|resume>
  begin [--timeout 10m] [--by LABEL]   freeze the tracker read-only and wait for running jobs
  status                               show the handoff state
  complete --export-id ID --successor-url https://...
                                       mark this (source) instance handed off for good
  abort                                return a frozen source to normal operation
  resume                               on an imported target: catch up every connector and
                                       start scheduled ingestion`

// handoff runs the handoff protocol steps (design
// §11.4) that live on the command line.
func (c cmd) handoff(ctx context.Context, args []string) (rerr error) {
	if len(args) < 1 {
		return errors.New(handoffUsage)
	}
	sub := args[0]
	started := time.Now()
	defer func() { c.finish("fraud handoff "+sub, started, 0, 0, rerr) }()
	fs := flag.NewFlagSet("fraud handoff "+sub, flag.ExitOnError)
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for in-flight writers and running jobs")
	by := fs.String("by", "", "who is freezing (default: $USER on this host)")
	exportID := fs.String("export-id", "", "the handoff bundle's export id (complete)")
	successor := fs.String("successor-url", "", "where the tracker continues, an https URL (complete)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	st := conn.Store
	log := c.log()

	switch sub {
	case "begin":
		label := *by
		if label == "" {
			label = "cli:" + os.Getenv("USER")
		}
		state, err := st.Freeze(ctx, label, *timeout)
		if errors.Is(err, store.ErrFrozen) {
			return errors.New("the tracker is already frozen or handed off; see `handoff status`")
		}
		if err != nil {
			return err
		}
		log.Info("tracker frozen for handoff", "freeze_id", state.FreezeID, "frozen_by", state.FrozenBy)
		deadline := time.Now().Add(*timeout)
		for {
			running, err := st.RunningJobs(ctx)
			if err != nil {
				return err
			}
			if len(running) == 0 {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("frozen, but jobs still running after %s: %s (abort, or investigate and retry status)",
					*timeout, strings.Join(running, ", "))
			}
			log.Info("waiting for running jobs to drain", "running", strings.Join(running, ", "))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		log.Info("handoff: frozen and drained; next, export --handoff --out DIR", "freeze_id", state.FreezeID)
		return nil

	case "status":
		state, err := st.GetInstanceState(ctx)
		if err != nil {
			return err
		}
		running, err := st.RunningJobs(ctx)
		if err != nil {
			return err
		}
		log.Info("handoff status", "mode", state.Mode, "freeze_id", state.FreezeID, "frozen_by", state.FrozenBy,
			"handoff_export_id", state.HandoffExportID, "successor_url", state.SuccessorURL,
			"ingestion_paused", state.IngestionPaused, "running_jobs", strings.Join(running, ", "))
		return nil

	case "complete":
		if *exportID == "" || *successor == "" {
			return errors.New("usage: ingester fraud handoff complete --export-id ID --successor-url https://...")
		}
		if err := st.CompleteHandoff(ctx, *exportID, *successor); err != nil {
			return err
		}
		log.Info("handoff complete: this tracker is now read-only for good", "successor_url", *successor)
		return nil

	case "abort":
		if err := st.AbortHandoff(ctx); err != nil {
			return err
		}
		log.Info("handoff aborted: back to normal operation; destroy any bundle taken under the aborted freeze")
		return nil

	case "resume":
		if err := st.ResumeAfterImport(ctx, Connectors()); err != nil {
			return err
		}
		log.Info("ingestion resumed: connectors catch up from 24h before the source's freeze")
		return nil
	}
	return errors.New(handoffUsage)
}
