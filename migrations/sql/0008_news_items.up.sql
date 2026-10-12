-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0008: tracker_news_items, the standalone news corpus.
--
-- Embedded in alitheia, classify-news reads alitheia's own news and
-- commentary corpus. Standalone has none, so the newsfeed connector polls
-- the operator's RSS/Atom feeds (Settings → News feeds) into this table and
-- classify-news reads it back (design §5.6).
--
-- One row per article URL: url_hash is sha256 of the normalised URL, so a
-- story syndicated into two feeds, or seen again on the next poll, is
-- stored once. It is dataset (exported in bundles, §10.4), so a successor
-- does not re-classify what the source already saw.

CREATE TABLE tracker_news_items (
    id           BIGSERIAL   PRIMARY KEY,
    url_hash     TEXT        NOT NULL UNIQUE,
    url          TEXT        NOT NULL,
    feed         TEXT        NOT NULL, -- the feed's slug in Settings
    corpus       TEXT        NOT NULL DEFAULT 'news' CHECK (corpus IN ('news', 'commentary')),
    title        TEXT        NOT NULL DEFAULT '',
    summary      TEXT        NOT NULL DEFAULT '',
    body         TEXT        NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- classify-news reads newest first within a corpus, bounded by --days.
CREATE INDEX tracker_news_items_corpus_recent
    ON tracker_news_items (corpus, (COALESCE(published_at, fetched_at)) DESC);
