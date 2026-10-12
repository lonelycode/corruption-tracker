// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lonelycode/corruption-tracker/ingest/fraudagency"
	"github.com/lonelycode/corruption-tracker/llm"
	"github.com/lonelycode/corruption-tracker/llm/anthropic"
	"github.com/lonelycode/corruption-tracker/llm/openaicompat"
)

// LLMConfig is the LLM part of the settings.
type LLMConfig struct {
	Provider string
	APIKey   string
	Model    string
	BaseURL  string
}

// NewCompleter builds the client for c.
func NewCompleter(c LLMConfig) llm.Completer {
	if c.Provider == ProviderOpenAICompatible {
		return openaicompat.New(openaicompat.Config{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Model})
	}
	return anthropic.New(anthropic.Config{APIKey: c.APIKey, Model: c.Model})
}

// TestLLM makes one tiny structured call with the current settings and
// returns a message an operator can act on.
func (s *Service) TestLLM(ctx context.Context) (string, error) {
	c, ok := s.LLMConfig()
	if !ok {
		if c.Provider == ProviderOpenAICompatible {
			return "", errors.New("Set the base URL first.")
		}
		return "", errors.New("Set the API key first.")
	}
	comp := s.newLLM(c)
	resp, err := comp.Complete(ctx, llm.Request{
		System:     "You check that an integration works.",
		User:       `Reply with {"ok": true}.`,
		SchemaName: "connection_check",
		Schema:     json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
		MaxTokens:  64,
	})
	if err != nil {
		return "", fmt.Errorf("The LLM call failed: %v", err)
	}
	var out struct{ OK bool }
	if json.Unmarshal(resp.Content, &out) != nil || !out.OK {
		return "", fmt.Errorf("The model answered, but not as expected: %s", resp.Content)
	}
	model := resp.Model
	if model == "" {
		model = c.Model
	}
	return fmt.Sprintf("Connected: %s answered a structured request.", model), nil
}

// TestIMAP connects to the configured mailbox with the current settings.
func (s *Service) TestIMAP() (string, error) {
	c, ok := s.IMAP()
	if !ok {
		return "", errors.New("Set the mail server first.")
	}
	n, err := fraudagency.CheckIMAP(c)
	if err != nil {
		return "", fmt.Errorf("Could not open the mailbox: %v", err)
	}
	return fmt.Sprintf("Connected: %q holds %d messages.", c.Folder, n), nil
}
