// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package llm is the tracker's narrow LLM port. Every tracker call is one
// system message plus one user message, both text, with a JSON schema for
// constrained output. The embedding app adapts its own client to Completer;
// the standalone server ships its own clients.
package llm

import (
	"context"
	"encoding/json"
)

// Request is a single schema-constrained completion.
type Request struct {
	System        string
	User          string
	SchemaName    string          // name exposed to the provider's structured-output mode
	Schema        json.RawMessage // JSON Schema for constrained output
	SchemaVersion string          // recorded with extractions; not sent to the model
	MaxTokens     int
	Temperature   float32
}

// Response carries the parsed JSON blob; callers unmarshal into their type.
type Response struct {
	Content          json.RawMessage
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Completer runs one completion.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
