// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package newsfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
)

// Source polls the configured feeds into tracker_news_items and lists them
// for classify-news.
type Source struct {
	Pool    *pgxpool.Pool
	Fetcher *fraudagency.Fetcher
	Feeds   []Feed
	Log     *slog.Logger
}

var _ host.NewsItemSource = (*Source)(nil)

// Stats summarises one refresh.
type Stats struct {
	Feeds         int
	RobotsBlocked int
	Failed        int
	Items         int // entries read from the feeds
	New           int // entries not seen before
}

// RefreshNews polls every feed once and stores the entries it has not seen
// (classify-news calls it before listing). A feed whose robots.txt now
// disallows it is skipped, not an error. A feed that fails to fetch or
// parse doesn't stop the others; the failures come back joined, after the
// rest are stored.
func (s *Source) RefreshNews(ctx context.Context) (int, error) {
	st, err := s.Refresh(ctx)
	return st.New, err
}

// Refresh is RefreshNews with the full counts.
func (s *Source) Refresh(ctx context.Context) (Stats, error) {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	var st Stats
	var errs []error
	for _, f := range s.Feeds {
		st.Feeds++
		ok, err := fraudagency.RobotsAllowsURL(ctx, s.Fetcher, f.URL)
		if err != nil {
			st.Failed++
			errs = append(errs, fmt.Errorf("feed %s: %w", f.Slug, err))
			continue
		}
		if !ok {
			st.RobotsBlocked++
			log.Warn("news feed skipped: robots.txt disallows it", "feed", f.Slug, "url", f.URL)
			continue
		}
		body, err := s.Fetcher.Get(ctx, f.URL)
		if err != nil {
			st.Failed++
			errs = append(errs, fmt.Errorf("feed %s: %w", f.Slug, err))
			continue
		}
		items, err := Parse([]byte(body))
		if err != nil {
			st.Failed++
			errs = append(errs, fmt.Errorf("feed %s: %w", f.Slug, err))
			continue
		}
		n, err := s.store(ctx, f, items)
		if err != nil {
			return st, err // the database, not the feed: stop
		}
		st.Items += len(items)
		st.New += n
		log.Info("news feed polled", "feed", f.Slug, "items", len(items), "new", n)
	}
	return st, errors.Join(errs...)
}

func (s *Source) store(ctx context.Context, f Feed, items []Item) (int, error) {
	n := 0
	for _, it := range items {
		norm, ok := normalizeURL(it.URL)
		if !ok || (it.Title == "" && it.Summary == "" && it.Body == "") {
			continue
		}
		var published *time.Time
		if !it.PublishedAt.IsZero() {
			published = &it.PublishedAt
		}
		tag, err := s.Pool.Exec(ctx, `
			INSERT INTO tracker_news_items (url_hash, url, feed, corpus, title, summary, body, published_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (url_hash) DO NOTHING`,
			urlHash(norm), norm, f.Slug, f.Corpus, it.Title, it.Summary, it.Body, published)
		if err != nil {
			return n, fmt.Errorf("store news item: %w", err)
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// ListNewsItems implements host.NewsItemSource over tracker_news_items:
// newest first within each corpus, news then commentary, limit per corpus.
// An item with no published date sorts by when it was fetched.
func (s *Source) ListNewsItems(ctx context.Context, corpus string, since time.Time, limit int) ([]host.NewsItem, error) {
	var corpora []string
	switch corpus {
	case "news", "commentary":
		corpora = []string{corpus}
	case "all", "":
		corpora = []string{"news", "commentary"}
	default:
		return nil, fmt.Errorf("unknown corpus %q (news, commentary or all)", corpus)
	}
	var out []host.NewsItem
	for _, c := range corpora {
		rows, err := s.Pool.Query(ctx, `
			SELECT feed, url, title, summary, body, published_at
			  FROM tracker_news_items
			 WHERE corpus = $1 AND ($2::timestamptz IS NULL OR COALESCE(published_at, fetched_at) >= $2)
			 ORDER BY COALESCE(published_at, fetched_at) DESC, id DESC
			 LIMIT $3`, c, nullTime(since), limit)
		if err != nil {
			return nil, fmt.Errorf("list news items: %w", err)
		}
		for rows.Next() {
			it := host.NewsItem{Corpus: c}
			var published *time.Time
			if err := rows.Scan(&it.Source, &it.URL, &it.Title, &it.Summary, &it.Body, &published); err != nil {
				rows.Close()
				return nil, err
			}
			if published != nil {
				it.PublishedAt = *published
			}
			out = append(out, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// trackingParams are query parameters that vary per share, not per story.
var trackingParams = []string{"utm_", "fbclid", "gclid", "mc_cid", "mc_eid", "ref", "cmp"}

// normalizeURL makes one story's URL compare equal across feeds and polls:
// lowercase scheme and host, no fragment, no tracking parameters, the rest
// of the query kept in a stable order.
func normalizeURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		for _, p := range trackingParams {
			if lk == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(lk, p)) {
				q.Del(k)
				break
			}
		}
	}
	u.RawQuery = q.Encode() // sorted by key
	return u.String(), true
}

func urlHash(norm string) string {
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}
