// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package llm

import "encoding/json"

// unsupportedSchemaKeywords are JSON Schema constraints that providers'
// structured-output modes reject or ignore (numeric bounds, string
// lengths, array sizes). The tracker validates those semantics itself
// after parsing, so they are dropped from what is sent.
var unsupportedSchemaKeywords = map[string]bool{
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"multipleOf": true, "minLength": true, "maxLength": true,
	"minItems": true, "maxItems": true, "uniqueItems": true,
}

// ProviderSchema returns schema as a map with the constraints structured
// outputs do not support removed, at every level.
func ProviderSchema(schema json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		return nil, err
	}
	strip(m)
	return m, nil
}

func strip(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if unsupportedSchemaKeywords[k] {
				delete(t, k)
			}
		}
		for _, c := range t {
			strip(c)
		}
	case []any:
		for _, c := range t {
			strip(c)
		}
	}
}
