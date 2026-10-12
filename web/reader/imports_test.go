// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package reader

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Presentation never touches storage or entity types (design §5.9.1): the
// reader may only see what the read model hands it, already redacted.
func TestReaderImportsOnlyTheReadModel(t *testing.T) {
	banned := []string{"/corruption-tracker/store", "/corruption-tracker/domain", "github.com/jackc/pgx"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, b := range banned {
				if strings.Contains(path, b) {
					t.Errorf("%s imports %s: reader surfaces must go through readmodel", f, path)
				}
			}
		}
	}
}
