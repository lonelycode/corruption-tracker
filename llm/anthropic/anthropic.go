// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package anthropic is the standalone tracker's Claude client: one
// schema-constrained Messages API call per Complete, using structured
// outputs (output_config.format) for the JSON the extractors parse.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/lonelycode/corruption-tracker/llm"
)

// DefaultModel is used when the operator leaves the model blank.
const DefaultModel = "claude-opus-5-5"

// minMaxTokens leaves room for adaptive thinking ahead of the JSON answer:
// the tracker's per-call budgets (1-3k) were sized for output alone.
const minMaxTokens = 16000

// fallbackModels accept server-side refusal fallbacks ("default" routing).
var fallbackModels = map[string]bool{
	"claude-opus-5-5": true, "claude-opus-5": true, "claude-fable-5-1": true, "claude-sonnet-5-5": true,
}

// Config configures a Client.
type Config struct {
	APIKey  string
	Model   string        // default DefaultModel
	BaseURL string        // tests only
	Timeout time.Duration // default 2 min
}

// Client implements llm.Completer.
type Client struct {
	model  string
	client sdk.Client
}

// New returns a Claude client.
func New(c Config) *Client {
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Minute
	}
	opts := []option.RequestOption{
		option.WithAPIKey(c.APIKey),
		option.WithHTTPClient(&http.Client{Timeout: c.Timeout}),
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	return &Client{model: c.Model, client: sdk.NewClient(opts...)}
}

var _ llm.Completer = (*Client)(nil)

// ErrRefused means Claude's safety classifiers declined the request.
var ErrRefused = errors.New("anthropic: request declined (stop_reason refusal)")

// Complete runs one structured-output request. Sampling parameters are not
// sent: current models reject them, and the schema constrains the output.
func (c *Client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if len(req.Schema) == 0 {
		return llm.Response{}, errors.New("anthropic: schema required")
	}
	schema, err := llm.ProviderSchema(req.Schema)
	if err != nil {
		return llm.Response{}, fmt.Errorf("anthropic: schema: %w", err)
	}
	maxTokens := req.MaxTokens
	if maxTokens < minMaxTokens {
		maxTokens = minMaxTokens
	}
	params := sdk.MessageNewParams{
		Model:     sdk.Model(c.model),
		MaxTokens: int64(maxTokens),
		Messages:  []sdk.MessageParam{sdk.NewUserMessage(sdk.NewTextBlock(req.User))},
		OutputConfig: sdk.OutputConfigParam{
			Format: sdk.JSONOutputFormatParam{Schema: schema},
		},
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	var opts []option.RequestOption
	if fallbackModels[c.model] {
		// On a safety-classifier decline the API re-serves the request on
		// a fallback model chosen by refusal category, inside this call.
		opts = append(opts,
			option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
			option.WithJSONSet("fallbacks", "default"))
	}
	msg, err := c.client.Messages.New(ctx, params, opts...)
	if err != nil {
		return llm.Response{}, fmt.Errorf("anthropic: %w", err)
	}
	if msg.StopReason == sdk.StopReasonRefusal {
		return llm.Response{}, fmt.Errorf("%w: %s", ErrRefused, msg.StopDetails.Explanation)
	}
	if msg.StopReason == sdk.StopReasonMaxTokens {
		return llm.Response{}, fmt.Errorf("anthropic: output hit max_tokens (%d)", maxTokens)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if tb, ok := b.AsAny().(sdk.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	out := strings.TrimSpace(text.String())
	if !json.Valid([]byte(out)) {
		return llm.Response{}, fmt.Errorf("anthropic: response is not valid JSON (stop_reason %s, %d chars)", msg.StopReason, len(out))
	}
	return llm.Response{
		Content:          json.RawMessage(out),
		Model:            string(msg.Model),
		PromptTokens:     int(msg.Usage.InputTokens),
		CompletionTokens: int(msg.Usage.OutputTokens),
		TotalTokens:      int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
	}, nil
}
