// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package web

import (
	"net/http"
	"net/url"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
	"github.com/lonelycode/corruption-tracker/web/internal/render"
)

// Handoff freeze UX (design §11.3). The database
// write guard already refuses every dataset write while the instance is
// frozen or handed off; this turns that into a clear 503 instead of a
// failed write, and gives hosts the banner to show on every page.

// FrozenMessage is the read-only explanation for state ("" when writable).
func FrozenMessage(state store.InstanceState) string {
	switch {
	case state.Writable():
		return ""
	case state.Mode == store.ModeHandedOff:
		msg := "This tracker has been handed off and is read-only."
		if state.SuccessorURL != "" {
			msg += " It continues at " + state.SuccessorURL + "."
		}
		return msg
	default:
		return "This tracker is frozen for a handoff: it is read-only until the handoff completes or is aborted."
	}
}

// InstanceBanner is the banner hosts show on every page for state; nil
// while the instance is writable. The successor link is only offered for
// an https URL.
func InstanceBanner(state store.InstanceState) *host.Banner {
	if state.Writable() {
		return nil
	}
	if state.Mode == store.ModeHandedOff {
		b := &host.Banner{Level: "handed_off", Message: "This tracker has been handed off and is read-only."}
		if u, err := url.Parse(state.SuccessorURL); err == nil && u.Scheme == "https" && u.Host != "" {
			b.Message = "This tracker has been handed off and is read-only. It continues at"
			b.LinkURL, b.LinkText = u.String(), u.Host
		}
		return b
	}
	return &host.Banner{Level: "frozen", Message: "This tracker is frozen for a handoff — read-only until the handoff completes or is aborted."}
}

// blockWritesWhileFrozen answers every non-GET with a 503 page while the
// instance isn't writable. Reads pass. If the state can't be read the
// request goes through: the database guard still refuses dataset writes.
func (h *Handlers) blockWritesWhileFrozen(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		state, err := h.st.GetInstanceState(r.Context())
		if err != nil {
			h.log.Error("fraud back office freeze check", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		msg := FrozenMessage(state)
		if msg == "" {
			next.ServeHTTP(w, r)
			return
		}
		h.rd.Page(w, r, "admin_frozen", h.pages["frozen"], http.StatusServiceUnavailable, render.Root{Data: msg})
	})
}
