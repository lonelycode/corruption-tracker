// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/store"
)

// export writes a bundle v1 of the whole tracker dataset (§10) into --out:
// one REPEATABLE READ snapshot, byte-stable table files, a manifest with
// checksums. Read-only; concurrent ingestion keeps running.
func (c cmd) export(ctx context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched, newCount int
	defer func() { c.finish("fraud export", started, fetched, newCount, rerr) }()

	fs := flag.NewFlagSet("fraud export", flag.ExitOnError)
	out := fs.String("out", "", "directory to write the bundle into (required)")
	instanceID := fs.String("instance-id", "", "source instance id recorded in the manifest (default: this instance's tracker_instance id)")
	baseURL := fs.String("base-url", "", "source base URL recorded in the manifest")
	accessLog := fs.Bool("include-access-log", false, "also export tracker_access_log")
	handoff := fs.Bool("handoff", false, "the handoff bundle: requires a frozen tracker (handoff begin) and records the freeze in the manifest")
	secrets := fs.Bool("include-secrets", false, "add secrets.enc, the secret settings encrypted under $"+bundle.PassphraseEnv+" (standalone sources only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("usage: ingester fraud export --out DIR [--instance-id ID] [--base-url URL] [--include-access-log]")
	}

	log := c.log()
	conn, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer conn.close()
	if *instanceID == "" {
		inst, err := conn.Store.EnsureInstance(ctx)
		if err != nil {
			return err
		}
		*instanceID = inst.ID
	}
	var ho *bundle.Handoff
	if *handoff {
		state, err := conn.Store.GetInstanceState(ctx)
		if err != nil {
			return err
		}
		if state.Mode != store.ModeFrozen || state.FrozenAt == nil {
			return fmt.Errorf("export --handoff needs a frozen tracker (mode is %s); run handoff begin first", state.Mode)
		}
		ho = &bundle.Handoff{FreezeID: state.FreezeID, FrozenAt: state.FrozenAt.UTC().Format(time.RFC3339),
			FrozenBy: state.FrozenBy}
	}

	path, m, err := bundle.Export(ctx, conn.Store.Pool, *out, bundle.ExportOptions{
		Source:           bundle.Source{InstanceID: *instanceID, Kind: c.d.SourceKind, BaseURL: *baseURL},
		AppVersion:       appVersion(),
		IncludeAccessLog: *accessLog,
		IncludeSecrets:   *secrets,
		Passphrase:       os.Getenv(bundle.PassphraseEnv),
		Secrets:          c.d.Secrets,
		Handoff:          ho,
		Log:              log,
	})
	if err != nil {
		return err
	}
	if ho != nil {
		if err := conn.Store.RecordHandoffExport(ctx, m.ExportID); err != nil {
			return err
		}
		log.Info("handoff bundle written; import it on the target, then run handoff complete here",
			"export_id", m.ExportID, "freeze_id", ho.FreezeID)
	}
	fetched, newCount = len(m.Tables), 1
	log.Info("fraud export done", "path", path, "export_id", m.ExportID, "schema_version", m.SchemaVersion)
	return nil
}

// verifyBundle checks a bundle offline: manifest, entry set and order,
// checksums and row counts. No database needed.
func (c cmd) verifyBundle(_ context.Context, args []string) (rerr error) {
	started := time.Now()
	var fetched int
	defer func() { c.finish("fraud verify-bundle", started, fetched, 0, rerr) }()

	fs := flag.NewFlagSet("fraud verify-bundle", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ingester fraud verify-bundle FILE")
	}
	m, err := bundle.VerifyFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var rows int64
	for _, t := range m.Tables {
		rows += t.Rows
	}
	fetched = len(m.Tables)
	c.log().Info("bundle verified", "export_id", m.ExportID, "schema_version", m.SchemaVersion,
		"source_kind", m.Source.Kind, "created_at", m.CreatedAt, "tables", len(m.Tables),
		"omitted_tables", len(m.OmittedTables), "rows", rows)
	return nil
}

// appVersion is the module version and VCS revision the binary was built
// from, when the build recorded them.
func appVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := bi.Main.Version
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			v += "+" + s.Value[:7]
		}
	}
	return v
}
