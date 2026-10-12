// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package cicheck tests the scripts the CI workflows rely on.
package cicheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goTestLog runs `go test -v` over a throwaway module holding src as its
// only test file and returns the log, so the check is tested against the
// real output format rather than a hand-written imitation of it.
func goTestLog(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module example.com/skips\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "x_test.go"), src)
	cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	out, _ := cmd.CombinedOutput() // a failing test run is still a log to check
	log := filepath.Join(dir, "test.log")
	write(t, log, string(out))
	return log
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noSkips(t *testing.T, log string) (string, error) {
	t.Helper()
	out, err := exec.Command("bash", "../../scripts/ci-no-skips.sh", log).CombinedOutput()
	return string(out), err
}

func TestNoSkipsFailsOnSkippedTests(t *testing.T) {
	for name, src := range map[string]string{
		// The case the check exists for: storetest skips without a database.
		"database skip": `package skips
import ("os"; "testing")
func TestStore(t *testing.T) {
	if os.Getenv("NO_SUCH_DATABASE_URL") == "" { t.Skip("DATABASE_URL_TEST not set; skipping DB test") }
}
func TestPure(t *testing.T) {}
`,
		"subtest skip": `package skips
import "testing"
func TestTable(t *testing.T) {
	t.Run("ok", func(t *testing.T) {})
	t.Run("skipped", func(t *testing.T) { t.Skip("no fixture") })
}
`,
		"skip without a reason": `package skips
import "testing"
func TestBare(t *testing.T) { t.SkipNow() }
`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := noSkips(t, goTestLog(t, src))
			if err == nil {
				t.Fatalf("a skipped test passed the check:\n%s", out)
			}
			if !strings.Contains(out, "skipped") {
				t.Errorf("no explanation in:\n%s", out)
			}
		})
	}
}

func TestNoSkipsNamesTheSkippedTestAndReason(t *testing.T) {
	out, _ := noSkips(t, goTestLog(t, `package skips
import "testing"
func TestStore(t *testing.T) { t.Skip("DATABASE_URL_TEST not set") }
`))
	for _, want := range []string{"TestStore", "DATABASE_URL_TEST not set"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestNoSkipsPassesWhenNothingSkipped(t *testing.T) {
	out, err := noSkips(t, goTestLog(t, `package skips
import "testing"
func TestA(t *testing.T) {}
func TestB(t *testing.T) { t.Run("sub", func(t *testing.T) {}) }
`))
	if err != nil {
		t.Fatalf("clean run failed the check: %v\n%s", err, out)
	}
}

// An empty or non-verbose log must not pass for a clean one.
func TestNoSkipsFailsWithoutAVerboseRun(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"empty":       "",
		"not verbose": "ok  \texample.com/skips\t0.01s\n",
	} {
		log := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".log")
		write(t, log, body)
		if out, err := noSkips(t, log); err == nil {
			t.Errorf("%s log passed the check:\n%s", name, out)
		}
	}
}
