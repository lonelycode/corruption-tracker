// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package llm

import (
	"encoding/json"
	"testing"
)

func TestProviderSchemaStripsUnsupportedConstraints(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"c":{"type":"number","minimum":0,"maximum":1},
		"xs":{"type":"array","minItems":1,"items":{"type":"string","maxLength":5}}},"additionalProperties":false}`)
	got, err := ProviderSchema(in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `{"additionalProperties":false,"properties":{"c":{"type":"number"},"xs":{"items":{"type":"string"},"type":"array"}},"type":"object"}`
	if string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
}
