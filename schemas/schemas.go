// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package schemas embeds the tracker's LLM extraction schemas.
package schemas

import (
	_ "embed"
	"encoding/json"
)

//go:embed fraud_case_v1.json
var fraudCaseV1 []byte

// FraudCaseV1 returns the schema for extracting a fraud/corruption
// enforcement matter (case-centric: persons are participants) from an
// agency media release or news article. Driven programmatically by the
// tracker's extraction pipeline only; the data is legally sensitive and
// review-gated. (The recode schema is built per run from the live taxonomy
// in the recode package.)
func FraudCaseV1() json.RawMessage { return json.RawMessage(fraudCaseV1) }
