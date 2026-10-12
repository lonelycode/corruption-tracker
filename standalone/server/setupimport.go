// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
)

// SetupImportPath is the setup wizard's "Start from an export" upload. It
// works only on a fresh instance: before the first account exists (the
// import itself refuses a non-empty dataset).
const SetupImportPath = "/setup/import"

// DefaultMaxImportBytes caps a setup-wizard bundle upload.
const DefaultMaxImportBytes = 2 << 30

type setupImport struct {
	pool   *pgxpool.Pool
	svc    *localauth.Service
	layout host.Layout
	dsn    string
	max    int64
	log    *slog.Logger
	// settings receives the bundle's settings (nil when the server was
	// given its own host.Settings rather than the settings store).
	settings bundle.SettingsTarget
	mu       sync.Mutex // one import at a time
}

// limitBody caps the request body before anything (the CSRF check
// included) parses the multipart form.
func (si *setupImport) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, si.max)
		next.ServeHTTP(w, r)
	})
}

func (si *setupImport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	needed, err := si.svc.SetupNeeded(ctx)
	if err != nil {
		si.log.Error("setup import: setup check", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !needed {
		http.NotFound(w, r) // only a fresh instance can start from an export
		return
	}
	if !si.mu.TryLock() {
		si.fail(w, r, errors.New("another import is already running"))
		return
	}
	defer si.mu.Unlock()

	file, _, err := r.FormFile("bundle")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = fmt.Errorf("the bundle is larger than %d MiB; import it with `corruption-tracker import` instead", si.max>>20)
		} else {
			err = errors.New("choose an export bundle (.tar.gz) to upload")
		}
		si.fail(w, r, err)
		return
	}
	defer file.Close()
	tmp, err := os.CreateTemp("", "corruption-tracker-setup-import-*.tar.gz")
	if err != nil {
		si.fail(w, r, err)
		return
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, file)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		si.fail(w, r, fmt.Errorf("receive upload: %w", err))
		return
	}

	rep, err := bundle.ImportFile(ctx, si.pool, tmp.Name(), bundle.ImportOptions{DSN: si.dsn,
		Settings: si.settings, Passphrase: r.FormValue("passphrase"), Log: si.log})
	if err != nil {
		si.fail(w, r, err)
		return
	}
	si.log.Info("setup wizard import", "export_id", rep.ExportID, "tables", len(rep.Tables),
		"settings", rep.SettingsImported, "secrets", rep.SecretsImported)
	msg := fmt.Sprintf("Imported export %s: %d tables verified, %d settings and %d secrets. Next, create the "+
		"administrator below. People in the imported history can then be claimed on the Users page. "+
		"Ingestion is paused until you resume it.",
		rep.ExportID, len(rep.Tables), rep.SettingsImported, rep.SecretsImported)
	// Say plainly what happened to the bundle's stored secrets (a wrong
	// passphrase never gets here: it fails the whole import).
	switch {
	case rep.SecretsImported > 0:
		msg += fmt.Sprintf(" Stored secrets restored from the bundle: %d. Check them with Test LLM and Test mailbox on the Settings page.",
			rep.SecretsImported)
		if len(rep.SecretsSkipped) > 0 {
			msg += " Not recognised by this version, so not restored: " + strings.Join(rep.SecretsSkipped, ", ") + "."
		}
	case rep.SecretsNotImported != "":
		msg += " The bundle holds encrypted secrets, but no passphrase was entered, so they were not restored: " +
			"enter the LLM key and mailbox password on the Settings page."
	case len(rep.SecretsSkipped) > 0:
		msg += " The bundle holds stored secrets, but this version doesn't recognise them (" +
			strings.Join(rep.SecretsSkipped, ", ") + "), so none were restored: " +
			"enter the LLM key and mailbox password on the Settings page."
	default:
		msg += " The bundle carries no stored secrets: enter the LLM key and mailbox password on the Settings page."
	}
	si.layout.SetFlash(w, "success", msg)
	http.Redirect(w, r, "/setup?imported="+url.QueryEscape(rep.ExportID), http.StatusSeeOther)
}

func (si *setupImport) fail(w http.ResponseWriter, r *http.Request, err error) {
	si.log.Warn("setup wizard import failed", "err", err)
	si.layout.SetFlash(w, "error", "Import failed: "+err.Error())
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}
