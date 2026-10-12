// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package host

import "testing"

func TestCrawler(t *testing.T) {
	cases := []struct {
		c         Crawler
		ua, token string
	}{
		{Crawler{}, "corruption-tracker/1.0 (+https://github.com/lonelycode/corruption-tracker)", "corruption-tracker"},
		{Crawler{Name: "ExampleBot"}, "corruption-tracker/1.0 (+https://github.com/lonelycode/corruption-tracker)", "corruption-tracker"}, // no contact: default
		{Crawler{Name: "ExampleBot", ContactURL: "https://example.org"}, "ExampleBot/1.0 (+https://example.org)", "example"},
		{Crawler{Name: "ExampleBot", ContactURL: "https://example.org", Purpose: "research", RobotsToken: "ExampleBot"},
			"ExampleBot/1.0 (+https://example.org; research)", "examplebot"},
	}
	for _, c := range cases {
		if got := c.c.UserAgent(); got != c.ua {
			t.Errorf("%+v UserAgent = %q, want %q", c.c, got, c.ua)
		}
		if got := c.c.Token(); got != c.token {
			t.Errorf("%+v Token = %q, want %q", c.c, got, c.token)
		}
	}
}
