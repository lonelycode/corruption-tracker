// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestHasFraudVocabulary(t *testing.T) {
	cases := map[string]bool{
		"A company director was sentenced today for tax evasion":          true,
		"Mother and daughter sentenced for wage subsidy fraud":            true,
		"The pair were charged with money laundering offences":            true,
		"Court of Appeal dismissed his appeal against a fraud conviction": true,
		"Crypto investors urged to get tax compliant":                     false, // no process word
		"Watch out for scammers this tax season":                          false,
		"Two-step verification helps contain cyber attack":                false,
		"New appointments announced to the board":                         false,
		"The defendant was jailed for cartel conduct":                     true,
		"He was sentenced for careless driving":                           false, // process word, no fraud word
	}
	for text, want := range cases {
		if got := HasFraudVocabulary(text); got != want {
			t.Errorf("HasFraudVocabulary(%q) = %v, want %v", text, got, want)
		}
	}
}

// firstChars must never cut a multi-byte rune in half — Postgres rejects
// invalid UTF-8 outright (seen in prod: an em-dash split at a note
// truncation boundary failed a quarantine transition).
func TestFirstCharsRuneSafe(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"plain ascii", 5, "plain"},
		{"short", 80, "short"},
		{"ab—cd", 3, "ab"}, // em-dash starts at byte 2, 3 bytes wide
		{"ab—cd", 4, "ab"}, // cut inside the dash
		{"ab—cd", 5, "ab—"},
		{"māori", 2, "m"}, // macron ā is 2 bytes
	}
	for _, c := range cases {
		got := firstChars(c.s, c.n)
		if got != c.want {
			t.Errorf("firstChars(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestHTMLToTextAndTitle(t *testing.T) {
	html := `<html><head><title>Guilty plea entered - Serious Fraud Office, New Zealand</title>
	<script>junk()</script></head><body><nav>menu</nav>
	<h1>Guilty plea entered in loan fraud case</h1>
	<p>A guilty plea was entered today.</p><footer>foot</footer></body></html>`
	text := HTMLToText(html)
	if strings.Contains(text, "junk") || strings.Contains(text, "menu") || strings.Contains(text, "foot") {
		t.Errorf("script/nav/footer not stripped: %q", text)
	}
	if !strings.Contains(text, "A guilty plea was entered today.") {
		t.Errorf("body text missing: %q", text)
	}
	if got := pageTitle(html); got != "Guilty plea entered in loan fraud case" {
		t.Errorf("pageTitle = %q", got)
	}
}

func TestRobotsAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /admin\nDisallow: /media-cases/\n"))
	}))
	defer srv.Close()

	blocked := Source{Slug: "x", RobotsURL: srv.URL, PathPrefix: "/media-cases/media-releases/"}
	ok, err := robotsAllows(context.Background(), NewFetcher(), blocked)
	if err != nil || ok {
		t.Errorf("disallowed prefix must block: ok=%v err=%v", ok, err)
	}

	open := Source{Slug: "y", RobotsURL: srv.URL, PathPrefix: "/media-releases/"}
	ok, err = robotsAllows(context.Background(), NewFetcher(), open)
	if err != nil || !ok {
		t.Errorf("unrelated disallow must not block: ok=%v err=%v", ok, err)
	}
}

func TestListingIndexer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<a href="/media-releases/2026/mother-and-daughter-sentenced-for-tax-evasion">x</a>
		<a href="/media-releases/2026/mother-and-daughter-sentenced-for-tax-evasion">dup</a>
		<a href="/media-releases/media">listing</a>
		<a href="/index/media-releases#f-mediaFacet=Business">facet</a>`))
	}))
	defer srv.Close()

	fetch := listingIndexer(srv.URL, "https://example.test",
		regexp.MustCompile(`href="(/media-releases/\d{4}/[^"#?]+)"`))
	items, err := fetch(context.Background(), NewFetcher())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 { // duplicate collapsed, listing/facet links excluded
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].URL != "https://example.test/media-releases/2026/mother-and-daughter-sentenced-for-tax-evasion" {
		t.Errorf("wrong item: %q", items[0].URL)
	}
}

func TestSitemapIndexer(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
			<sitemap><loc>%s/sitemap-child.xml</loc></sitemap></sitemapindex>`, srv.URL)
	})
	mux.HandleFunc("/sitemap-child.xml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
			<url><loc>https://x.test/newsroom/stories/fraud-sentencing</loc><lastmod>2026-01-02T03:04:05+13:00</lastmod></url>
			<url><loc>https://x.test/newsroom/stories/older-item</loc><lastmod>2024-01-02T03:04:05+13:00</lastmod></url>
			<url><loc>https://x.test/newsroom/stories</loc></url>
			<url><loc>https://x.test/contact</loc></url></urlset>`))
	})

	fetch := sitemapIndexer(srv.URL, "/newsroom/stories/", 0)
	items, err := fetch(context.Background(), NewFetcher())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 { // listing root + unrelated path excluded
		t.Fatalf("items = %d, want 2: %+v", len(items), items)
	}
	if items[0].URL != "https://x.test/newsroom/stories/fraud-sentencing" {
		t.Errorf("newest-first ordering broken: %q", items[0].URL)
	}
	if items[0].IndexDate == nil {
		t.Error("lastmod should populate IndexDate")
	}
}

func TestRobotsMissingMeansAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ok, err := robotsAllows(context.Background(), NewFetcher(),
		Source{Slug: "x", RobotsURL: srv.URL + "/robots.txt", PathPrefix: "/newsroom/"})
	if err != nil || !ok {
		t.Errorf("404 robots must mean default-allow: ok=%v err=%v", ok, err)
	}
}

func TestSuppressionScreen(t *testing.T) {
	named := &Extraction{IsFraudCase: true, CaseStage: "sentenced"}
	named.Persons = append(named.Persons, struct {
		NameAsWritten string `json:"name_as_written"`
		Role          string `json:"role"`
		Profession    string `json:"profession"`
		Charges       string `json:"charges"`
		Outcome       string `json:"outcome"`
	}{NameAsWritten: "Jane Doe"})

	// Clean sentenced case with a named person: no signal.
	if hit, _ := suppressionScreen(named, "Jane Doe was sentenced for fraud."); hit {
		t.Error("clean case must not flag")
	}

	// Extractor flag fires.
	flagged := *named
	flagged.SuppressionIndicators.NameSuppressionMentioned = true
	flagged.SuppressionIndicators.SuppressionDetail = "cannot be named"
	if hit, note := suppressionScreen(&flagged, "x"); !hit || !strings.Contains(note, "extractor") {
		t.Errorf("extractor signal: hit=%v note=%q", hit, note)
	}

	// Text scan catches what the extractor missed.
	if hit, note := suppressionScreen(named, "The accused has interim name suppression."); !hit || !strings.Contains(note, "text scan") {
		t.Errorf("text signal: hit=%v note=%q", hit, note)
	}

	// Unnamed defendant at a pre-verdict stage: the interim-suppression shape.
	unnamed := &Extraction{IsFraudCase: true, CaseStage: "charges_laid"}
	if hit, note := suppressionScreen(unnamed, "A man faces fraud charges."); !hit || !strings.Contains(note, "interim suppression likely") {
		t.Errorf("unnamed pre-verdict: hit=%v note=%q", hit, note)
	}
	// ...but an unnamed SENTENCED case is just a thin source, not suppression.
	unnamedDone := &Extraction{IsFraudCase: true, CaseStage: "sentenced"}
	if hit, _ := suppressionScreen(unnamedDone, "A man was sentenced for fraud."); hit {
		t.Error("unnamed post-verdict must not auto-flag")
	}
}
