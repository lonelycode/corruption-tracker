// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package server is the standalone corruption-tracker server: Postgres
// wait + automatic migrations, local accounts, the back office and reader
// surfaces, and the in-process job scheduler with its ingestion status page
// (design §9). cmd/corruption-tracker is a thin
// command line over it.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/backup"
	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/ingest/newsfeed"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/llm"
	"github.com/lonelycode/corruption-tracker/migrations"
	"github.com/lonelycode/corruption-tracker/readmodel"
	"github.com/lonelycode/corruption-tracker/scheduler"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/standalone/accesslog"
	sitelayout "github.com/lonelycode/corruption-tracker/standalone/layout"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/version"
	"github.com/lonelycode/corruption-tracker/web"
	"github.com/lonelycode/corruption-tracker/web/reader"
)

// Options configure the standalone server.
type Options struct {
	DatabaseURL string
	// Listen is the HTTP address (default ":8080"); Listener, when set,
	// is used instead (tests).
	Listen   string
	Listener net.Listener
	// TrustProxy honours X-Forwarded-* from the compose deployment's proxy.
	TrustProxy bool
	// OperatorName, when set, pins the instance name (header and the
	// suppression notice on reader surfaces) over the Settings page. The
	// binary leaves it empty: TRACKER_OPERATOR locks the setting instead.
	OperatorName string
	// Layout is the page chrome; nil builds the standalone layout
	// (standalone/layout) with the instance name and live branding.
	Layout host.Layout
	// LookupEnv resolves TRACKER_SOURCE_URL for the AGPL source offer
	// (default os.LookupEnv). An unusable source URL refuses to start.
	LookupEnv func(string) (string, bool)
	// Settings, when nil, is the settings store (tracker_settings, edited
	// at /admin/settings), opened over the pool. Tests may pass their own.
	Settings host.Settings
	// DataDir holds the generated secret key that encrypts stored secrets
	// (default $TRACKER_DATA_DIR, else /data).
	DataDir string
	// MaxImportBytes caps a setup-wizard bundle upload (default
	// DefaultMaxImportBytes).
	MaxImportBytes int64
	// WaitForDB bounds how long boot waits for Postgres (default 60 s).
	WaitForDB time.Duration
	// ProcessTimezone sets time.Local to the site time zone (Settings →
	// Time zone) when Build opens the settings store, so pages show
	// timestamps in the operator's zone rather than the container's UTC.
	// time.Local is process-wide: the binary sets
	// this, tests and embedding hosts leave it off. A change of zone
	// applies after a restart, as the setting's help says.
	ProcessTimezone bool
	Log             *slog.Logger

	// Tests may pin the scheduler's clock, schedule and job runner.
	Now  func() time.Time
	Jobs []jobs.Job
	Run  scheduler.RunFunc
}

// Server is a built standalone server.
type Server struct {
	Pool      *pgxpool.Pool
	Store     *store.Store
	Scheduler *scheduler.Scheduler
	Handler   http.Handler
	Accounts  *localauth.Service
	// SettingsStore is the settings store when Build opened it (nil when
	// Options.Settings was supplied).
	SettingsStore *settings.Service
	// Backups takes, lists and prunes this instance's backups.
	Backups *backup.Runner
	opts    Options
}

// Boot waits for Postgres, applies the tracker migrations and builds the
// server. Close the returned Server's Pool when done.
func Boot(ctx context.Context, o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	pool, err := WaitForPostgres(ctx, o.DatabaseURL, o.WaitForDB, o.Log)
	if err != nil {
		return nil, err
	}
	if err := PreMigrationBackup(ctx, pool, o); err != nil {
		pool.Close()
		return nil, err
	}
	if err := Migrate(ctx, pool, o.DatabaseURL); err != nil {
		pool.Close()
		return nil, err
	}
	s, err := Build(ctx, pool, o)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// WaitForPostgres connects and pings until Postgres answers or wait
// (default 60 s) runs out, so the server can start alongside its database.
func WaitForPostgres(ctx context.Context, dsn string, wait time.Duration, log *slog.Logger) (*pgxpool.Pool, error) {
	if wait <= 0 {
		wait = 60 * time.Second
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	deadline := time.Now().Add(wait)
	backoff := 250 * time.Millisecond
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("postgres not reachable after %s: %w", wait, err)
		}
		log.Info("waiting for postgres", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
}

// Migrate brings the tracker schema up to date. Adopt is a no-op on a
// standalone database (it only stamps fraud tables a host created).
func Migrate(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	if _, err := migrations.Adopt(ctx, pool, migrations.AdoptOptions{}); err != nil {
		return err
	}
	return migrations.Migrate(dsn)
}

// Build wires every component over an already-migrated pool.
func Build(ctx context.Context, pool *pgxpool.Pool, o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	st := store.New(pool)
	if _, err := st.EnsureInstance(ctx); err != nil {
		return nil, err
	}
	svc := localauth.NewService(pool)
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	var cfg *settings.Service
	if o.Settings == nil {
		var err error
		if cfg, err = settings.Open(ctx, pool, settings.Options{LookupEnv: o.LookupEnv, DataDir: o.DataDir, Log: o.Log}); err != nil {
			return nil, err
		}
		o.Settings = cfg
		if o.ProcessTimezone {
			// Before Serve starts the scheduler or any handler.
			time.Local = cfg.Timezone()
		}
	}
	storedSource := ""
	if cfg != nil {
		storedSource = cfg.SourceURL()
	}
	sourceURL, err := version.ResolveSourceURL(o.LookupEnv, storedSource)
	if err != nil {
		return nil, err
	}
	name := o.OperatorName
	if name == "" && cfg != nil {
		name = cfg.SiteName()
	}
	if name == "" {
		name = sitelayout.DefaultName
	}
	var branding sitelayout.BrandingSource = sitelayout.StaticBranding{Name: name}
	if cfg != nil && o.OperatorName == "" {
		branding = cfg // live: a rename in Settings shows on the next page
	}
	var lay *sitelayout.Layout
	if lay, err = sitelayout.New(sitelayout.Options{Auth: localauth.Authorizer(), CSRFToken: localauth.CSRFToken,
		Branding: branding, SourceURL: sourceURL, Log: o.Log,
		// The handoff freeze banner on every page (tracker, Settings,
		// Users, Ingestion, handoff, sign-in).
		Banner: func(r *http.Request) *host.Banner {
			state, err := st.GetInstanceState(r.Context())
			if err != nil {
				o.Log.Warn("instance state for banner", "err", err)
				return nil
			}
			return web.InstanceBanner(state)
		}}); err != nil {
		return nil, err
	}
	layout := o.Layout
	if layout == nil {
		layout = lay
	}
	cookies := localauth.CookieSecureAuto
	auth, err := localauth.New(localauth.Options{Service: svc, Layout: layout, Log: o.Log,
		Cookies: cookies, TrustProxy: o.TrustProxy})
	if err != nil {
		return nil, err
	}
	authz := auth.Authorizer()
	audit := accesslog.Auditor{Pool: pool}

	wh, err := web.New(web.Deps{Store: st, Auth: authz, Audit: audit, Layout: layout, Log: o.Log})
	if err != nil {
		return nil, err
	}
	rd, err := reader.New(reader.Options{Auth: authz, Layout: layout, Log: o.Log, ChartScript: sitelayout.ChartScript})
	if err != nil {
		return nil, err
	}

	run := o.Run
	if run == nil {
		run = scheduler.CLIRunner(CLIDeps(pool, o.Settings, o.Log))
	}
	sched, err := scheduler.New(scheduler.Config{
		Pool: pool, Jobs: o.Jobs, Run: run, Settings: o.Settings, Now: o.Now, Log: o.Log,
		Paused:           st.SchedulerPaused, // operator pause, or frozen/handed off
		ConnectorEnabled: st.ConnectorEnabled,
	})
	if err != nil {
		return nil, err
	}

	hand, err := newHandoffPage(st, layout, audit, o.Log)
	if err != nil {
		return nil, err
	}
	backups := newBackupRunner(pool, settings.ResolveDataDir(o.LookupEnv, o.DataDir), o.Log)
	ing, err := newIngestionPage(st, sched, layout, audit, backups, o.Log)
	if err != nil {
		return nil, err
	}
	var setPage *settingsPage
	if cfg != nil {
		if setPage, err = newSettingsPage(cfg, st, layout, audit, o.Log); err != nil {
			return nil, err
		}
	}
	bkPage, err := newBackupsPage(backups, layout, audit, o.Log)
	if err != nil {
		return nil, err
	}

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	// The wizard's "Start from an export" sits outside SetupGate (which
	// sends everything else to /setup); it checks setup state itself.
	maxImport := o.MaxImportBytes
	if maxImport <= 0 {
		maxImport = DefaultMaxImportBytes
	}
	si := &setupImport{pool: pool, svc: svc, layout: layout, dsn: o.DatabaseURL, max: maxImport, log: o.Log}
	if cfg != nil {
		si.settings = cfg // the wizard import restores the bundle's settings too
	}
	r.Group(func(r chi.Router) {
		r.Use(si.limitBody, auth.CSRF)
		r.Post(SetupImportPath, si.ServeHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.CSRF, auth.Session, auth.SetupGate, frozenWritesBlocked(st, o.Log))
		auth.Routes(r)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, reader.Base, http.StatusSeeOther)
		})
		rd.Mount(r, readmodel.New(readmodel.Options{Store: st, Auth: authz, Audit: audit, Log: o.Log,
			// Per request, from the live branding: a rename in Settings also
			// renames the operator in the suppression notice, no restart.
			NoticeFunc: func(r *http.Request) string {
				return readmodel.NoticeFor(branding.Branding(r.Context()).Name)
			},
			NamingEnabled: namingSwitch(o.Settings)}))
		r.Mount("/admin/fraud", wh.Admin)
		r.Route(IngestionPath, ing.routes)
		r.Route(HandoffPath, hand.routes)
		if setPage != nil {
			r.Route(SettingsPath, setPage.routes)
		}
		r.Route(BackupsPath, bkPage.routes)
		r.Handle(web.StaticPrefix+"*", wh.Static)
		r.Get("/about", lay.About)
		r.Handle(sitelayout.StaticPrefix+"*", sitelayout.StaticHandler())
	})

	return &Server{Pool: pool, Store: st, Scheduler: sched, Handler: r, Accounts: svc, SettingsStore: cfg, Backups: backups, opts: o}, nil
}

// llmSource is implemented by settings that build an LLM client.
type llmSource interface{ LLM() llm.Completer }

// CLIDeps is the tracker CLI wired to a standalone database: the jobs the
// scheduler runs, `job run` and `export`. The LLM comes from the settings
// (read per run, so a key saved in Settings applies to the next job), and
// so does the news corpus: the feeds in Settings, polled by classify-news.
func CLIDeps(pool *pgxpool.Pool, settings host.Settings, log *slog.Logger) cli.Deps {
	var llmFn func() llm.Completer
	if ls, ok := settings.(llmSource); ok {
		llmFn = ls.LLM
	}
	secrets, _ := settings.(bundle.SecretOpener)
	var backupFn func(context.Context, string) (string, error)
	if ds, ok := settings.(dataDirSource); ok {
		runner := newBackupRunner(pool, ds.DataDir(), log)
		backupFn = func(ctx context.Context, kind string) (string, error) {
			b, err := runner.Run(ctx, kind, "")
			return b.Name, err
		}
	}
	return cli.Deps{
		LLM:     llmFn,
		Secrets: secrets,
		Backup:  backupFn,
		Open: func(context.Context) (*cli.Conn, error) {
			// The pool is shared with the server; the CLI must not close it.
			news, err := newsSource(pool, settings, log)
			if err != nil {
				return nil, err
			}
			return &cli.Conn{Store: store.New(pool), News: news}, nil
		},
		Settings:   settings,
		Log:        log,
		SourceKind: bundle.KindStandalone,
	}
}

// namingSwitch is the instance's naming switch (settings.Service.NamingOn):
// off until an admin turns it on. Settings without one (a test's bare
// host.Settings) leave naming to the per-case rule.
func namingSwitch(s host.Settings) func(context.Context) bool {
	if ns, ok := s.(interface{ NamingOn(context.Context) bool }); ok {
		return ns.NamingOn
	}
	return nil
}

// dataDirSource is the settings store's data volume (settings.Service);
// backups go under it.
type dataDirSource interface{ DataDir() string }

// feedSettings is the settings store's news-feed list (settings.Service).
type feedSettings interface {
	NewsFeeds() ([]newsfeed.Feed, error)
}

// newsSource is standalone's classify-news corpus: the operator's feeds,
// read at the start of each run so an edit applies to the next one. With
// no feed list (a test's bare settings) it still reads what is stored.
func newsSource(pool *pgxpool.Pool, settings host.Settings, log *slog.Logger) (*newsfeed.Source, error) {
	var cr host.Crawler
	if cs, ok := settings.(host.CrawlerSettings); ok {
		cr = cs.Crawler()
	}
	src := &newsfeed.Source{Pool: pool, Fetcher: fraudagency.NewFetcherFor(cr), Log: log}
	if fs, ok := settings.(feedSettings); ok {
		feeds, err := fs.NewsFeeds()
		if err != nil {
			return nil, err
		}
		src.Feeds = feeds
	}
	return src, nil
}

// Serve runs the HTTP server and the scheduler until ctx is cancelled, then
// shuts both down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	ln := s.opts.Listener
	if ln == nil {
		addr := s.opts.Listen
		if addr == "" {
			addr = ":8080"
		}
		var err error
		if ln, err = net.Listen("tcp", addr); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: s.Handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		s.Scheduler.Start(ctx)
	}()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.opts.Log.Info("corruption-tracker serving", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		stop()
		<-schedDone
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	<-schedDone // running jobs see the cancelled ctx and finish
	return err
}
