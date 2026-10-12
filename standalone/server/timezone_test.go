// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package server_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/standalone/server"
	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// Pages show timestamps in the site time zone, not the container's UTC:
// a claim created at 13:00 UTC on 10 June was created on
// 11 June in Auckland.
func TestPagesUseTheSiteTimezone(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })

	for _, tc := range []struct {
		name            string
		processTimezone bool
		wantLocal       string
		wantDay         string
	}{
		{"binary (site time zone)", true, "Pacific/Auckland", "11 Jun"},
		{"embedded or test (process zone untouched)", false, "UTC", "10 Jun"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			time.Local = time.UTC // as in the container
			ctx := context.Background()
			st := storetest.Open(t)
			if _, err := st.Pool.Exec(ctx, `INSERT INTO tracker_settings (key, value) VALUES ('site.timezone', 'Pacific/Auckland')`); err != nil {
				t.Fatal(err)
			}
			c, _, err := st.CreateFraudClaim(ctx, domain.FraudClaim{Headline: "Late-evening release",
				SourceType: domain.FraudSourceNews, ContentHash: "tz-" + tc.wantDay}, nil, "seed")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool.Exec(ctx, `UPDATE fraud_claims SET created_at = '2026-06-10T13:00:00Z' WHERE id = $1`, c.ID); err != nil {
				t.Fatal(err)
			}

			s, err := server.Build(ctx, st.Pool, server.Options{Log: quiet(), DataDir: t.TempDir(), ProcessTimezone: tc.processTimezone})
			if err != nil {
				t.Fatal(err)
			}
			if got := time.Local.String(); got != tc.wantLocal {
				t.Errorf("time.Local = %s, want %s", got, tc.wantLocal)
			}
			srv := httptest.NewServer(s.Handler)
			t.Cleanup(srv.Close)
			b := newBrowser(t, srv.URL)
			if resp, _ := b.post("/setup", "/setup", map[string][]string{"username": {"admin"},
				"password": {adminPassword}, "password_confirm": {adminPassword}}); resp.StatusCode != 303 {
				t.Fatalf("setup = %d", resp.StatusCode)
			}
			_, page := b.get("/admin/fraud/claims?state=all")
			row := page[strings.Index(page, "Late-evening release"):]
			if i := strings.Index(row, "</tr>"); i > 0 {
				row = row[:i]
			}
			if !strings.Contains(row, tc.wantDay) {
				t.Errorf("claim row should say %q:\n%s", tc.wantDay, row)
			}
		})
	}
}
