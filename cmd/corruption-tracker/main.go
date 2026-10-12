// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Command corruption-tracker is the standalone tracker: one binary that
// serves the web app with its job scheduler, migrates its database, and
// runs jobs, exports and account maintenance from the command line.
//
//	corruption-tracker serve              wait for Postgres, migrate, serve + schedule
//	corruption-tracker migrate            apply pending migrations and exit
//	corruption-tracker job list           the job schedule
//	corruption-tracker job run NAME       run one scheduled job now (ignores pause)
//	corruption-tracker export --out DIR   write an export bundle
//	corruption-tracker import FILE        restore an export bundle
//	corruption-tracker backup             take a backup now (also nightly)
//	corruption-tracker restore PATH       restore a backup's database dump
//	corruption-tracker user ...           local account maintenance
//
// The environment carries only what is needed before the database is up:
// DATABASE_URL (required), TRACKER_LISTEN (default :8080),
// TRACKER_TRUST_PROXY=1 behind a reverse proxy, and TRACKER_DATA_DIR (the
// data volume holding the generated key that encrypts stored secrets,
// default /data) or TRACKER_SECRET (derive that key instead). Everything
// else (site name, LLM, mailbox, crawler identity, time zone, source URL)
// is set on the admin Settings page; any setting's environment variable,
// e.g. TRACKER_TIMEZONE or TRACKER_LLM_API_KEY, overrides it and locks the
// field (see package settings); TRACKER_OPERATOR is the site name's. A
// blank TRACKER_SOURCE_URL refuses to start (AGPL offer).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lonelycode/corruption-tracker/backup"
	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/migrations"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/standalone/server"
)

const usage = `usage: corruption-tracker <command>
  serve                 wait for Postgres, apply migrations, serve the app and run the job schedule
  migrate               apply pending migrations and exit
  job list              print the job schedule
  job run NAME          run one scheduled job now (manual runs ignore the ingestion pause)
  export --out DIR      write an export bundle (see export --help)
  import [--replace] [--backup-dir DIR] FILE
                        restore an export bundle into this (empty) instance; --replace
                        imports over existing data after a pg_dump + bundle backup
  backup                take a backup now (pg_dump + export bundle under $TRACKER_DATA_DIR/backups)
  restore [--force] PATH
                        load a backup's database dump into this (empty) instance, then migrate
  handoff begin|status|complete|abort|resume
                        the handoff protocol (also on /admin/handoff); export --handoff
                        writes the handoff bundle from a frozen tracker
  user list | reset-password NAME   local account maintenance`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(os.Stderr, usage)
		if len(args) == 0 {
			return errors.New("no command")
		}
		return nil
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: utcLogTime}))
	slog.SetDefault(log)
	opts := server.Options{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Listen:      os.Getenv("TRACKER_LISTEN"),
		TrustProxy:  os.Getenv("TRACKER_TRUST_PROXY") == "1",
		// Pages show times in the site time zone.
		ProcessTimezone: true,
		Log:             log,
	}
	switch args[0] {
	case "serve":
		s, err := server.Boot(ctx, opts)
		if err != nil {
			return err
		}
		defer s.Pool.Close()
		return s.Serve(ctx)

	case "migrate":
		pool, err := server.WaitForPostgres(ctx, opts.DatabaseURL, 0, log)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := server.PreMigrationBackup(ctx, pool, opts); err != nil {
			return err
		}
		if err := server.Migrate(ctx, pool, opts.DatabaseURL); err != nil {
			return err
		}
		v, _, err := migrations.Version(opts.DatabaseURL)
		if err != nil {
			return err
		}
		log.Info("migrations applied", "schema_version", v)
		return nil

	case "job":
		if len(args) < 2 || (args[1] == "run" && len(args) != 3) || (args[1] != "run" && args[1] != "list") {
			return errors.New("usage: corruption-tracker job list | job run NAME")
		}
		s, err := server.Boot(ctx, opts)
		if err != nil {
			return err
		}
		defer s.Pool.Close()
		if args[1] == "list" {
			status, err := s.Scheduler.Status(ctx)
			if err != nil {
				return err
			}
			for _, j := range status {
				next := "—"
				if j.NextRun != nil {
					next = j.NextRun.UTC().Format(time.RFC3339)
				}
				fmt.Printf("%-26s %-40s next %s\n", j.Job.Name, j.Job.Schedule, next)
			}
			return nil
		}
		status, err := s.Scheduler.RunNow(ctx, args[2])
		if err != nil {
			return err
		}
		log.Info("job run finished", "job", args[2], "status", status)
		return nil

	case "export":
		s, err := server.Boot(ctx, opts)
		if err != nil {
			return err
		}
		defer s.Pool.Close()
		return cli.Run(ctx, server.CLIDeps(s.Pool, s.SettingsStore, log), args)

	case "user":
		s, err := server.Boot(ctx, opts)
		if err != nil {
			return err
		}
		defer s.Pool.Close()
		return localauth.RunCLI(ctx, s.Accounts, args[1:], os.Stdin, os.Stdout)

	case "import":
		return runImport(ctx, opts, args[1:])
	case "backup":
		pool, err := server.WaitForPostgres(ctx, opts.DatabaseURL, 0, log)
		if err != nil {
			return err
		}
		defer pool.Close()
		b, err := backup.New(backup.Options{Pool: pool, DSN: opts.DatabaseURL,
			Dir: filepath.Join(settings.ResolveDataDir(nil, ""), backup.DirName), Log: log}).Run(ctx, backup.KindManual, "command line")
		if err != nil {
			return err
		}
		fmt.Printf("backup written: %s\n", b.Name)
		return nil

	case "restore":
		return runRestore(ctx, opts, args[1:])

	case "handoff":
		s, err := server.Boot(ctx, opts)
		if err != nil {
			return err
		}
		defer s.Pool.Close()
		return cli.Run(ctx, server.CLIDeps(s.Pool, s.SettingsStore, log), args)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

// utcLogTime keeps log timestamps in UTC ("…Z") after ProcessTimezone
// moves time.Local to the site time zone.
func utcLogTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}

// runRestore loads a backup's database dump, then migrates it to this
// binary's schema (a backup from an older version is upgraded in place).
func runRestore(ctx context.Context, opts server.Options, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	force := fs.Bool("force", false, "restore over a tracker that already has accounts or cases (everything in it is replaced)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: corruption-tracker restore [--force] BACKUP_DIR|database.dump")
	}
	pool, err := server.WaitForPostgres(ctx, opts.DatabaseURL, 0, opts.Log)
	if err != nil {
		return err
	}
	defer pool.Close()
	r := backup.New(backup.Options{Pool: pool, DSN: opts.DatabaseURL, Log: opts.Log})
	if err := r.Restore(ctx, fs.Arg(0), backup.RestoreOptions{Force: *force}); err != nil {
		return err
	}
	if err := server.Migrate(ctx, pool, opts.DatabaseURL); err != nil {
		return fmt.Errorf("restored, but migrating it failed: %w", err)
	}
	v, _, err := migrations.Version(opts.DatabaseURL)
	if err != nil {
		return err
	}
	fmt.Printf("restored %s (schema now %d). Stored secrets need this server's data volume key or TRACKER_SECRET; without it, re-enter them on the Settings page.\n", fs.Arg(0), v)
	return nil
}

// runImport restores a bundle (design §10.6) and
// prints the verification report.
func runImport(ctx context.Context, opts server.Options, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	replace := fs.Bool("replace", false, "import over existing data (takes a pg_dump and a bundle backup first)")
	backupDir := fs.String("backup-dir", "backups", "where --replace writes its backups")
	pgDump := fs.String("pg-dump", "pg_dump", "pg_dump binary for the --replace backup (must match the server's major version)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: corruption-tracker import [--replace] [--backup-dir DIR] [--pg-dump BIN] FILE")
	}
	pool, err := server.WaitForPostgres(ctx, opts.DatabaseURL, 0, opts.Log)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The settings store needs its table; on a brand-new database create the
	// schema first (the import then takes it to the bundle's version).
	if _, ok, err := migrations.Version(opts.DatabaseURL); err != nil {
		return err
	} else if !ok {
		if err := server.Migrate(ctx, pool, opts.DatabaseURL); err != nil {
			return err
		}
	}
	cfg, err := settings.Open(ctx, pool, settings.Options{Log: opts.Log})
	if err != nil {
		return err
	}
	passphrase, _ := os.LookupEnv(bundle.PassphraseEnv)
	rep, err := bundle.ImportFile(ctx, pool, fs.Arg(0), bundle.ImportOptions{
		DSN: opts.DatabaseURL, Replace: *replace, BackupDir: *backupDir, PGDump: *pgDump,
		Settings: cfg, Passphrase: passphrase, Log: opts.Log,
	})
	for _, b := range rep.Backups {
		fmt.Printf("backup written: %s\n", b)
	}
	if len(rep.Tables) > 0 {
		fmt.Printf("%-30s %8s  %-64s  %s\n", "table", "rows", "sha256", "ok")
		for _, t := range rep.Tables {
			fmt.Printf("%-30s %8d  %-64s  %v\n", t.Name, t.Rows, t.SHA256, t.OK)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("imported export %s from instance %s (schema %d); ingestion is paused until you resume it on /admin/ingestion\n",
		rep.ExportID, rep.SourceInstance, rep.SchemaVersion)
	fmt.Printf("settings imported: %d, secrets imported: %d\n", rep.SettingsImported, rep.SecretsImported)
	if len(rep.SettingsSkipped) > 0 {
		fmt.Printf("settings skipped (unknown to this version): %s\n", strings.Join(rep.SettingsSkipped, ", "))
	}
	if rep.SecretsNotImported != "" {
		fmt.Println("secrets not imported: " + rep.SecretsNotImported)
	}
	return nil
}
