// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/ingest/newsfeed"
	"github.com/lonelycode/corruption-tracker/llm"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// extractor answers like the real extraction prompt would: the sentencing
// story is an enforcement matter, anything else is not.
type extractor struct{ calls int }

func (e *extractor) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	e.calls++
	if !strings.Contains(req.User, "sentenced") {
		return llm.Response{Content: json.RawMessage(`{"is_fraud_case":false,"confidence":0.9}`)}, nil
	}
	return llm.Response{Content: json.RawMessage(`{"is_fraud_case":true,"case_stage":"sentenced",
		"persons":[{"name_as_written":"Feed Fixture Person","role":"defendant"}],
		"typology_hint":"dishonestly using a document","event_date":"2026-10-05","total_amount_nzd":120000,
		"exact_excerpt":"sentenced to two years","confidence":0.9}`)}, nil
}

// A fresh standalone instance with only feeds configured gets claims from
// one classify-news run (the scheduled job's own arguments), and running
// it again adds no items, claims or LLM calls.
func TestClassifyNewsFromFeedsOnAFreshInstance(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	today := time.Now().UTC().Format(time.RFC1123Z)
	mux := http.NewServeMux()
	mux.HandleFunc("/crime.rss", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Crime</title>
<item><title>Bookkeeper sentenced for $120,000 fraud</title><link>https://news.example.org/bookkeeper</link>
  <description>A bookkeeper was sentenced to two years after pleading guilty to dishonestly using a document.</description>
  <pubDate>` + today + `</pubDate></item>
<item><title>Council approves new cycleway</title><link>https://news.example.org/cycleway</link>
  <description>The council voted on Tuesday.</description><pubDate>` + today + `</pubDate></item>
<item><title>Tax appeal dismissed</title><link>https://news.example.org/leaky</link>
  <description>The Court of Appeal dismissed a company's appeal over a disputed tax assessment.</description><pubDate>` + today + `</pubDate></item>
</channel></rss>`))
	})
	srv := httptest.NewServer(mux) // no robots.txt: 404, the web's default-allow
	defer srv.Close()

	ext := &extractor{}
	var logs bytes.Buffer
	d := cli.Deps{
		Open: func(context.Context) (*cli.Conn, error) {
			return &cli.Conn{Store: st, News: &newsfeed.Source{Pool: st.Pool, Fetcher: fraudagency.NewFetcher(),
				Feeds: []newsfeed.Feed{{Slug: "crime", URL: srv.URL + "/crime.rss", Corpus: "news"}}}}, nil
		},
		LLM: func() llm.Completer { return ext },
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	run := func() {
		t.Helper()
		if err := cli.Run(ctx, d, []string{"classify-news", "--days", "7"}); err != nil {
			t.Fatalf("classify-news: %v\n%s", err, logs.String())
		}
	}
	claims := func() map[string]int {
		t.Helper()
		rows, err := st.Pool.Query(ctx, `SELECT source_url, review_state FROM fraud_claims`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var u, state string
			if err := rows.Scan(&u, &state); err != nil {
				t.Fatal(err)
			}
			out[u+" "+state]++
		}
		return out
	}

	run()
	first := claims()
	// The cycleway story has no enforcement vocabulary, so it never reaches
	// the LLM; the civil tax appeal does, and is auto-rejected.
	if first["https://news.example.org/bookkeeper pending"] != 1 || first["https://news.example.org/leaky rejected"] != 1 || len(first) != 2 {
		t.Fatalf("claims after one run = %v\n%s", first, logs.String())
	}
	if ext.calls != 2 {
		t.Errorf("LLM calls = %d, want 2", ext.calls)
	}

	run()
	if again := claims(); len(again) != 2 || again["https://news.example.org/bookkeeper pending"] != 1 {
		t.Errorf("claims after a second run = %v", again)
	}
	if ext.calls != 2 {
		t.Errorf("second run called the LLM again: %d calls", ext.calls)
	}
	var items int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM tracker_news_items`).Scan(&items); err != nil || items != 3 {
		t.Errorf("news items = %d, %v; want 3", items, err)
	}
}
