// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// nonTableRefs are identifiers the scan below finds after FROM/JOIN/INTO/
// UPDATE/TABLE in SQL literals that are not tables: CTE and subquery names,
// functions, keywords, and words from SQL-looking error messages.
var nonTableRefs = map[string]string{
	"anchored": "CTE", "grp": "CTE", "latest": "CTE", "parts": "CTE", "pp": "CTE",
	"sec": "CTE", "srcs": "CTE",
	"coalesce": "function", "unnest": "function", "json_populate_recordset": "function", "lateral": "keyword", "set": "keyword (ON CONFLICT DO UPDATE SET)",
	"case": "error text", "checks": "error text", "fraud": "error text", "participant": "error text", "status": "error text",
}

// catalogRefs are Postgres catalogs the migrations, exporter and test
// harness read.
var catalogRefs = regexp.MustCompile(`^(pg_[a-z_]+|information_schema\.[a-z_]+)$`)

// Every piece of state the tracker reads or writes lives in its own tables
// (fraud_*, tracker_*), so an export bundle carries all of it: no tracker
// SQL may name another application's table. This scans every SQL string
// literal in the module's non-test code. A new identifier fails the test
// until it is either a tracker table or listed above with a reason.
func TestModuleSQLTouchesOnlyTrackerTables(t *testing.T) {
	sqlish := regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE|CREATE|ALTER)\b`)
	ref := regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE|TABLE)\s+([a-z_][a-z0-9_.]*)`)
	found := map[string][]string{}
	root, _ := filepath.Abs("..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil || !sqlish.MatchString(s) {
				return true
			}
			for _, m := range ref.FindAllStringSubmatch(s, -1) {
				name := strings.ToLower(m[2])
				rel, _ := filepath.Rel(root, p)
				found[name] = append(found[name], rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "fraud_"), strings.HasPrefix(n, "tracker_"), catalogRefs.MatchString(n):
		case nonTableRefs[n] != "":
		default:
			t.Errorf("tracker SQL references %q (in %s), which is not a tracker table; "+
				"tracker state must live in fraud_*/tracker_* tables so bundles carry it", n, found[n][0])
		}
	}
	for n := range nonTableRefs {
		if _, ok := found[n]; !ok {
			t.Logf("nonTableRefs entry %q no longer occurs; remove it", n)
		}
	}
}
