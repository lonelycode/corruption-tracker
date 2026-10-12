// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package suppression

import (
	"reflect"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		text    string
		hit     bool
		phrases []string
	}{
		{"", false, nil},
		{"He was sentenced to two years for tax evasion.", false, nil},
		// Phrases come back in regex-bank order, verbatim, deduped case-insensitively.
		{"The man, who has name suppression, cannot be named.", true, []string{"name suppression", "suppression", "cannot be named", "has name suppression"}},
		{"[Name deleted] pleaded guilty; NAME SUPPRESSION and name suppression.", true, []string{"NAME SUPPRESSION", "SUPPRESSION", "[Name deleted]"}},
		{"The insuppressible suppressor.", false, nil},
	}
	for _, c := range cases {
		hit, phrases := Detect(c.text)
		if hit != c.hit || !reflect.DeepEqual(phrases, c.phrases) {
			t.Errorf("Detect(%q) = %v %q, want %v %q", c.text, hit, phrases, c.hit, c.phrases)
		}
	}
}
