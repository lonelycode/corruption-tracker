// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package host declares the ports an embedding application implements to
// run the tracker: identity, layout, audit, settings and the news corpus.
// See design §5.
package host

import (
	"context"
	"time"
)

// NewsItem is one scannable news/commentary record for the classify-news
// pipeline.
type NewsItem struct {
	Corpus      string // "news" | "commentary"
	Source      string // publisher slug
	URL         string
	Title       string
	Summary     string
	Body        string
	PublishedAt time.Time // zero when unknown
}

// NewsItemSource lists corpus items for classify-news. corpus is "news",
// "commentary" or "all"; limit applies per corpus; a zero since means
// unbounded. Items come newest first within each corpus.
type NewsItemSource interface {
	ListNewsItems(ctx context.Context, corpus string, since time.Time, limit int) ([]NewsItem, error)
}
