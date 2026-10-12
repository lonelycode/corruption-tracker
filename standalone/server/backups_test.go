// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/backup"
	"github.com/lonelycode/corruption-tracker/standalone/localauth"
)

// fakePGDump puts a pg_dump stand-in first on PATH (the real one is
// exercised against PostgreSQL 16 by scripts/compose-upgrade-test.sh).
func fakePGDump(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --file=*) echo PGDMP > \"${a#--file=}\";; esac; done\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_dump"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The Backups page: admin only, says when there is no backup, takes one
// on demand, serves only the backup's own files, and audits downloads.
func TestBackupsPage(t *testing.T) {
	fakePGDump(t)
	h := newHarness(t)
	ctx := context.Background()

	resp, page := h.b.get("/admin/backups/")
	if resp.StatusCode != 200 || !strings.Contains(page, "No backups yet") || !strings.Contains(page, `href="/admin/backups"`) {
		t.Fatalf("backups page = %d\n%s", resp.StatusCode, page)
	}
	// The Ingestion status page carries the same warning (.56).
	if _, ing := h.b.get("/admin/ingestion/"); !strings.Contains(ing, "No backups yet") || !strings.Contains(ing, `<a href="/admin/backups">Backups</a>`) {
		t.Errorf("ingestion page has no backup line:\n%s", ing)
	}

	if resp, _ := h.b.post("/admin/backups/", "/admin/backups/run", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("back up now = %d", resp.StatusCode)
	}
	all, err := h.s.Backups.List()
	if err != nil || len(all) != 1 || all[0].Kind != backup.KindManual {
		t.Fatalf("backups after Back up now = %+v, %v", all, err)
	}
	b := all[0]
	_, page = h.b.get("/admin/backups/")
	if !strings.Contains(page, "Backup "+b.Name+" written.") || !strings.Contains(page, "/admin/backups/"+b.Name+"/"+backup.DumpFile) ||
		strings.Contains(page, "No backups yet") {
		t.Errorf("page after a backup:\n%s", page)
	}

	if _, ing := h.b.get("/admin/ingestion/"); !strings.Contains(ing, "Last backup: ") || !strings.Contains(ing, "(manual)") ||
		strings.Contains(ing, "No backups yet") || strings.Contains(ing, "hours ago") {
		t.Errorf("ingestion page after a backup:\n%s", ing)
	}

	resp, body := h.b.get("/admin/backups/" + b.Name + "/" + backup.DumpFile)
	if resp.StatusCode != 200 || body != "PGDMP\n" || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("download = %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Disposition"))
	}
	var n int
	if err := h.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_access_log WHERE surface = 'backup_download' AND detail = $1`,
		b.Name+"/"+backup.DumpFile).Scan(&n); err != nil || n != 1 {
		t.Errorf("download audit rows = %d, %v", n, err)
	}
	if err := h.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_access_log WHERE surface = 'backup_run'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("run audit rows = %d, %v", n, err)
	}
	for _, p := range []string{
		"/admin/backups/" + b.Name + "/..%2Fbackup.json", "/admin/backups/" + b.Name + "/secret.key",
		"/admin/backups/..%2F..%2Fsecret.key/x", "/admin/backups/20261012T030000Z-other/" + backup.DumpFile,
	} {
		if resp, _ := h.b.get(p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, resp.StatusCode)
		}
	}

	admin, err := h.s.Accounts.UserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Accounts.CreateUser(ctx, admin, localauth.NewUser{Username: "rev",
		Password: adminPassword, Role: localauth.RoleReviewer, Note: "test"}); err != nil {
		t.Fatal(err)
	}
	rev := newBrowser(t, h.b.base)
	rev.post("/login", "/login", url.Values{"username": {"rev"}, "password": {adminPassword}})
	for _, p := range []string{"/admin/backups/", "/admin/backups/" + b.Name + "/" + backup.DumpFile} {
		if resp, _ := rev.get(p); resp.StatusCode != http.StatusForbidden {
			t.Errorf("reviewer GET %s = %d, want 403", p, resp.StatusCode)
		}
	}
	if resp, _ := rev.post("/admin/fraud/", "/admin/backups/run", url.Values{}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("reviewer back up now = %d, want 403", resp.StatusCode)
	}
}
