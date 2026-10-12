// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package newsfeed

import (
	"strings"
	"testing"
	"time"
)

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>Example News</title>
<item>
  <title>Former accountant jailed for $1.2m fraud</title>
  <link>https://news.example.org/crime/accountant-jailed?utm_source=rss#top</link>
  <description><![CDATA[<p>A former accountant was <b>sentenced</b> today.</p>]]></description>
  <content:encoded><![CDATA[<p>Full story: the High Court heard the offending ran for six years.</p><script>track()</script>]]></content:encoded>
  <pubDate>Mon, 05 Oct 2026 09:30:00 +1300</pubDate>
</item>
<item>
  <title>Weather: rain on the way</title>
  <guid isPermaLink="true">https://news.example.org/weather/rain</guid>
  <dc:date>2026-10-05T08:00:00Z</dc:date>
</item>
<item><title>No link at all</title></item>
</channel></rss>`

func TestParseRSS(t *testing.T) {
	items, err := Parse([]byte(rssFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	a := items[0]
	if a.URL != "https://news.example.org/crime/accountant-jailed?utm_source=rss#top" ||
		a.Title != "Former accountant jailed for $1.2m fraud" ||
		a.Summary != "A former accountant was sentenced today." ||
		a.Body != "Full story: the High Court heard the offending ran for six years." {
		t.Errorf("item 0 = %+v", a)
	}
	if want := time.Date(2026, 10, 4, 20, 30, 0, 0, time.UTC); !a.PublishedAt.Equal(want) {
		t.Errorf("pubDate = %v, want %v", a.PublishedAt, want)
	}
	if b := items[1]; b.URL != "https://news.example.org/weather/rain" || b.PublishedAt.IsZero() {
		t.Errorf("guid/dc:date fallback: %+v", b)
	}
	if items[2].URL != "" {
		t.Errorf("linkless item URL = %q", items[2].URL)
	}
}

func TestParseAtom(t *testing.T) {
	doc := `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Blog</title>
<entry><title type="html">Director convicted of &lt;i&gt;tax&lt;/i&gt; evasion</title>
  <link rel="self" href="https://blog.example.org/self"/>
  <link href="https://blog.example.org/2026/10/director"/>
  <summary>The director pleaded guilty.</summary>
  <updated>2026-10-06T01:00:00Z</updated></entry>
</feed>`
	items, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].URL != "https://blog.example.org/2026/10/director" ||
		items[0].Title != "Director convicted of tax evasion" || items[0].Summary != "The director pleaded guilty." ||
		items[0].PublishedAt.IsZero() {
		t.Errorf("atom = %+v", items)
	}
}

func TestParseLatin1AndErrors(t *testing.T) {
	doc := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss><channel><item><title>Caf\xe9 owner sentenced</title>" +
		"<link>https://x.example/a</link></item></channel></rss>"
	items, err := Parse([]byte(doc))
	if err != nil || len(items) != 1 || items[0].Title != "Café owner sentenced" {
		t.Errorf("latin1 = %+v, %v", items, err)
	}
	for name, doc := range map[string]string{
		"html page":   "<html><body>not a feed</body></html>",
		"empty":       "",
		"bad charset": `<?xml version="1.0" encoding="EBCDIC"?><rss/>`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestPlainCapsLength(t *testing.T) {
	long := strings.Repeat("é", maxText) // 2 bytes each
	if got := plain(long); len(got) > maxText || !strings.HasPrefix(long, got) {
		t.Errorf("plain(long) = %d bytes, valid prefix %v", len(got), strings.HasPrefix(long, got))
	}
}

func TestParseFeeds(t *testing.T) {
	feeds, err := ParseFeeds("# comment\r\n\nherald https://www.nzherald.co.nz/rss\r\nhttps://www.rnz.co.nz/rss/news.xml\nblog https://blog.example.org/feed commentary\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []Feed{
		{Slug: "herald", URL: "https://www.nzherald.co.nz/rss", Corpus: "news"},
		{Slug: "rnz-co-nz", URL: "https://www.rnz.co.nz/rss/news.xml", Corpus: "news"},
		{Slug: "blog", URL: "https://blog.example.org/feed", Corpus: "commentary"},
	}
	if len(feeds) != len(want) {
		t.Fatalf("feeds = %+v", feeds)
	}
	for i := range want {
		if feeds[i] != want[i] {
			t.Errorf("feed %d = %+v, want %+v", i, feeds[i], want[i])
		}
	}
	for _, bad := range []string{
		"x ftp://example.org/feed",
		"Bad_Name https://example.org/feed",
		"a https://example.org/1\na https://example.org/2",
		"a https://example.org/feed opinion",
		"a b c d",
	} {
		if _, err := ParseFeeds(bad); err == nil {
			t.Errorf("ParseFeeds(%q): no error", bad)
		}
	}
	if def, err := ParseFeeds(DefaultFeeds); err != nil || len(def) == 0 {
		t.Errorf("DefaultFeeds = %v, %v", def, err)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://News.Example.org/a?utm_source=rss&id=2&utm_medium=x#c": "https://news.example.org/a?id=2",
		"https://news.example.org/a?b=1&a=2&fbclid=z":                   "https://news.example.org/a?a=2&b=1",
		"https://news.example.org/a":                                    "https://news.example.org/a",
	} {
		if got, ok := normalizeURL(in); !ok || got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "mailto:x@example.org", "/relative"} {
		if _, ok := normalizeURL(bad); ok {
			t.Errorf("normalizeURL(%q) accepted", bad)
		}
	}
}
