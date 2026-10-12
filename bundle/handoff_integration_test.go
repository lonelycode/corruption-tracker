// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle_test

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lonelycode/corruption-tracker/bundle"
	"github.com/lonelycode/corruption-tracker/cli"
	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// bundledDedupeKeys reads fraud_claims.ndjson out of a bundle.
func bundledDedupeKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	keys := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return keys
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "tables/fraud_claims.ndjson" {
			continue
		}
		sc := bufio.NewScanner(tr)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var row struct {
				DedupeKey *string `json:"dedupe_key"`
			}
			if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
			if row.DedupeKey != nil {
				keys[*row.DedupeKey] = true
			}
		}
	}
}

// The handoff acceptance test: ingestion keeps writing
// while the source freezes. Every claim either lands in the handoff bundle
// or failed on the source (none silently lost); the target's first poll
// after resume duplicates nothing that was bundled and picks up items
// published after the cutoff.
func TestHandoffUnderConcurrentIngestion(t *testing.T) {
	src := storetest.Open(t)
	ctx := context.Background()

	// 1. Writers create claims continuously.
	type outcome struct {
		committed bool
		err       error
	}
	var mu sync.Mutex
	results := map[string]outcome{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("url:https://agency.test/w%d/%d", w, n)
				_, _, err := src.CreateFraudClaim(ctx, domain.FraudClaim{
					Headline: "Release " + key, SourceType: domain.FraudSourceAgencyRelease,
					SourcePublisher: "Agency", SourceURL: key[4:], LicenceBasis: "test",
					ContentHash: "h-" + key, DedupeKey: &key,
				}, nil, "writer")
				mu.Lock()
				results[key] = outcome{committed: err == nil, err: err}
				mu.Unlock()
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)

	// 2. Freeze mid-stream. The writers keep going: the bundle below is
	// taken while they still try to write, so a claim that slipped in after
	// the freeze would be committed yet missing from the bundle.
	if _, err := src.Freeze(ctx, "test", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	// 3. The handoff bundle, through the CLI as an operator runs it.
	deps := cli.Deps{
		Open:       func(context.Context) (*cli.Conn, error) { return &cli.Conn{Store: src}, nil },
		Log:        quiet(),
		SourceKind: bundle.KindStandalone,
	}
	out := t.TempDir()
	if err := cli.Run(ctx, deps, []string{"export", "--handoff", "--out", out}); err != nil {
		t.Fatalf("export --handoff: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	var committed, refused int
	for key, o := range results {
		if o.committed {
			committed++
			continue
		}
		var pg *pgconn.PgError
		if !errors.As(o.err, &pg) || pg.Code != "25006" {
			t.Errorf("%s failed with %v, want only read-only refusals", key, o.err)
		}
		refused++
	}
	if committed == 0 || refused == 0 {
		t.Fatalf("test didn't straddle the freeze: %d committed, %d refused", committed, refused)
	}

	files, _ := filepath.Glob(filepath.Join(out, "*.tar.gz"))
	if len(files) != 1 {
		t.Fatalf("bundles = %v", files)
	}
	m, err := bundle.VerifyFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	state, _ := src.GetInstanceState(ctx)
	if m.Handoff == nil || m.Handoff.FreezeID != state.FreezeID || state.HandoffExportID != m.ExportID {
		t.Fatalf("handoff manifest %+v, source state %+v", m.Handoff, state)
	}

	// Every committed claim is in the bundle; nothing refused is.
	inBundle := bundledDedupeKeys(t, files[0])
	for key, o := range results {
		if o.committed != inBundle[key] {
			t.Errorf("%s: committed=%v but in bundle=%v", key, o.committed, inBundle[key])
		}
	}

	// 4. Target: import, resume.
	dst := storetest.Open(t)
	if _, err := bundle.ImportFile(ctx, dst.Pool, files[0], bundle.ImportOptions{Log: quiet()}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := dst.ResumeAfterImport(ctx, []string{"agency:fake"}); err != nil {
		t.Fatal(err)
	}
	cu, err := dst.GetConnectorCatchUp(ctx, "agency:fake")
	if err != nil || !cu.CatchUp || cu.ResumeFrom == nil || !cu.ResumeFrom.Before(*state.FrozenAt) {
		t.Fatalf("catch-up after resume = %+v %v", cu, err)
	}

	// 5. The target's first poll: an index mixing bundled releases with two
	// published after the cutoff, polled with --limit 1.
	var bundledURLs []string
	for key, o := range results {
		if o.committed && len(bundledURLs) < 3 {
			bundledURLs = append(bundledURLs, key[4:])
		}
	}
	mux := http.NewServeMux()
	page := `<html><head><title>Sentenced for fraud</title></head><body>The defendant was convicted of fraud and sentenced in the District Court.</body></html>`
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	newURLs := []string{srv.URL + "/after-cutoff-1", srv.URL + "/after-cutoff-2"}
	index := []fraudagency.Item{{URL: newURLs[0]}}
	for _, u := range bundledURLs {
		index = append(index, fraudagency.Item{URL: u})
	}
	index = append(index, fraudagency.Item{URL: newURLs[1]})
	fake := fraudagency.Source{Slug: "fake", Label: "Fake Agency", LicenceBasis: "test",
		RobotsURL: srv.URL + "/robots.txt", PathPrefix: "/",
		FetchIndex: func(context.Context, *fraudagency.Fetcher) ([]fraudagency.Item, error) { return index, nil }}
	pd := &fraudagency.Deps{DB: dst, Log: quiet(), Limit: 1, Delay: time.Millisecond, CatchUp: cu.CatchUp}
	st, err := pd.Poll(ctx, fake)
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped != len(bundledURLs) {
		t.Errorf("skipped %d bundled releases, want %d", st.Skipped, len(bundledURLs))
	}
	for _, u := range newURLs {
		var n int
		if err := dst.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_claims WHERE dedupe_key = $1`, "url:"+u).Scan(&n); err != nil || n != 1 {
			t.Errorf("post-cutoff release %s: %d claims (%v), want 1", u, n, err)
		}
	}
	for _, u := range bundledURLs {
		var n int
		_ = dst.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_claims WHERE dedupe_key = $1`, "url:"+u).Scan(&n)
		if n != 1 {
			t.Errorf("bundled release %s: %d claims on the target, want exactly 1", u, n)
		}
	}
	var total int
	_ = dst.Pool.QueryRow(ctx, `SELECT count(*) FROM fraud_claims`).Scan(&total)
	if total != committed+len(newURLs) {
		t.Errorf("target claims = %d, want %d bundled + %d new", total, committed, len(newURLs))
	}
	if err := dst.ConnectorRan(ctx, "agency:fake", true); err != nil {
		t.Fatal(err)
	}
	if cu, _ := dst.GetConnectorCatchUp(ctx, "agency:fake"); cu.CatchUp {
		t.Error("catch-up not cleared after a successful run")
	}

	// 6. Source: complete. It stays read-only and points at the successor.
	if err := cli.Run(ctx, deps, []string{"handoff", "complete", "--export-id", m.ExportID, "--successor-url", "https://next.example"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := src.GetInstanceState(ctx); st.Mode != store.ModeHandedOff {
		t.Errorf("source after complete: %+v", st)
	}
}
