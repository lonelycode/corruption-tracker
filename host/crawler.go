// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package host

import (
	"strings"
)

// Crawler names the operator's web crawler: it is sent as the User-Agent on
// every connector fetch and decides which robots.txt groups apply to it (the
// "*" group always does). Operators are identifiable and contactable by the
// sites they poll; the module itself carries no operator's name.
type Crawler struct {
	Name       string // bot name, e.g. "ExampleTrackerBot"
	ContactURL string // where a site owner can reach the operator
	Purpose    string // optional short description, e.g. "public-interest research"
	// RobotsToken is matched case-insensitively as a substring of robots.txt
	// User-agent lines. Empty derives it from Name (lowercased, without a
	// trailing "bot").
	RobotsToken string
}

// DefaultCrawler is the identity used when the operator configured none:
// neutral, naming the software and its source rather than any operator.
var DefaultCrawler = Crawler{
	Name:       "corruption-tracker",
	ContactURL: "https://github.com/lonelycode/corruption-tracker",
}

// UserAgent renders the header value: "Name/1.0 (+ContactURL; Purpose)".
func (c Crawler) UserAgent() string {
	c = c.orDefault()
	ua := c.Name + "/1.0 (+" + c.ContactURL
	if c.Purpose != "" {
		ua += "; " + c.Purpose
	}
	return ua + ")"
}

// Token is the lowercased robots.txt agent token.
func (c Crawler) Token() string {
	c = c.orDefault()
	if c.RobotsToken != "" {
		return strings.ToLower(c.RobotsToken)
	}
	return strings.TrimSuffix(strings.ToLower(c.Name), "bot")
}

func (c Crawler) orDefault() Crawler {
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.ContactURL) == "" {
		return DefaultCrawler
	}
	return c
}

// CrawlerSettings is implemented by Settings that name the operator's
// crawler. Settings without it (or returning a zero Crawler) get
// DefaultCrawler.
type CrawlerSettings interface {
	Crawler() Crawler
}
