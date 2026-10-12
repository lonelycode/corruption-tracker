// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package fraudagency polls agency media-release feeds (SFO, IRD — the two
// sources behind ~70% of the historical spreadsheet) for fraud/corruption
// enforcement matters and routes them as UNVERIFIED claims into the
// fraud-claims review queue. Nothing here writes to fraud_cases; a human
// reviewer promotes every claim.
//
// Source compliance: both sites' robots.txt permit the paths used (checked
// 2026-06-10; the connector re-checks robots.txt on every run and disables
// itself rather than argue). Content is NZ Crown copyright; both agencies
// publish under NZGOAL / CC BY 4.0 — the licence basis is recorded on every
// claim. There is NO judgment-database (JDO/NZLII) code path here and none
// may be added — see the original tracker design notes §4.
//
// Pipeline per item: index fetch -> URL-keyed dedupe -> page fetch ->
// html-to-text -> vocabulary pre-filter (IRD publishes plenty of
// non-prosecution releases) -> LLM extraction (fraud_case_v1, forced
// tool-use; failure queues for manual extraction, never blind-retries) ->
// claim creation (+auto-quarantine on suppression indicators) -> case-match
// suggestions so appeal/sentencing follow-ups attach to existing cases.
package fraudagency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lonelycode/corruption-tracker/store"

	"github.com/PuerkitoBio/goquery"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/llm"
	"github.com/lonelycode/corruption-tracker/schemas"
	"github.com/lonelycode/corruption-tracker/suppression"
)

// Item is one media release discovered on an index.
type Item struct {
	URL       string
	Title     string
	IndexDate *time.Time // from sitemap lastmod where available
}

// Source is one pollable agency feed.
type Source struct {
	Slug         string
	Label        string // publisher label on claims
	AgencySlug   string // fraud_agencies slug
	LicenceBasis string
	RobotsURL    string
	// pathPrefix must be allowed by robots for the connector to run.
	PathPrefix string
	FetchIndex func(ctx context.Context, f *Fetcher) ([]Item, error)
}

// Sources is the registry. Adding an agency = one entry + an index parser
// (sitemapIndexer where the listing is JS-rendered, listingIndexer where it
// is server-rendered). NOT registered, and why:
//   - FMA: listing is JS-rendered and the sitemap endpoint throttles/drops
//     connections after one fetch (probed 2026-06-10). Their enforcement
//     announcements reach us via the news classifier; revisit if they
//     publish a stable feed.
//   - INZ: news-centre listing is JS-paginated (one featured item in HTML)
//     and the sitemap is 11 chunks per poll. Immigration-fraud prosecutions
//     are covered via MBIE, Police, and the news classifier.
//   - OpenLaw NZ: blocked on an access arrangement (non-commercial default
//     licence) — see the original tracker design notes §4.
var Sources = []Source{
	{
		Slug:         "sfo",
		Label:        "Serious Fraud Office",
		AgencySlug:   "sfo",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, sfo.govt.nz)",
		RobotsURL:    "https://www.sfo.govt.nz/robots.txt",
		PathPrefix:   "/media-cases/media-releases/",
		FetchIndex:   sitemapIndexer("https://www.sfo.govt.nz", "/media-cases/media-releases/", 2*time.Second),
	},
	{
		Slug:         "ird",
		Label:        "Inland Revenue",
		AgencySlug:   "ird",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, ird.govt.nz)",
		RobotsURL:    "https://www.ird.govt.nz/robots.txt",
		PathPrefix:   "/media-releases/",
		FetchIndex: listingIndexer("https://www.ird.govt.nz/media-releases", "https://www.ird.govt.nz",
			regexp.MustCompile(`href="(/media-releases/\d{4}/[^"#?]+)"`)),
	},
	{
		Slug:         "comcom",
		Label:        "Commerce Commission",
		AgencySlug:   "comcom",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, comcom.govt.nz)",
		RobotsURL:    "https://comcom.govt.nz/robots.txt",
		PathPrefix:   "/news-and-media/",
		FetchIndex: listingIndexer("https://comcom.govt.nz/news-and-media/media-releases", "https://comcom.govt.nz",
			regexp.MustCompile(`href="(/news-and-media/news-and-events/\d{4}/[^"#?]+)"`)),
	},
	{
		Slug:         "msd",
		Label:        "Ministry of Social Development",
		AgencySlug:   "msd",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, msd.govt.nz)",
		RobotsURL:    "https://www.msd.govt.nz/robots.txt",
		PathPrefix:   "/about-msd-and-our-work/newsroom/media-releases/",
		FetchIndex: listingIndexer("https://www.msd.govt.nz/about-msd-and-our-work/newsroom/media-releases/index.html", "https://www.msd.govt.nz",
			regexp.MustCompile(`href="(/about-msd-and-our-work/newsroom/media-releases/\d{4}/[^"#?]+\.html)"`)),
	},
	{
		Slug:         "nz-police",
		Label:        "NZ Police",
		AgencySlug:   "nz-police",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, police.govt.nz)",
		RobotsURL:    "https://www.police.govt.nz/robots.txt",
		PathPrefix:   "/news/release/",
		// High volume, mostly non-fraud: the vocabulary pre-filter rejects
		// (and records) most items before any LLM spend.
		FetchIndex: listingIndexer("https://www.police.govt.nz/news", "https://www.police.govt.nz",
			regexp.MustCompile(`href="(/news/release/[^"#?]+)"`)),
	},
	{
		Slug:         "acc",
		Label:        "ACC",
		AgencySlug:   "acc",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, acc.co.nz)",
		RobotsURL:    "https://www.acc.co.nz/robots.txt", // 404 = default-allow
		PathPrefix:   "/newsroom/stories/",
		FetchIndex:   sitemapIndexer("https://www.acc.co.nz", "/newsroom/stories/", 2*time.Second),
	},
	{
		Slug:         "mbie",
		Label:        "MBIE",
		AgencySlug:   "mbie",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, mbie.govt.nz)",
		RobotsURL:    "https://www.mbie.govt.nz/robots.txt",
		PathPrefix:   "/about/news/",
		FetchIndex: listingIndexer("https://www.mbie.govt.nz/about/news", "https://www.mbie.govt.nz",
			regexp.MustCompile(`href="(/about/news/[^"#?]+)"`)),
	},
	{
		Slug:         "dia",
		Label:        "Department of Internal Affairs",
		AgencySlug:   "dia",
		LicenceBasis: "crown-copyright-cc-by-4.0 (NZGOAL, dia.govt.nz)",
		RobotsURL:    "https://www.dia.govt.nz/robots.txt",
		PathPrefix:   "/press.nsf/",
		FetchIndex: listingIndexer("https://www.dia.govt.nz/press.nsf/index?OpenView", "https://www.dia.govt.nz",
			regexp.MustCompile(`href="(/press\.nsf/[0-9a-f]+/[0-9a-f]+!OpenDocument)"`)),
	},
}

// SourceBySlug returns a registered source.
func SourceBySlug(slug string) (Source, bool) {
	for _, s := range Sources {
		if s.Slug == slug {
			return s, true
		}
	}
	return Source{}, false
}

// Fetcher is a minimal polite HTTP client. Crawler identifies the operator
// (User-Agent and robots.txt agent); the zero value is host.DefaultCrawler.
type Fetcher struct {
	Client  *http.Client
	Crawler host.Crawler
}

// NewFetcher returns a Fetcher identifying as host.DefaultCrawler.
func NewFetcher() *Fetcher {
	return NewFetcherFor(host.Crawler{})
}

// NewFetcherFor returns a Fetcher identifying as the operator's crawler.
func NewFetcherFor(c host.Crawler) *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: 30 * time.Second}, Crawler: c}
}

func (f *Fetcher) Get(ctx context.Context, url string) (string, error) {
	body, status, err := f.get(ctx, url)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, status)
	}
	return body, nil
}

func (f *Fetcher) get(ctx context.Context, url string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", f.Crawler.UserAgent())
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}

// robotsAllows re-checks robots.txt for the source's path prefix on every
// run. Any Disallow line that prefixes our path (for * agents) disables the
// connector — terms changes get a disabled connector, not an argument. A
// missing robots.txt (404/410, e.g. FMA and ACC) is the web's default-allow;
// any other failure is an error, not permission.
func robotsAllows(ctx context.Context, f *Fetcher, s Source) (bool, error) {
	body, status, err := f.get(ctx, s.RobotsURL)
	if err != nil {
		return false, fmt.Errorf("robots fetch: %w", err)
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return true, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("robots fetch %s: status %d", s.RobotsURL, status)
	}
	return !robotsDisallows(body, s.PathPrefix, f.Crawler.Token()), nil
}

// RobotsAllowsURL applies the same rule to one URL: its host's robots.txt
// must not disallow its path and query for us. The news-feed connector
// checks every feed with it before each poll.
func RobotsAllowsURL(ctx context.Context, f *Fetcher, rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false, fmt.Errorf("not an http(s) URL: %q", rawURL)
	}
	return robotsAllows(ctx, f, Source{RobotsURL: u.Scheme + "://" + u.Host + "/robots.txt", PathPrefix: u.RequestURI()})
}

// robotsDisallows reports whether a robots.txt body disallows the given
// path for the * agent or for our own agent (token: the operator crawler's
// robots token, lowercased).
func robotsDisallows(body, path, token string) bool {
	applies := false
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "user-agent:"):
			agent := strings.TrimSpace(line[len("user-agent:"):])
			applies = agent == "*" || (token != "" && strings.Contains(strings.ToLower(agent), token))
		case applies && strings.HasPrefix(low, "disallow:"):
			p := strings.TrimSpace(line[len("disallow:"):])
			if p == "" {
				continue
			}
			clean := strings.TrimSuffix(strings.TrimSuffix(p, "*"), "$")
			if clean != "" && strings.HasPrefix(path, clean) {
				return true
			}
		}
	}
	return false
}

// ─────────────────────────── index parsers ───────────────────────────
//
// Two generic shapes cover every agency:
//   - sitemapIndexer: walks a SilverStripe sitemap index (sitemap.xml ->
//     child urlsets) keeping URLs under a path prefix, newest lastmod
//     first. Used where the listing page is JS-rendered (SFO, ACC, FMA).
//   - listingIndexer: regex over a plain-HTML listing page. Used where the
//     items are server-rendered (IRD, ComCom, MSD, MBIE, Police, DIA, INZ).

// sitemapIndexer returns a FetchIndex that walks base+"/sitemap.xml".
// Child-sitemap fetches are spaced by pause — some hosts (FMA) throttle
// rapid sitemap reads.
func sitemapIndexer(base, pathPrefix string, pause time.Duration) func(context.Context, *Fetcher) ([]Item, error) {
	return func(ctx context.Context, f *Fetcher) ([]Item, error) {
		body, err := f.Get(ctx, base+"/sitemap.xml")
		if err != nil {
			return nil, err
		}
		var index struct {
			Sitemaps []struct {
				Loc string `xml:"loc"`
			} `xml:"sitemap"`
		}
		children := []string{base + "/sitemap.xml"}
		if err := xml.Unmarshal([]byte(body), &index); err == nil && len(index.Sitemaps) > 0 {
			children = children[:0]
			for _, s := range index.Sitemaps {
				children = append(children, strings.TrimSpace(s.Loc))
			}
		}
		var items []Item
		for i, child := range children {
			if i > 0 && pause > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(pause):
				}
			}
			chunk := body
			if child != base+"/sitemap.xml" {
				chunk, err = f.Get(ctx, child)
				if err != nil {
					return nil, fmt.Errorf("sitemap chunk %s: %w", child, err)
				}
			}
			var parsed struct {
				URLs []struct {
					Loc     string `xml:"loc"`
					LastMod string `xml:"lastmod"`
				} `xml:"url"`
			}
			if err := xml.Unmarshal([]byte(chunk), &parsed); err != nil {
				return nil, fmt.Errorf("sitemap parse %s: %w", child, err)
			}
			for _, u := range parsed.URLs {
				loc := strings.TrimSpace(u.Loc)
				if !strings.Contains(loc, pathPrefix) || strings.HasSuffix(strings.TrimSuffix(loc, "/"), strings.TrimSuffix(pathPrefix, "/")) {
					continue // skip non-matching URLs and the listing root itself
				}
				it := Item{URL: loc}
				if t, err := time.Parse(time.RFC3339, u.LastMod); err == nil {
					tt := t.UTC()
					it.IndexDate = &tt
				}
				items = append(items, it)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			ti, tj := time.Time{}, time.Time{}
			if items[i].IndexDate != nil {
				ti = *items[i].IndexDate
			}
			if items[j].IndexDate != nil {
				tj = *items[j].IndexDate
			}
			return ti.After(tj)
		})
		return items, nil
	}
}

// listingIndexer returns a FetchIndex that regexes item links out of a
// server-rendered listing page. The regex's first capture group is the item
// path, made absolute against base.
func listingIndexer(listingURL, base string, re *regexp.Regexp) func(context.Context, *Fetcher) ([]Item, error) {
	return func(ctx context.Context, f *Fetcher) ([]Item, error) {
		body, err := f.Get(ctx, listingURL)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		var items []Item
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			u := m[1]
			if !strings.HasPrefix(u, "http") {
				u = base + u
			}
			if !seen[u] {
				seen[u] = true
				items = append(items, Item{URL: u})
			}
		}
		return items, nil
	}
}

// ─────────────────────────── page processing ───────────────────────────

// HTMLToText strips markup to readable text.
func HTMLToText(html string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return html
	}
	doc.Find("script, style, nav, header, footer").Remove()
	return strings.Join(strings.Fields(doc.Text()), " ")
}

func pageTitle(html string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return ""
	}
	if h1 := strings.TrimSpace(doc.Find("h1").First().Text()); h1 != "" {
		return strings.Join(strings.Fields(h1), " ")
	}
	t := strings.TrimSpace(doc.Find("title").Text())
	// Strip site suffixes like " - Serious Fraud Office, New Zealand".
	if i := strings.Index(t, " - "); i > 0 {
		t = t[:i]
	}
	return strings.Join(strings.Fields(t), " ")
}

// HasFraudVocabulary is the cheap pre-filter: IRD/ComCom publish plenty of
// compliance reminders and awareness pieces that never go near a court, so
// a release must pass before an LLM call is spent. Passing requires BOTH a
// court-process word and an offence word, so generic mentions of "court" or
// "fraud awareness" alone don't qualify.
func HasFraudVocabulary(text string) bool {
	low := strings.ToLower(text)
	process := false
	for _, w := range []string{"sentenc", "convict", "guilty", "charged", "charges laid",
		"prosecut", "jailed", "imprison", "home detention", "appeal", "pleaded"} {
		if strings.Contains(low, w) {
			process = true
			break
		}
	}
	if !process {
		return false
	}
	for _, w := range []string{"fraud", "bribery", "corrupt", "money laundering", "launder",
		"evasion", "deception", "cartel", "insider trading", "dishonest", "wage subsidy",
		"tax", "forg", "embezzle", "ponzi", "scam", "secret commission"} {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// extraction ------------------------------------------------------------

const systemPrompt = `You extract structured information about New Zealand fraud, bribery, corruption, money-laundering, cartel, or markets-offence enforcement matters from an agency media release or news article.

Rules:
- Extract ONLY what the source states. Never infer, guess, or add facts.
- Set is_fraud_case=false for compliance reminders, warnings, policy announcements, statistics, scam-awareness pieces, or civil-only matters. Only actual enforcement matters (charges, court proceedings, convictions, sentencings, appeals) qualify.
- The matter is the CASE; people are participants. Record each named defendant/accused separately. Never list investigators, officials, or spokespeople as persons.
- Record names EXACTLY as written. If a person cannot be named (suppression), do not record a placeholder name — set the suppression indicator instead.
- Amounts: record the case total, not per-person figures; classify it alleged (pre-verdict) or proven.
- typology_slug: classify the primary offending onto the fixed list ("" only when nothing fits); typology_hint stays the source's own words.
- agency_slug: pick from the fixed list ONLY when the source states which agency investigated or prosecuted; never derive it from the offence type.
- exact_excerpt MUST be a verbatim span copied from the source.
This data is never published; it routes to human review. When in doubt, be conservative and lower your confidence.`

// Extraction mirrors fraud_case_v1.
type Extraction struct {
	IsFraudCase  bool   `json:"is_fraud_case"`
	CaseStage    string `json:"case_stage"`
	CaseNameHint string `json:"case_name_hint"`
	Persons      []struct {
		NameAsWritten string `json:"name_as_written"`
		Role          string `json:"role"`
		Profession    string `json:"profession"`
		Charges       string `json:"charges"`
		Outcome       string `json:"outcome"`
	} `json:"persons"`
	Organisations []struct {
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"organisations"`
	ProsecutingAgency string   `json:"prosecuting_agency"`
	AgencySlug        string   `json:"agency_slug"`
	Court             string   `json:"court"`
	CourtIdentifiers  []string `json:"court_identifiers"`
	Region            string   `json:"region"`
	TypologyHint      string   `json:"typology_hint"`
	TypologySlug      string   `json:"typology_slug"`
	TotalAmountNZD    float64  `json:"total_amount_nzd"`
	AmountBasis       string   `json:"amount_basis"`
	OffendingPeriod   struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"offending_period"`
	EventDate             string `json:"event_date"`
	SuppressionIndicators struct {
		NameSuppressionMentioned bool   `json:"name_suppression_mentioned"`
		SuppressionDetail        string `json:"suppression_detail"`
	} `json:"suppression_indicators"`
	CovidRelated bool    `json:"covid_related"`
	ExactExcerpt string  `json:"exact_excerpt"`
	Confidence   float64 `json:"confidence"`
}

const maxBodyChars = 24000

// runExtraction sends page text through the completer. Failure -> (nil,
// err); the caller queues for manual extraction, never blind-retries.
func runExtraction(ctx context.Context, c llm.Completer, text string) (*Extraction, json.RawMessage, error) {
	if len(text) > maxBodyChars {
		text = text[:maxBodyChars]
	}
	resp, err := c.Complete(ctx, llm.Request{
		System:        systemPrompt,
		User:          text,
		SchemaName:    "fraud_case_v1",
		Schema:        schemas.FraudCaseV1(),
		SchemaVersion: "fraud_case_v1",
		MaxTokens:     3072,
		Temperature:   0,
	})
	if err != nil {
		return nil, nil, err
	}
	var parsed Extraction
	if err := json.Unmarshal(resp.Content, &parsed); err != nil {
		return nil, resp.Content, err
	}
	return &parsed, resp.Content, nil
}

// ─────────────────────────── the poll pipeline ───────────────────────────

// Stats summarises one poll run.
type Stats struct {
	IndexItems    int
	Skipped       int // already-ingested URLs
	Fetched       int
	VocabRejected int
	NotFraudCase  int
	ClaimsNew     int
	ClaimsDeduped int
	ManualQueued  int
	Quarantined   int
	Suggestions   int
	Errors        int
}

// Deps wires a poll run.
type Deps struct {
	DB      *store.Store
	LLM     llm.Completer // nil => claims queue with needs_manual_extraction
	Log     *slog.Logger
	Fetcher *Fetcher
	Limit   int  // max NEW items processed per run
	DryRun  bool // fetch + extract, write nothing
	// CatchUp (after a handoff import, §11.4 step 4) ignores Limit and keeps
	// going through the index until CatchUpKnownRun consecutive items are
	// already known, so nothing published around the cutoff is missed.
	CatchUp bool
	// Delay between page fetches; politeness.
	Delay time.Duration
}

// CatchUpKnownRun is how many consecutive already-known index items end a
// catch-up poll.
const CatchUpKnownRun = 10

// Poll runs one source. Idempotent: URL-keyed dedupe means a re-poll only
// processes releases not yet in the queue.
func (d *Deps) Poll(ctx context.Context, src Source) (Stats, error) {
	var st Stats
	if d.Fetcher == nil {
		d.Fetcher = NewFetcher()
	}
	if d.Limit <= 0 {
		d.Limit = 25
	}
	if d.Delay == 0 {
		d.Delay = 2 * time.Second
	}

	ok, err := robotsAllows(ctx, d.Fetcher, src)
	if err != nil {
		return st, err
	}
	if !ok {
		return st, fmt.Errorf("%s robots.txt no longer permits %s — connector disabled, investigate terms", src.Slug, src.PathPrefix)
	}

	items, err := src.FetchIndex(ctx, d.Fetcher)
	if err != nil {
		return st, fmt.Errorf("%s index: %w", src.Slug, err)
	}
	st.IndexItems = len(items)

	processed, knownRun := 0, 0
	for _, it := range items {
		if !d.CatchUp && processed >= d.Limit {
			break
		}
		if d.CatchUp && knownRun >= CatchUpKnownRun {
			break
		}
		key := "url:" + it.URL
		exists, err := d.claimKeyExists(ctx, key)
		if err != nil {
			return st, err
		}
		if exists {
			st.Skipped++
			knownRun++
			continue
		}
		knownRun = 0
		processed++
		if err := d.processItem(ctx, src, it, key, &st); err != nil {
			st.Errors++
			d.Log.Warn("fraud poll item failed", "source", src.Slug, "url", it.URL, "err", err)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(d.Delay):
		}
	}
	return st, nil
}

func (d *Deps) claimKeyExists(ctx context.Context, key string) (bool, error) {
	var exists bool
	err := d.DB.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM fraud_claims WHERE dedupe_key=$1 AND deleted_at IS NULL)`, key).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("claim key lookup: %w", err)
	}
	return exists, nil
}

func (d *Deps) processItem(ctx context.Context, src Source, it Item, dedupeKey string, st *Stats) error {
	html, err := d.Fetcher.Get(ctx, it.URL)
	if err != nil {
		return err
	}
	st.Fetched++
	text := HTMLToText(html)
	title := pageTitle(html)
	if title == "" {
		title = it.URL
	}

	sum := sha256.Sum256([]byte(text))
	hash := "agency:" + hex.EncodeToString(sum[:])

	if !HasFraudVocabulary(text) {
		// Record the vocab rejection as an auto-rejected claim (no LLM call,
		// no payload) so the next poll skips this URL — essential for
		// high-volume, low-signal sources like Police where most releases
		// are not fraud matters. Rejected claims are the designed dedupe
		// memory; nothing auto-rejected is ever published.
		st.VocabRejected++
		if d.DryRun {
			return nil
		}
		rejected, wasNew, err := d.DB.CreateFraudClaim(ctx, domain.FraudClaim{
			Headline:        title,
			SourceType:      domain.FraudSourceAgencyRelease,
			SourceURL:       it.URL,
			SourcePublisher: src.Label,
			LicenceBasis:    src.LicenceBasis,
			PublishedDate:   it.IndexDate,
			ContentHash:     hash,
			DedupeKey:       &dedupeKey,
		}, nil, "fraudagency:"+src.Slug)
		if err != nil || !wasNew {
			return err
		}
		if _, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: rejected.ID, To: domain.ReviewRejected,
			ReviewerLabel: "fraudagency:" + src.Slug,
			Note:          "auto-rejected: vocabulary pre-filter (no enforcement terms in text)",
		}); err != nil {
			return err
		}
		return nil
	}

	claim := domain.FraudClaim{
		Headline:        title,
		SourceType:      domain.FraudSourceAgencyRelease,
		SourceURL:       it.URL,
		SourcePublisher: src.Label,
		LicenceBasis:    src.LicenceBasis,
		PublishedDate:   it.IndexDate,
		ContentHash:     hash,
		DedupeKey:       &dedupeKey,
	}

	var personNames, orgNames, claimCourtIDs []string
	var suppressionNote string
	var amountNZD float64
	if d.LLM == nil {
		claim.NeedsManualExtraction = true
		claim.Excerpt = firstChars(text, 800)
	} else {
		ex, raw, err := runExtraction(ctx, d.LLM, text)
		if err != nil {
			claim.NeedsManualExtraction = true
			claim.Excerpt = firstChars(text, 800)
			d.Log.Warn("extraction failed; queueing manual", "url", it.URL, "err", err)
		} else if !ex.IsFraudCase {
			// Record the negative classification as an auto-rejected claim:
			// rejected claims are retained (the designed dedupe mechanism),
			// so the next poll skips this URL instead of re-fetching and
			// re-extracting it. Nothing auto-rejected is ever published and
			// the audit trail records the machine decision for spot checks.
			st.NotFraudCase++
			if d.DryRun {
				return nil
			}
			claim.Payload = raw
			conf := ex.Confidence
			claim.ConfidenceScore = &conf
			claim.Excerpt = firstChars(text, 400)
			rejected, wasNew, err := d.DB.CreateFraudClaim(ctx, claim, nil, "fraudagency:"+src.Slug)
			if err != nil || !wasNew {
				return err
			}
			if _, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
				ClaimID: rejected.ID, To: domain.ReviewRejected,
				ReviewerLabel: "fraudagency:" + src.Slug,
				Note: fmt.Sprintf("auto-rejected: extractor classified as not an enforcement matter (stage=%s, confidence=%.2f)",
					ex.CaseStage, ex.Confidence),
			}); err != nil {
				return err
			}
			return nil
		} else {
			claim.Payload = raw
			conf := ex.Confidence
			claim.ConfidenceScore = &conf
			claim.Excerpt = ex.ExactExcerpt
			suppressed, reason := suppressionScreen(ex, text)
			claim.SuppressionFlag = suppressed
			suppressionNote = reason
			if t := parseISODate(ex.EventDate); t != nil && claim.PublishedDate == nil {
				claim.PublishedDate = t
			}
			personNames = namedPersons(ex)
			for _, o := range ex.Organisations {
				if strings.TrimSpace(o.Name) != "" {
					orgNames = append(orgNames, o.Name)
				}
			}
			amountNZD = ex.TotalAmountNZD
			claimCourtIDs = courtIDs(ex, text)
		}
	}

	if d.DryRun {
		d.Log.Info("dry-run: would create claim", "source", src.Slug, "title", title,
			"manual", claim.NeedsManualExtraction, "suppression", claim.SuppressionFlag,
			"persons", len(personNames), "orgs", len(orgNames))
		st.ClaimsNew++
		return nil
	}

	created, wasNew, err := d.DB.CreateFraudClaim(ctx, claim, nil, "fraudagency:"+src.Slug)
	if err != nil {
		return err
	}
	if !wasNew {
		st.ClaimsDeduped++
		return nil
	}
	st.ClaimsNew++
	if claim.NeedsManualExtraction {
		st.ManualQueued++
	}

	// Any suppression signal auto-quarantines the claim (two distinct
	// reviewers required to release — same rule as convictions).
	if claim.SuppressionFlag {
		if _, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: created.ID, To: domain.ReviewQuarantined,
			ReviewerLabel: "fraudagency:" + src.Slug,
			Note:          "auto-quarantined: " + suppressionNote,
		}); err != nil {
			d.Log.Warn("auto-quarantine failed", "claim", created.ID, "err", err)
		} else {
			st.Quarantined++
		}
	}

	// Case matching BEFORE person matching: suggest existing cases so an
	// appeal/sentencing follow-up attaches as an event, not a new case.
	n, err := d.DB.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: created.ID, PersonNames: personNames, OrgNames: orgNames,
		AmountNZD: amountNZD, CourtIDs: claimCourtIDs,
	})
	if err != nil {
		d.Log.Warn("case match suggestion failed", "claim", created.ID, "err", err)
	}
	st.Suggestions += n
	return nil
}

// ───────────────────── suppression screening ─────────────────────

// suppressionScreen combines three independent suppression signals so a
// single missed cue can't publish a suppressed name:
//  1. the extractor's structured judgement (suppression_indicators);
//  2. a deterministic regex scan of the page text (the convictions
//     pipeline's legally-tuned signal bank — catches what the LLM missed);
//  3. the unnamed-defendant heuristic: an enforcement matter at a
//     pre-verdict stage with NO named persons is the classic shape of
//     interim suppression at first appearance.
//
// Any hit flags (and thus auto-quarantines) the claim; the note tells the
// reviewer which signal fired and why.
func suppressionScreen(ex *Extraction, text string) (bool, string) {
	var reasons []string
	if ex.SuppressionIndicators.NameSuppressionMentioned {
		r := "extractor: source mentions suppression"
		if d := strings.TrimSpace(ex.SuppressionIndicators.SuppressionDetail); d != "" {
			r += " (" + firstChars(d, 120) + ")"
		}
		reasons = append(reasons, r)
	}
	if hit, phrases := suppression.Detect(text); hit {
		reasons = append(reasons, "text scan: "+firstChars(strings.Join(phrases, "; "), 160))
	}
	if ex.IsFraudCase && len(namedPersons(ex)) == 0 {
		switch ex.CaseStage {
		case "charges_laid", "before_court", "other":
			reasons = append(reasons, "no named persons at a pre-verdict stage — interim suppression likely")
		}
	}
	if len(reasons) == 0 {
		return false, ""
	}
	return true, strings.Join(reasons, " | ")
}

func namedPersons(ex *Extraction) []string {
	var out []string
	for _, p := range ex.Persons {
		if strings.TrimSpace(p.NameAsWritten) != "" {
			out = append(out, p.NameAsWritten)
		}
	}
	return out
}

// CourtIDsFromExtraction is the exported form of courtIDs for promote-time
// callers (admin handlers), where the claim excerpt is the remaining text.
func CourtIDsFromExtraction(ex *Extraction, text string) []string { return courtIDs(ex, text) }

// courtIDs unions the extractor's identifiers with a deterministic regex
// scan of the page text (same belt-and-braces as suppression — the LLM
// misses some, the regex can't), normalised and deduplicated. Junk the
// extractor invents fails normalisation and is dropped.
func courtIDs(ex *Extraction, text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		if id := fraudtracker.NormalizeCourtID(raw); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, raw := range ex.CourtIdentifiers {
		add(raw)
	}
	for _, id := range fraudtracker.DetectCourtIdentifiers(text) {
		add(id)
	}
	return out
}

// ───────────────────── promote-time mapping helpers ─────────────────────

// Resolvers bundles the taxonomy resolvers promote needs.
type Resolvers struct {
	Typology, Agency, Region, Profession *fraudtracker.TermResolver
}

// LoadResolvers builds the bundle from the DB taxonomies.
func LoadResolvers(ctx context.Context, d *store.Store) (*Resolvers, error) {
	r := &Resolvers{}
	for kind, dst := range map[string]**fraudtracker.TermResolver{
		"typology":            &r.Typology,
		"agency":              &r.Agency,
		"region":              &r.Region,
		"profession_category": &r.Profession,
	} {
		terms, err := d.LoadFraudTaxonomy(ctx, kind)
		if err != nil {
			return nil, err
		}
		*dst = fraudtracker.NewTermResolver(terms)
	}
	return r, nil
}

// stageEvent maps an extraction case_stage onto the lifecycle event type.
func stageEvent(stage string) domain.FraudEventType {
	switch stage {
	case "charges_laid", "other":
		return domain.EventChargesLaid
	case "before_court":
		return domain.EventFirstAppearance
	case "convicted", "acquitted":
		// Both are verdicts; DeriveCaseStatus reads the outcome detail
		// ("acquitted" / "not guilty") to land on the right status.
		return domain.EventVerdict
	case "sentenced":
		return domain.EventSentencing
	case "appeal_outcome":
		return domain.EventAppealOutcome
	case "discontinued":
		return domain.EventDiscontinued
	}
	return domain.EventChargesLaid
}

// EventFromExtraction builds the lifecycle event + evidencing source a
// verified claim contributes when attached to an EXISTING case.
func EventFromExtraction(ex *Extraction, claim domain.FraudClaim) (domain.FraudCaseEvent, domain.FraudCaseSource) {
	detail := outcomeDetail(ex)
	// An acquitted verdict MUST carry the word in its detail — that is what
	// DeriveCaseStatus reads to land on acquitted rather than convicted.
	if ex.CaseStage == "acquitted" && !strings.Contains(strings.ToLower(detail), "acquit") {
		detail = strings.TrimSpace("acquitted. " + detail)
	}
	ev := domain.FraudCaseEvent{
		EventType:     stageEvent(ex.CaseStage),
		EventDate:     parseISODate(ex.EventDate),
		Court:         ex.Court,
		OutcomeDetail: detail,
	}
	if ev.EventDate != nil {
		p := "day"
		ev.EventDatePrecision = &p
	}
	src := domain.FraudCaseSource{
		URL: claim.SourceURL, Publisher: claim.SourcePublisher,
		SourceType: claim.SourceType, LicenceBasis: claim.LicenceBasis,
		PublishedDate: claim.PublishedDate, Excerpt: claim.Excerpt,
		ContentHash: claim.ContentHash,
	}
	return ev, src
}

func outcomeDetail(ex *Extraction) string {
	var parts []string
	for _, p := range ex.Persons {
		if p.Outcome != "" {
			parts = append(parts, p.NameAsWritten+": "+p.Outcome)
		}
	}
	if len(parts) == 0 && ex.CaseNameHint != "" {
		parts = append(parts, ex.CaseNameHint)
	}
	return strings.Join(parts, "; ")
}

// CaseInputFromExtraction maps a verified claim's payload onto a NEW case.
// The reviewer has already attested the content; taxonomy coding resolves
// deterministically and unmapped values keep their raw text for later alias
// promotion (same contract as the spreadsheet importer).
//
// The extractor's closed-set classification (typology_slug / agency_slug,
// since fraud_case_v1 grew the enums) is preferred; the free-text hint
// through the alias resolver remains the fallback for payloads extracted
// before the enums existed. The recode sweep catches whatever still misses.
func CaseInputFromExtraction(ex *Extraction, claim domain.FraudClaim, res *Resolvers) store.FraudCaseInput {
	var in store.FraudCaseInput

	label := ex.TypologyHint
	if t, ok := resolveFirst(res.Typology, ex.TypologySlug, ex.TypologyHint); ok {
		in.Case.TypologyID = &t.ID
		label = t.Label
	}
	in.Case.TypologyRaw = ex.TypologyHint
	if t, ok := res.Region.Resolve(ex.Region); ok {
		in.Case.RegionID = &t.ID
	}
	in.Case.RegionRaw = ex.Region
	if t, ok := resolveFirst(res.Agency, ex.AgencySlug, ex.ProsecutingAgency); ok {
		in.Case.ProsecutingAgencyID = &t.ID
	}
	in.Case.AgencyRaw = ex.ProsecutingAgency

	if ex.TotalAmountNZD > 0 {
		amt := ex.TotalAmountNZD
		in.Case.TotalAmountNZD = &amt
		if ex.AmountBasis != "" {
			basis := ex.AmountBasis
			in.Case.AmountBasis = &basis
		}
	}
	in.Case.OffendingPeriodStart = parseISODate(ex.OffendingPeriod.Start)
	in.Case.OffendingPeriodEnd = parseISODate(ex.OffendingPeriod.End)
	if in.Case.OffendingPeriodStart != nil {
		p := datePrecision(ex.OffendingPeriod.Start)
		in.Case.OffendingPeriodPrecision = &p
	}
	in.Case.CourtName = ex.Court
	// Excerpt is the only page text still available at promote time; it
	// backstops the LLM's identifier list the same way the full-text scan
	// does at detection time.
	in.Case.CourtIdentifiers = courtIDs(ex, claim.Excerpt)
	in.Case.CovidRelated = ex.CovidRelated
	in.Case.Summary = ex.CaseNameHint + " — " + claim.Headline

	year := ""
	if d := parseISODate(ex.EventDate); d != nil {
		year = fmt.Sprintf("%d", d.Year())
	}
	if label == "" {
		label = "fraud"
	}
	in.Case.CaseName = strings.TrimSpace(fmt.Sprintf("%s (%s) %s", label, claim.SourcePublisher, year))

	for _, p := range ex.Persons {
		if strings.TrimSpace(p.NameAsWritten) == "" {
			continue
		}
		role := p.Role
		if role == "" {
			role = "defendant"
		}
		part := domain.FraudCaseParticipant{
			Role: role, ProfessionRaw: p.Profession, IndividualCharges: p.Charges,
			SuppressionFlag: ex.SuppressionIndicators.NameSuppressionMentioned,
		}
		if t, ok := res.Profession.Resolve(p.Profession); ok {
			part.ProfessionCategoryID = &t.ID
		}
		in.Participants = append(in.Participants, store.FraudParticipantInput{
			PersonName: p.NameAsWritten, Participant: part,
		})
	}
	for _, o := range ex.Organisations {
		if strings.TrimSpace(o.Name) == "" {
			continue
		}
		role := o.Role
		if role == "" {
			role = "vehicle"
		}
		in.Organisations = append(in.Organisations, store.FraudOrgInput{Name: o.Name, Role: role})
	}

	ev, src := EventFromExtraction(ex, claim)
	in.Events = []domain.FraudCaseEvent{ev}
	in.Sources = []domain.FraudCaseSource{src}
	return in
}

// resolveFirst resolves the first input that maps to a term (resolvers index
// slugs too, so a closed-set slug resolves directly).
func resolveFirst(r *fraudtracker.TermResolver, inputs ...string) (domain.FraudTaxonomyTerm, bool) {
	for _, in := range inputs {
		if t, ok := r.Resolve(in); ok {
			return t, true
		}
	}
	return domain.FraudTaxonomyTerm{}, false
}

func datePrecision(s string) string {
	switch len(strings.TrimSpace(s)) {
	case 4:
		return "year"
	case 7:
		return "month"
	default:
		return "day"
	}
}

func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary — a mid-rune cut produces invalid UTF-8
	// that Postgres rejects (macrons and dashes are routine in NZ text).
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ParseExtractionDate parses an ISO-ish date the extractor produced
// (YYYY-MM-DD / YYYY-MM / YYYY); nil when empty or unparseable.
func ParseExtractionDate(s string) *time.Time { return parseISODate(s) }

// DatePrecision reports the precision of an extractor date string.
func DatePrecision(s string) string { return datePrecision(s) }

func parseISODate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}
