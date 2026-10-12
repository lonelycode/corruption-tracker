// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package migrations

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// Latest is the newest migration version this binary carries. An importer
// refuses bundles from a newer schema (design §10.6).
func Latest() (uint, error) {
	entries, err := fs.ReadDir(sqlFS, "sql")
	if err != nil {
		return 0, err
	}
	var latest uint
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(num, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if uint(v) > latest {
			latest = uint(v)
		}
	}
	return latest, nil
}
