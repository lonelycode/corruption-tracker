// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/backup"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/settings"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
	"github.com/lonelycode/corruption-tracker/web"
)

// BackupsPath is the admin-only Backups page.
const BackupsPath = "/admin/backups"

// backupTimeout bounds a "Back up now" from the page.
const backupTimeout = 30 * time.Minute

// newBackupRunner is the instance's backup runner: backups go under the
// data volume, pg_dump connects with the pool's own URL.
func newBackupRunner(pool *pgxpool.Pool, dataDir string, log *slog.Logger) *backup.Runner {
	return backup.New(backup.Options{
		Pool: pool, DSN: pool.Config().ConnString(),
		Dir: filepath.Join(dataDir, backup.DirName), Log: log,
	})
}

// PreMigrationBackup takes the pre-migrate backup when this binary is
// about to migrate an existing tracker to a newer schema (serve and
// migrate both call it before Migrate).
func PreMigrationBackup(ctx context.Context, pool *pgxpool.Pool, o Options) error {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	dsn := o.DatabaseURL
	if dsn == "" {
		dsn = pool.Config().ConnString()
	}
	r := newBackupRunner(pool, settings.ResolveDataDir(o.LookupEnv, o.DataDir), o.Log)
	b, err := r.BeforeMigrate(ctx, dsn, o.LookupEnv)
	if err != nil {
		return err
	}
	if b != nil {
		o.Log.Info("pre-migration backup taken", "name", b.Name, "note", b.Note)
	}
	return nil
}

type backupsPage struct {
	runner *backup.Runner
	layout host.Layout
	audit  host.AccessAuditor
	log    *slog.Logger
	tmpl   *template.Template
}

func newBackupsPage(runner *backup.Runner, layout host.Layout, audit host.AccessAuditor, log *slog.Logger) (*backupsPage, error) {
	t, err := template.New("backups.html.tmpl").Funcs(template.FuncMap{"bytes": humanBytes}).
		ParseFS(templateFS, "templates/backups.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &backupsPage{runner: runner, layout: layout, audit: audit, log: log, tmpl: t}, nil
}

func (p *backupsPage) routes(r chi.Router) {
	r.Use(requireAdmin)
	r.Get("/", p.show)
	r.Post("/run", p.run)
	r.Get("/{name}/{file}", p.download)
}

func (p *backupsPage) show(w http.ResponseWriter, r *http.Request) {
	all, err := p.runner.List()
	if err != nil {
		p.log.Error("list backups", "err", err)
	}
	st, _ := p.runner.Status()
	var body bytes.Buffer
	if err := p.tmpl.Execute(&body, struct {
		Backups    []backup.Backup
		Status     backup.Status
		StaleHours int
		CSRF       string
		Dir        string
	}{all, st, int(backup.StaleAfter.Hours()), p.layout.CSRFToken(r), p.runner.Dir()}); err != nil {
		p.log.Error("render backups page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p.layout.Render(w, r, host.Page{
		Name: "admin_backups", Title: "Backups", Section: "admin",
		Head: template.HTML(`<link rel="stylesheet" href="` + web.StaticPrefix + `tracker.css">`),
		Body: template.HTML(body.String()),
	})
}

func (p *backupsPage) run(w http.ResponseWriter, r *http.Request) {
	u, _ := localauth.CurrentUser(r)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), backupTimeout)
	defer cancel()
	b, err := p.runner.Run(ctx, backup.KindManual, "taken by "+u.Username)
	detail := b.Name
	if err != nil {
		detail = "failed"
		p.log.Error("manual backup", "err", err)
		p.layout.SetFlash(w, "error", "Backup failed: "+err.Error())
	} else {
		p.layout.SetFlash(w, "success", "Backup "+b.Name+" written.")
	}
	if aerr := p.audit.LogAccess(r.Context(), u.Principal(), "admin", "backup_run", detail); aerr != nil {
		p.log.Error("audit backup run", "err", aerr)
	}
	http.Redirect(w, r, BackupsPath, http.StatusSeeOther)
}

// download serves one file of a backup. Only names and files the backup's
// own manifest lists are served, and every download is audited: a backup
// holds the whole dataset.
func (p *backupsPage) download(w http.ResponseWriter, r *http.Request) {
	name, file := chi.URLParam(r, "name"), chi.URLParam(r, "file")
	f, _, err := p.runner.Open(name, file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	u, _ := localauth.CurrentUser(r)
	if aerr := p.audit.LogAccess(r.Context(), u.Principal(), "admin", "backup_download", name+"/"+file); aerr != nil {
		p.log.Error("audit backup download", "err", aerr)
		http.Error(w, "audit unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+"-"+file+`"`)
	if info, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	if _, err := io.Copy(w, f); err != nil {
		p.log.Warn("backup download interrupted", "name", name, "file", file, "err", err)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}
