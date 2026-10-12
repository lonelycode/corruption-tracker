// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package backup

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lonelycode/corruption-tracker/migrations"
)

// SkipEnv, set to 1, lets a tracker migrate without the pre-migration
// backup (e.g. the data volume is full). The server logs loudly when it is
// used.
const SkipEnv = "TRACKER_SKIP_PREMIGRATION_BACKUP"

// BeforeMigrate takes a pre-migrate backup when the database holds a
// tracker at an older schema than this binary's, which means an upgrade is
// about to migrate it. A fresh database (schema 0) needs none. A failed
// backup stops the upgrade: migrations are forward-only, so this backup is
// the way back.
func (r *Runner) BeforeMigrate(ctx context.Context, dsn string, lookupEnv func(string) (string, bool)) (*Backup, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	current, _, err := migrations.Version(dsn)
	if err != nil {
		return nil, err
	}
	latest, err := migrations.Latest()
	if err != nil {
		return nil, err
	}
	if current == 0 || current >= latest {
		return nil, nil
	}
	if v, _ := lookupEnv(SkipEnv); strings.TrimSpace(v) == "1" {
		r.o.Log.Warn("migrating WITHOUT a pre-migration backup", "env", SkipEnv, "from", current, "to", latest)
		return nil, nil
	}
	b, err := r.Run(ctx, KindPreMigrate, fmt.Sprintf("before migrating schema %d to %d", current, latest))
	if err != nil {
		return nil, fmt.Errorf("pre-migration backup failed, so the schema was not migrated (%d to %d): %w; "+
			"fix the cause, or set %s=1 to migrate without one", current, latest, err, SkipEnv)
	}
	return &b, nil
}
