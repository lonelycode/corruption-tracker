// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/host"
)

type capturingTransport struct {
	ua   string
	body string
}

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.ua = r.Header.Get("User-Agent")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(c.body)), Header: http.Header{}}, nil
}

func TestFetcherSendsOperatorUserAgent(t *testing.T) {
	op := host.Crawler{Name: "ExampleBot", ContactURL: "https://example.org", Purpose: "research"}
	for _, c := range []struct {
		f    *Fetcher
		want string
	}{
		{NewFetcher(), host.DefaultCrawler.UserAgent()},
		{&Fetcher{}, host.DefaultCrawler.UserAgent()}, // zero value: default identity
		{NewFetcherFor(op), "ExampleBot/1.0 (+https://example.org; research)"},
	} {
		tr := &capturingTransport{}
		c.f.Client = &http.Client{Transport: tr}
		if _, err := c.f.Get(context.Background(), "https://agency.example/x"); err != nil {
			t.Fatal(err)
		}
		if tr.ua != c.want {
			t.Errorf("User-Agent = %q, want %q", tr.ua, c.want)
		}
	}
}

func TestRobotsGroupsMatchOperatorToken(t *testing.T) {
	src := Source{RobotsURL: "https://agency.example/robots.txt", PathPrefix: "/news/"}
	robots := "User-agent: ExampleBot\nDisallow: /news/\n\nUser-agent: *\nDisallow: /private/\n"
	for _, c := range []struct {
		crawler host.Crawler
		allowed bool
	}{
		{host.Crawler{}, true}, // default token "corruption-tracker" is not named
		{host.Crawler{Name: "ExampleBot", ContactURL: "https://example.org"}, false},
		{host.Crawler{Name: "OtherBot", ContactURL: "https://example.org"}, true},
	} {
		f := NewFetcherFor(c.crawler)
		f.Client = &http.Client{Transport: &capturingTransport{body: robots}}
		ok, err := robotsAllows(context.Background(), f, src)
		if err != nil {
			t.Fatal(err)
		}
		if ok != c.allowed {
			t.Errorf("crawler %q: allowed = %v, want %v", c.crawler.Name, ok, c.allowed)
		}
	}
}
