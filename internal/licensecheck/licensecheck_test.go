// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package licensecheck

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// enforce turns missing headers into failures (on since the headers were
// stamped across the module).
const enforce = true

// headerLines is how far into a file the headers may sit (after a shebang,
// a build constraint or a template's opening comment).
const headerLines = 10

var (
	licenceRE   = regexp.MustCompile(`SPDX-License-Identifier:\s*(\S+)`)
	copyrightRE = regexp.MustCompile(`SPDX-FileCopyrightText:\s*\S`)
	// Either variant is acceptable here; which one ships is decided in .28.
	allowedLicence = regexp.MustCompile(`^AGPL-3\.0-(only|or-later)$`)
)

// vendored maps third-party directories (module-relative, trailing slash)
// to the licences they ship under. Their files keep their upstream
// licence and must never get an AGPL header; each directory must carry its
// upstream LICENSE file instead. Every licence here must be AGPL-3.0
// compatible.
var vendored = map[string]string{
	"standalone/layout/static/vendor/": "Apache-2.0 (Apache ECharts) + BSD-3-Clause (d3, embedded)",
}

func vendoredDir(rel string) (string, bool) {
	for dir := range vendored {
		if strings.HasPrefix(rel, dir) {
			return dir, true
		}
	}
	return "", false
}

// needsHeader reports whether a file is source that must carry inline SPDX
// headers. Data files (*.json schemas, fixtures) are covered by REUSE.toml
// instead; LICENSE and go.mod/go.sum carry none.
func needsHeader(rel string) bool {
	base := filepath.Base(rel)
	if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") {
		return true
	}
	switch filepath.Ext(rel) {
	case ".go", ".sql", ".tmpl", ".css", ".js", ".py", ".sh", ".yml", ".yaml":
		return true
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestSPDXHeaders(t *testing.T) {
	root := moduleRoot(t)
	var missing, wrong []string
	total := 0
	vendoredPkgs := map[string]bool{} // vendor/<pkg>/ dirs seen
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if dir, ok := vendoredDir(rel); ok {
			if rest := strings.TrimPrefix(rel, dir); strings.Contains(rest, "/") {
				vendoredPkgs[dir+rest[:strings.Index(rest, "/")+1]] = true
			}
			return nil
		}
		if !needsHeader(rel) {
			return nil
		}
		total++
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		var licence string
		var copyright bool
		sc := bufio.NewScanner(f)
		for n := 0; n < headerLines && sc.Scan(); n++ {
			if m := licenceRE.FindStringSubmatch(sc.Text()); m != nil && licence == "" {
				licence = strings.TrimRight(m[1], "*/}-> ")
			}
			if copyrightRE.MatchString(sc.Text()) {
				copyright = true
			}
		}
		switch {
		case licence == "" || !copyright:
			missing = append(missing, rel)
		case !allowedLicence.MatchString(licence):
			wrong = append(wrong, rel+" ("+licence+")")
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(missing)
	sort.Strings(wrong)

	// Vendored packages are exempt from headers but must ship their licence.
	for pkg := range vendoredPkgs {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(pkg), "LICENSE")); err != nil {
			t.Errorf("vendored package %s has no upstream LICENSE file", pkg)
		}
	}

	report := t.Logf
	if enforce {
		report = t.Errorf
	}
	for _, w := range wrong {
		// A non-AGPL identifier is always an error: it would misstate the
		// licence of a file in an AGPL repository.
		t.Errorf("%s: SPDX-License-Identifier is not AGPL-3.0", w)
	}
	for _, m := range missing {
		report("%s: missing SPDX-License-Identifier and/or SPDX-FileCopyrightText in the first %d lines", m, headerLines)
	}
	t.Logf("SPDX headers: %d of %d source files missing (enforce=%v)", len(missing), total, enforce)
}
