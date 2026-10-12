// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/jobs"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// One agency failing (its robots.txt unreachable here) no longer stops the
// run: the sources after it are still polled, every attempt is recorded on
// its connector, a failed catch-up stays on, and the run's error names the
// failed source.
func TestPollContinuesPastAFailingSource(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/down/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux) // other robots.txt paths 404: the web's default-allow
	defer srv.Close()

	indexed := map[string]bool{}
	src := func(slug string) fraudagency.Source {
		return fraudagency.Source{Slug: slug, RobotsURL: srv.URL + "/" + slug + "/robots.txt", PathPrefix: "/",
			FetchIndex: func(context.Context, *fraudagency.Fetcher) ([]fraudagency.Item, error) {
				indexed[slug] = true
				return nil, nil
			}}
	}
	sources := []fraudagency.Source{src("first"), src("down"), src("last"), src("off")}
	if err := st.SetConnectorEnabled(ctx, jobs.ConnectorAgencyPrefix+"off", false); err != nil {
		t.Fatal(err)
	}
	// Both "down" and "last" are catching up after a handoff.
	for _, c := range []string{"down", "last"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO tracker_connector_state (connector, catch_up) VALUES ($1, true)`,
			jobs.ConnectorAgencyPrefix+c); err != nil {
			t.Fatal(err)
		}
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	deps := &fraudagency.Deps{DB: st, Fetcher: fraudagency.NewFetcher(), Log: log, Delay: -1}
	_, _, err := pollSources(ctx, &Conn{Store: st}, deps, sources, false, log)
	if err == nil || !strings.Contains(err.Error(), "1 of 3 sources failed: down:") {
		t.Fatalf("err = %v, want the down source named", err)
	}
	if !indexed["first"] || !indexed["last"] || indexed["down"] || indexed["off"] {
		t.Errorf("indexed = %v, want first and last (down failed before its index, off is disabled)", indexed)
	}
	if !strings.Contains(logs.String(), "continuing with the others") {
		t.Errorf("the failure was not logged:\n%s", logs.String())
	}

	states, err := st.ConnectorStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for slug, wantSuccess := range map[string]bool{"first": true, "down": false, "last": true} {
		s := states[jobs.ConnectorAgencyPrefix+slug]
		if s.LastRunAt == nil || (s.LastSuccessAt != nil) != wantSuccess {
			t.Errorf("%s state = run %v success %v, want a run and success=%v", slug, s.LastRunAt, s.LastSuccessAt, wantSuccess)
		}
	}
	for slug, want := range map[string]bool{"down": true, "last": false} {
		cu, err := st.GetConnectorCatchUp(ctx, jobs.ConnectorAgencyPrefix+slug)
		if err != nil || cu.CatchUp != want {
			t.Errorf("%s catch_up = %v, %v; want %v", slug, cu.CatchUp, err, want)
		}
	}

	// A dry run records nothing.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM tracker_connector_state WHERE connector LIKE 'agency:%' AND enabled`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pollSources(ctx, &Conn{Store: st}, deps, sources[:1], true, log); err != nil {
		t.Fatal(err)
	}
	if states, _ := st.ConnectorStates(ctx); states[jobs.ConnectorAgencyPrefix+"first"].LastRunAt != nil {
		t.Error("a dry run recorded a connector run")
	}
}
