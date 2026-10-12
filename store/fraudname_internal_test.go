// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package store

import "testing"

func TestNormalizeFraudPersonName(t *testing.T) {
	cases := map[string]string{
		"Kiri-Ann Fenwick Fenwick":      "Kiri-Ann Fenwick",
		"Rowan Ellis McTavish McTavish": "Rowan Ellis McTavish",
		"Dorian Quayle Quayle":          "Dorian Quayle",
		"Yuting (Ivy) Testa Testa":      "Yuting (Ivy) Testa",
		"fenwick FENWICK":               "fenwick",            // case-insensitive duplicate
		"  Jane   Doe  ":                "Jane Doe",           // trim + collapse, no dup
		"Jane Doe":                      "Jane Doe",           // untouched
		"Madonna":                       "Madonna",            // single token untouched
		"Talia Talia Morrow":            "Talia Talia Morrow", // only trailing dup stripped
		"":                              "",
	}
	for in, want := range cases {
		if got := normalizeFraudPersonName(in); got != want {
			t.Errorf("normalizeFraudPersonName(%q) = %q, want %q", in, got, want)
		}
	}
}
