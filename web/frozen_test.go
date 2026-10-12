// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package web

import (
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/store"
)

func TestInstanceBannerAndMessage(t *testing.T) {
	if InstanceBanner(store.InstanceState{Mode: store.ModeNormal}) != nil || FrozenMessage(store.InstanceState{Mode: store.ModeNormal}) != "" {
		t.Error("a writable instance has no banner or message")
	}
	b := InstanceBanner(store.InstanceState{Mode: store.ModeFrozen})
	if b == nil || b.Level != "frozen" || b.LinkURL != "" {
		t.Errorf("frozen banner = %+v", b)
	}
	for successor, linked := range map[string]bool{
		"https://next.example.org/x": true, "http://next.example.org": false,
		"javascript:alert(1)": false, "": false, "//evil.example": false,
	} {
		b := InstanceBanner(store.InstanceState{Mode: store.ModeHandedOff, SuccessorURL: successor})
		if b == nil || b.Level != "handed_off" || (b.LinkURL != "") != linked {
			t.Errorf("handed off to %q: %+v (linked=%v)", successor, b, linked)
		}
	}
	if msg := FrozenMessage(store.InstanceState{Mode: store.ModeHandedOff, SuccessorURL: "https://n.example"}); !strings.Contains(msg, "continues at https://n.example") {
		t.Errorf("handed-off message = %q", msg)
	}
}
