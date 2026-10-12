// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package newsfeed

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// Item is one parsed feed entry.
type Item struct {
	URL         string
	Title       string
	Summary     string    // plain text
	Body        string    // plain text; only when the feed carries full text
	PublishedAt time.Time // zero when the feed gives no usable date
}

// maxText caps each stored text field: classify-news needs the story, not
// the page furniture some feeds inline.
const maxText = 20000

// Parse reads an RSS 2.0 or Atom document.
func Parse(doc []byte) ([]Item, error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	dec.Strict = false
	dec.CharsetReader = charsetReader
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("not a feed: no root element")
			}
			return nil, fmt.Errorf("not a feed: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "rss":
			var d rssDoc
			if err := dec.DecodeElement(&d, &start); err != nil {
				return nil, fmt.Errorf("rss: %w", err)
			}
			return d.items(), nil
		case "feed":
			var d atomDoc
			if err := dec.DecodeElement(&d, &start); err != nil {
				return nil, fmt.Errorf("atom: %w", err)
			}
			return d.items(), nil
		default:
			return nil, fmt.Errorf("not a feed: root element <%s>", start.Name.Local)
		}
	}
}

type rssDoc struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			GUID        string `xml:"guid"`
			Description string `xml:"description"`
			Encoded     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
			PubDate     string `xml:"pubDate"`
			DCDate      string `xml:"http://purl.org/dc/elements/1.1/ date"`
		} `xml:"item"`
	} `xml:"channel"`
}

func (d rssDoc) items() []Item {
	out := make([]Item, 0, len(d.Channel.Items))
	for _, it := range d.Channel.Items {
		link := strings.TrimSpace(it.Link)
		if link == "" && isHTTP(it.GUID) {
			link = strings.TrimSpace(it.GUID)
		}
		date := it.PubDate
		if date == "" {
			date = it.DCDate
		}
		out = append(out, Item{
			URL:         link,
			Title:       plain(it.Title),
			Summary:     plain(it.Description),
			Body:        plain(it.Encoded),
			PublishedAt: parseDate(date),
		})
	}
	return out
}

type atomDoc struct {
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
	} `xml:"entry"`
}

func (d atomDoc) items() []Item {
	out := make([]Item, 0, len(d.Entries))
	for _, e := range d.Entries {
		var link string
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = strings.TrimSpace(l.Href)
				break
			}
		}
		date := e.Published
		if date == "" {
			date = e.Updated
		}
		out = append(out, Item{
			URL:         link,
			Title:       plain(e.Title),
			Summary:     plain(e.Summary),
			Body:        plain(e.Content),
			PublishedAt: parseDate(date),
		})
	}
	return out
}

// plain turns feed text (often escaped HTML) into whitespace-collapsed
// plain text.
func plain(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, "<&") {
		if doc, err := goquery.NewDocumentFromReader(strings.NewReader(s)); err == nil {
			doc.Find("script, style").Remove()
			s = doc.Text()
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxText {
		s = s[:maxText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

var dateLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04 -0700",
	"2006-01-02T15:04:05", "2006-01-02",
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func isHTTP(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// charsetReader accepts the encodings NZ feeds use. Anything else is an
// error rather than mojibake in the review queue.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "latin1", "latin-1":
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return strings.NewReader(string(runes)), nil
	}
	return nil, fmt.Errorf("unsupported feed encoding %q", label)
}
