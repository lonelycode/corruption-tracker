// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package prepublish

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// denied is content that must never appear in the published tree. Operator-
// and infrastructure-specific names are deliberately NOT listed here (the
// list itself would publish them); an embedding host can keep its own list
// outside this repository and scan the module tree with it.
var denied = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private IPv4 address", regexp.MustCompile(`\b(10\.\d{1,3}|192\.168|172\.(1[6-9]|2\d|3[01])|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7]))\.\d{1,3}\.\d{1,3}\b`)},
	{"Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`)},
	{"OpenAI-style API key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// emailRE finds email addresses; each must be on an allowed domain.
var emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)

// allowedEmailDomain reports whether an address domain may be published:
// reserved test domains, plus the public sender addresses of the alert
// services the mail connector parses (they appear in synthetic fixtures).
func allowedEmailDomain(domain string) bool {
	d := strings.ToLower(domain)
	for _, suffix := range []string{".example", ".test", ".invalid", ".localhost"} {
		if strings.HasSuffix(d, suffix) {
			return true
		}
	}
	switch d {
	case "example.org", "example.com", "example.net",
		"courtsofnz.govt.nz", "capitalletter.co.nz":
		return true
	}
	return false
}

// skipped files are verbatim third-party text or generated.
var skipped = map[string]bool{
	"LICENSE":                                true,
	"go.sum":                                 true,
	"internal/prepublish/prepublish_test.go": true, // the patterns themselves
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestNoDeniedContent(t *testing.T) {
	root := moduleRoot(t)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if skipped[rel] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			for _, p := range denied {
				if p.re.MatchString(line) {
					hits = append(hits, rel+":"+strconv.Itoa(n)+": "+p.name+": "+strings.TrimSpace(line))
				}
			}
			for _, m := range emailRE.FindAllStringSubmatch(line, -1) {
				addr := m[0]
				domain := addr[strings.LastIndex(addr, "@")+1:]
				if !allowedEmailDomain(domain) {
					hits = append(hits, rel+":"+strconv.Itoa(n)+": email on a non-reserved domain: "+addr)
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Error(h)
	}
	if len(hits) > 0 {
		t.Log("Use reserved names in fixtures (example.org, *.test, *.invalid, TEST-NET 192.0.2.0/24).")
	}
}

// vettedLicences maps every third-party module that may be linked into a
// module binary or test to its licence. All are AGPL-3.0 compatible
// (MPL-2.0 via its secondary-licence clause). A new dependency fails the
// test until someone checks its licence and adds it here.
var vettedLicences = map[string]string{
	"github.com/PuerkitoBio/goquery":         "BSD-3-Clause",
	"github.com/andybalholm/cascadia":        "BSD-2-Clause",
	"github.com/anthropics/anthropic-sdk-go": "MIT",
	"github.com/bahlo/generic-list-go":       "BSD-3-Clause",
	"github.com/buger/jsonparser":            "MIT",
	"github.com/emersion/go-imap":            "MIT",
	"github.com/emersion/go-message":         "MIT",
	"github.com/emersion/go-sasl":            "MIT",
	"github.com/emersion/go-textwrapper":     "MIT",
	"github.com/go-chi/chi/v5":               "MIT",
	"github.com/golang-migrate/migrate/v4":   "MIT",
	"github.com/hashicorp/errwrap":           "MPL-2.0",
	"github.com/hashicorp/go-multierror":     "MPL-2.0",
	"github.com/invopop/jsonschema":          "MIT",
	"github.com/jackc/pgerrcode":             "MIT",
	"github.com/jackc/pgpassfile":            "MIT",
	"github.com/jackc/pgservicefile":         "MIT",
	"github.com/jackc/pgx/v5":                "MIT",
	"github.com/jackc/puddle/v2":             "MIT",
	"github.com/mailru/easyjson":             "MIT",
	"github.com/tidwall/gjson":               "MIT",
	"github.com/tidwall/match":               "MIT",
	"github.com/tidwall/pretty":              "MIT",
	"github.com/tidwall/sjson":               "MIT",
	"github.com/wk8/go-ordered-map/v2":       "Apache-2.0",
	"go.uber.org/atomic":                     "MIT",
	"golang.org/x/crypto":                    "BSD-3-Clause",
	"golang.org/x/net":                       "BSD-3-Clause",
	"golang.org/x/sync":                      "BSD-3-Clause",
	"golang.org/x/sys":                       "BSD-3-Clause",
	"golang.org/x/text":                      "BSD-3-Clause",
	"gopkg.in/yaml.v3":                       "MIT AND Apache-2.0",
}

// licencePlatforms are the targets whose dependency sets the licence check
// covers: the image's platforms plus the host's. Dependencies differ by
// platform (golang.org/x/sys is linked on amd64 only), so a check that
// listed only the host's would pass on one machine and fail on another.
var licencePlatforms = []string{"linux/amd64", "linux/arm64", runtime.GOOS + "/" + runtime.GOARCH}

func TestDependencyLicences(t *testing.T) {
	root := moduleRoot(t)
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	var out []byte
	for _, platform := range licencePlatforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		cmd := exec.Command(goBin, "list", "-deps", "-test",
			"-f", "{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}", "./...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		o, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				t.Fatalf("go list (%s): %v\n%s", platform, err, ee.Stderr)
			}
			t.Fatalf("go list (%s): %v", platform, err)
		}
		out = append(out, o...)
	}
	seen := map[string]bool{}
	for _, mod := range strings.Fields(string(out)) {
		if seen[mod] {
			continue
		}
		seen[mod] = true
		if _, ok := vettedLicences[mod]; !ok {
			t.Errorf("dependency %s is not in vettedLicences: check its licence is AGPL-3.0 compatible (no SSPL, non-commercial or proprietary terms), then add it", mod)
		}
	}
	for mod := range vettedLicences {
		if !seen[mod] {
			t.Logf("vettedLicences entry %s is no longer linked; delete it", mod)
		}
	}
}
