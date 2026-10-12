// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package newsfeed_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/ingest/newsfeed"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func rss(items ...string) string {
	return `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>` + strings.Join(items, "") + `</channel></rss>`
}

func rssItem(title, link, pub string) string {
	return fmt.Sprintf(`<item><title>%s</title><link>%s</link><description>%s</description><pubDate>%s</pubDate></item>`,
		title, link, title, pub)
}

// feedServer serves robots.txt (disallowing /private) and the feeds.
func feedServer(t *testing.T, feeds map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := feeds[r.URL.Path]
		if !ok {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		fetches.Add(1)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &fetches
}

func TestRefreshStoresOncePerStoryAndListsNewestFirst(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	srv, fetches := feedServer(t, map[string]string{
		"/crime.rss": rss(
			rssItem("Accountant sentenced for fraud", "https://news.example.org/a?utm_source=crime", "Mon, 05 Oct 2026 09:00:00 +0000"),
			rssItem("Director charged", "https://news.example.org/b", "Tue, 06 Oct 2026 09:00:00 +0000"),
		),
		// The same story syndicated with different tracking parameters.
		"/national.rss": rss(rssItem("Accountant sentenced for fraud", "https://news.example.org/a?utm_source=national#x", "Mon, 05 Oct 2026 09:00:00 +0000")),
		"/blog.atom": `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Opinion: the fraud sentencing</title>
			<link href="https://blog.example.org/opinion"/><updated>2026-10-07T00:00:00Z</updated></entry></feed>`,
		"/private/feed.rss": rss(rssItem("never fetched", "https://news.example.org/p", "")),
		"/notafeed":         "<html>moved</html>",
	})
	src := &newsfeed.Source{Pool: st.Pool, Fetcher: fraudagency.NewFetcher(), Feeds: []newsfeed.Feed{
		{Slug: "crime", URL: srv.URL + "/crime.rss", Corpus: "news"},
		{Slug: "national", URL: srv.URL + "/national.rss", Corpus: "news"},
		{Slug: "blog", URL: srv.URL + "/blog.atom", Corpus: "commentary"},
		{Slug: "private", URL: srv.URL + "/private/feed.rss", Corpus: "news"},
		{Slug: "broken", URL: srv.URL + "/notafeed", Corpus: "news"},
		{Slug: "down", URL: srv.URL + "/missing.rss", Corpus: "news"},
	}}

	stats, err := src.Refresh(ctx)
	if err == nil || !strings.Contains(err.Error(), "feed broken") || !strings.Contains(err.Error(), "feed down") {
		t.Errorf("refresh error = %v, want the broken and down feeds named", err)
	}
	if stats != (newsfeed.Stats{Feeds: 6, RobotsBlocked: 1, Failed: 2, Items: 4, New: 3}) {
		t.Errorf("stats = %+v", stats)
	}

	// Again: nothing new, nothing duplicated.
	n, _ := src.RefreshNews(ctx)
	if n != 0 {
		t.Errorf("second refresh stored %d new items, want 0", n)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_news_items`).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("rows = %d, %v; want 3", rows, err)
	}
	if fetches.Load() == 0 {
		t.Fatal("no feed fetched")
	}

	all, err := src.ListNewsItems(ctx, "all", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range all {
		got = append(got, it.Corpus+":"+it.Source+":"+it.URL)
	}
	want := []string{
		"news:crime:https://news.example.org/b",
		"news:crime:https://news.example.org/a", // first feed to carry it names it
		"commentary:blog:https://blog.example.org/opinion",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("listed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// since, limit and corpus bound the read the way classify-news uses it.
	recent, _ := src.ListNewsItems(ctx, "news", time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), 10)
	if len(recent) != 1 || recent[0].URL != "https://news.example.org/b" {
		t.Errorf("since = %+v", recent)
	}
	one, _ := src.ListNewsItems(ctx, "news", time.Time{}, 1)
	if len(one) != 1 {
		t.Errorf("limit 1 = %d items", len(one))
	}
	if _, err := src.ListNewsItems(ctx, "radio", time.Time{}, 1); err == nil {
		t.Error("unknown corpus accepted")
	}
}
