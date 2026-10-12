// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package openaicompat calls any OpenAI-compatible chat-completions
// endpoint (a self-hosted model server, a gateway) with JSON-schema
// constrained output, for operators who don't use Anthropic.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/llm"
)

// Config configures a Client.
type Config struct {
	BaseURL string // e.g. https://llm.example.org/v1
	APIKey  string // optional
	Model   string
	Timeout time.Duration // default 2 min
}

// Client implements llm.Completer.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a client for c.BaseURL.
func New(c Config) *Client {
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Minute
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return &Client{cfg: c, http: &http.Client{Timeout: c.Timeout}}
}

var _ llm.Completer = (*Client)(nil)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Complete runs one schema-constrained chat completion.
func (c *Client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if len(req.Schema) == 0 {
		return llm.Response{}, errors.New("llm: schema required")
	}
	schema, err := llm.ProviderSchema(req.Schema)
	if err != nil {
		return llm.Response{}, fmt.Errorf("llm: schema: %w", err)
	}
	name := req.SchemaName
	if name == "" {
		name = "structured_output"
	}
	var msgs []chatMessage
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: req.User})
	body := map[string]any{
		"model":       c.cfg.Model,
		"messages":    msgs,
		"temperature": req.Temperature,
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": name, "schema": schema, "strict": true},
		},
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	b, err := json.Marshal(body)
	if err != nil {
		return llm.Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return llm.Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return llm.Response{}, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return llm.Response{}, err
	}
	if resp.StatusCode/100 != 2 {
		return llm.Response{}, fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return llm.Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return llm.Response{}, errors.New("llm: no choices in response")
	}
	content := stripFence(out.Choices[0].Message.Content)
	if !json.Valid([]byte(content)) {
		return llm.Response{}, fmt.Errorf("llm: response is not valid JSON (finish_reason %q, %d chars)",
			out.Choices[0].FinishReason, len(content))
	}
	return llm.Response{
		Content: json.RawMessage(content), Model: out.Model,
		PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens,
		TotalTokens: out.Usage.TotalTokens,
	}, nil
}

// stripFence removes a ```json fence some servers wrap around the output.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
