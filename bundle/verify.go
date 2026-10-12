// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// maxManifestBytes bounds manifest.json so a hostile bundle can't make the
// verifier buffer an arbitrary amount before any check has run.
const maxManifestBytes = 16 << 20

// VerifyFile checks a bundle file offline; see Verify.
func VerifyFile(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	return Verify(f)
}

// Verify checks a bundle offline, without a database: the manifest is a
// supported v1 manifest that accounts for every table of the v1 set; the
// archive holds exactly the files it lists, in order, with matching sha256
// and (for tables) row counts. It reads the archive once, streaming.
func Verify(r io.Reader) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, fmt.Errorf("bundle: not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil {
		return m, fmt.Errorf("bundle: empty or unreadable archive: %w", err)
	}
	if hdr.Name != ManifestFile {
		return m, fmt.Errorf("bundle: first entry is %q, want %s", hdr.Name, ManifestFile)
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
	if err != nil {
		return m, fmt.Errorf("bundle: read manifest: %w", err)
	}
	if len(raw) > maxManifestBytes {
		return m, errors.New("bundle: manifest.json is too large")
	}
	if err := checkHeader(raw); err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("bundle: parse manifest: %w", err)
	}
	expected, err := checkManifest(m)
	if err != nil {
		return m, err
	}

	for i := 0; ; i++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if i < len(expected) {
				return m, fmt.Errorf("bundle: %s is missing (archive ends early)", expected[i].name)
			}
			return m, nil
		}
		if err != nil {
			return m, fmt.Errorf("bundle: read archive: %w", err)
		}
		if i >= len(expected) {
			return m, fmt.Errorf("bundle: unexpected entry %q", hdr.Name)
		}
		want := expected[i]
		if hdr.Name != want.name {
			if slices.ContainsFunc(expected, func(e expectedEntry) bool { return e.name == hdr.Name }) {
				return m, fmt.Errorf("bundle: %s is missing or out of order (found %s)", want.name, hdr.Name)
			}
			return m, fmt.Errorf("bundle: unexpected entry %q (want %s)", hdr.Name, want.name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return m, fmt.Errorf("bundle: %s is not a regular file", hdr.Name)
		}
		h := sha256.New()
		lines := &lineCounter{}
		if _, err := io.Copy(io.MultiWriter(h, lines), tr); err != nil {
			return m, fmt.Errorf("bundle: read %s: %w", hdr.Name, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want.sha256 {
			return m, fmt.Errorf("bundle: %s checksum mismatch (manifest %s, file %s)", hdr.Name, want.sha256, got)
		}
		if want.rows >= 0 {
			if lines.n > 0 && !lines.endsNL {
				return m, fmt.Errorf("bundle: %s does not end with a newline", hdr.Name)
			}
			if lines.lines() != want.rows {
				return m, fmt.Errorf("bundle: %s has %d rows, manifest says %d", hdr.Name, lines.lines(), want.rows)
			}
		}
	}
}

// checkHeader reads just format/format_version, so a newer bundle gets a
// clear "upgrade first" rather than an unknown-field error.
func checkHeader(raw []byte) error {
	var head struct {
		Format        string `json:"format"`
		FormatVersion int    `json:"format_version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("bundle: parse manifest: %w", err)
	}
	if head.Format != Format {
		return fmt.Errorf("bundle: not a corruption-tracker bundle (format %q)", head.Format)
	}
	if head.FormatVersion > FormatVersion {
		return fmt.Errorf("bundle: format_version %d is from a newer corruption-tracker; upgrade first", head.FormatVersion)
	}
	if head.FormatVersion < 1 {
		return fmt.Errorf("bundle: invalid format_version %d", head.FormatVersion)
	}
	return nil
}

type expectedEntry struct {
	name   string
	sha256 string
	rows   int64 // -1 for non-table files
}

// checkManifest validates the manifest's own consistency and returns the
// entries the archive must contain after manifest.json, in order.
func checkManifest(m Manifest) ([]expectedEntry, error) {
	if m.Source.Kind != KindAlitheiaEmbedded && m.Source.Kind != KindStandalone {
		return nil, fmt.Errorf("bundle: unknown source kind %q", m.Source.Kind)
	}
	if m.Settings.File != SettingsFile {
		return nil, fmt.Errorf("bundle: settings file is %q, want %s", m.Settings.File, SettingsFile)
	}
	allowed := append(slices.Clone(TableSet), AccessLogTable)
	accounted := map[string]bool{}
	expected := []expectedEntry{{name: SettingsFile, sha256: m.Settings.SHA256, rows: -1}}
	last := -1
	for _, t := range m.Tables {
		pos := slices.Index(allowed, t.Name)
		if pos < 0 {
			return nil, fmt.Errorf("bundle: manifest lists %q, which is not in the v1 table set", t.Name)
		}
		if t.Name == AccessLogTable && !m.Options.IncludeAccessLog {
			return nil, fmt.Errorf("bundle: %s present but include_access_log is false", AccessLogTable)
		}
		if accounted[t.Name] {
			return nil, fmt.Errorf("bundle: %s listed twice", t.Name)
		}
		if pos < last {
			return nil, fmt.Errorf("bundle: %s is out of load order", t.Name)
		}
		last = pos
		if t.File != tablesDir+t.Name+".ndjson" {
			return nil, fmt.Errorf("bundle: %s has file %q, want %s", t.Name, t.File, tablesDir+t.Name+".ndjson")
		}
		if t.Rows < 0 || len(t.PK) == 0 || len(t.Columns) == 0 {
			return nil, fmt.Errorf("bundle: %s has an incomplete manifest entry", t.Name)
		}
		accounted[t.Name] = true
		expected = append(expected, expectedEntry{name: t.File, sha256: t.SHA256, rows: t.Rows})
	}
	for _, o := range m.OmittedTables {
		if !slices.Contains(allowed, o.Name) {
			return nil, fmt.Errorf("bundle: omitted table %q is not in the v1 table set", o.Name)
		}
		if accounted[o.Name] {
			return nil, fmt.Errorf("bundle: %s is both exported and omitted", o.Name)
		}
		accounted[o.Name] = true
	}
	for _, name := range allowed {
		if !accounted[name] {
			return nil, fmt.Errorf("bundle: table %s is neither exported nor listed as omitted", name)
		}
	}
	if m.Secrets != nil {
		if m.Secrets.File != SecretsFile || !m.Options.IncludeSecrets {
			return nil, errors.New("bundle: secrets entry inconsistent with include_secrets")
		}
		expected = append(expected, expectedEntry{name: SecretsFile, sha256: m.Secrets.SHA256, rows: -1})
	} else if m.Options.IncludeSecrets {
		return nil, errors.New("bundle: include_secrets is set but no secrets file is listed")
	}
	return expected, nil
}

// lineCounter counts '\n' bytes and remembers whether the data ended on one.
type lineCounter struct {
	n      int64 // bytes seen
	nl     int64
	endsNL bool
}

func (c *lineCounter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.n += int64(len(p))
		c.nl += int64(bytes.Count(p, []byte{'\n'}))
		c.endsNL = p[len(p)-1] == '\n'
	}
	return len(p), nil
}

func (c *lineCounter) lines() int64 { return c.nl }
