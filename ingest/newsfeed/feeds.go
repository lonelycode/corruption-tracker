// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package newsfeed is the standalone tracker's news corpus: it polls the
// operator's RSS/Atom feeds into tracker_news_items and serves them to
// classify-news as a host.NewsItemSource (design
// §5.6). Embedded in alitheia it is unused: alitheia's own
// corpus adapter feeds classify-news there.
//
// Source rules are the connectors' rules: every feed is re-checked against
// its host's robots.txt before each poll, and fetched with the operator's
// crawler identity. Only the feed document is fetched, never the article
// pages, so what classify-news reads is what the publisher put in the feed
// (title, summary and, where the feed carries it, the full text).
package newsfeed

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Feed is one configured feed.
type Feed struct {
	Slug   string // names the feed on claims and in logs
	URL    string
	Corpus string // "news" or "commentary"
}

// DefaultFeeds is the feed list of a fresh instance: New Zealand outlets'
// own public RSS feeds, led by the NZ Herald's crime section, whose court
// reporting is where most fraud prosecutions surface. Each publishes the
// feed for syndication and allows it in robots.txt (checked 2026-10-12;
// the connector re-checks on every poll). classify-news only reads items
// whose text has enforcement vocabulary, so a general news feed costs
// nothing beyond the fetch.
const DefaultFeeds = `nzherald-crime https://www.nzherald.co.nz/arc/outboundfeeds/rss/section/nz/crime/?outputType=xml&_website=nzh
rnz https://www.rnz.co.nz/rss/news.xml
1news https://www.1news.co.nz/arc/outboundfeeds/rss/?outputType=xml
stuff https://www.stuff.co.nz/rss`

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ParseFeeds reads a feed list: one feed per line, as "slug URL",
// "slug URL commentary" or just "URL" (the slug is then the host name).
// Blank lines and lines starting with # are ignored.
func ParseFeeds(text string) ([]Feed, error) {
	var out []Feed
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f, err := parseFeedLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if seen[f.Slug] {
			return nil, fmt.Errorf("line %d: feed name %q is used twice", i+1, f.Slug)
		}
		seen[f.Slug] = true
		out = append(out, f)
	}
	return out, nil
}

func parseFeedLine(line string) (Feed, error) {
	fields := strings.Fields(line)
	var f Feed
	switch len(fields) {
	case 1:
		f.URL = fields[0]
	case 2:
		f.Slug, f.URL = fields[0], fields[1]
	case 3:
		f.Slug, f.URL, f.Corpus = fields[0], fields[1], fields[2]
	default:
		return f, errors.New(`use "name URL" (optionally followed by "commentary")`)
	}
	u, err := url.Parse(f.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return f, fmt.Errorf("%q is not an http(s) URL", f.URL)
	}
	if f.Slug == "" {
		f.Slug = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		f.Slug = strings.ReplaceAll(f.Slug, ".", "-")
	}
	if !slugRE.MatchString(f.Slug) {
		return f, fmt.Errorf("feed name %q: use lowercase letters, digits and hyphens", f.Slug)
	}
	switch f.Corpus {
	case "":
		f.Corpus = "news"
	case "news", "commentary":
	default:
		return f, fmt.Errorf("%q: the third word may only be news or commentary", f.Corpus)
	}
	return f, nil
}
