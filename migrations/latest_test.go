// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package migrations

import (
	"fmt"
	"io/fs"
	"testing"
)

func TestLatestIsTheHighestEmbeddedMigration(t *testing.T) {
	v, err := Latest()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := fs.ReadDir(sqlFS, "sql") // sorted by name
	last := entries[len(entries)-1].Name()
	if got := fmt.Sprintf("%04d", v); got != last[:4] {
		t.Fatalf("Latest() = %d, newest migration file %s", v, last)
	}
}
