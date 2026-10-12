// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package reader

import (
	"context"
	"net/http"
	"testing"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/readmodel/contracttest"
)

// bareLayout writes the page body with no chrome.
type bareLayout struct{}

func (bareLayout) Render(w http.ResponseWriter, _ *http.Request, pg host.Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(pg.Body))
}
func (bareLayout) CSRFToken(*http.Request) string               { return "" }
func (bareLayout) SetFlash(http.ResponseWriter, string, string) {}

type anyAuth struct{}

func (anyAuth) Principal(*http.Request) (host.Principal, bool) { return host.Principal{}, true }
func (anyAuth) Access(context.Context, *http.Request) (host.Access, error) {
	return host.Access{Operate: true}, nil
}
func (anyAuth) LoginURL(*http.Request) string { return "/login" }

// The default presentation meets the obligations core can't enforce: the
// ALLEGED mark on pre-verdict entries and the notice on every page.
func TestReaderMeetsPresentationContract(t *testing.T) {
	rr, err := New(Options{Auth: anyAuth{}, Layout: bareLayout{}})
	if err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, rr, contracttest.Paths{
		Report: Base + "?period=2024-W23", Case: Base + "/case/1", Explorer: Base + "/explorer",
	})
}
