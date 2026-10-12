// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lonelycode/corruption-tracker/llm"
)

func server(t *testing.T, reply string, got *map[string]any, hdr *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, got)
		*hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
}

const okReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
 "content":[{"type":"text","text":"{\"is_fraud_case\":true}"}],"stop_reason":"end_turn",
 "usage":{"input_tokens":10,"output_tokens":5}}`

func TestCompleteSendsStructuredOutputRequest(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := server(t, okReply, &body, &hdr)
	defer srv.Close()
	c := New(Config{APIKey: "k", BaseURL: srv.URL})
	resp, err := c.Complete(context.Background(), llm.Request{
		System: "sys", User: "text", SchemaName: "fraud_case_v1", MaxTokens: 3072, Temperature: 0,
		Schema: json.RawMessage(`{"type":"object","properties":{"c":{"type":"number","minimum":0}},"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Content) != `{"is_fraud_case":true}` || resp.TotalTokens != 15 {
		t.Errorf("response = %+v", resp)
	}
	if body["model"] != DefaultModel || body["max_tokens"].(float64) != minMaxTokens {
		t.Errorf("model/max_tokens = %v/%v", body["model"], body["max_tokens"])
	}
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must not be sent")
	}
	f := body["output_config"].(map[string]any)["format"].(map[string]any)
	if f["type"] != "json_schema" || strings.Contains(mustJSON(f["schema"]), "minimum") {
		t.Errorf("format = %v", f)
	}
	if body["fallbacks"] != "default" || !strings.Contains(hdr.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("fallbacks not requested: %v / %q", body["fallbacks"], hdr.Get("Anthropic-Beta"))
	}
	if hdr.Get("X-Api-Key") != "k" {
		t.Error("api key header missing")
	}
}

func TestCompleteNoFallbacksForOtherModels(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := server(t, okReply, &body, &hdr)
	defer srv.Close()
	c := New(Config{APIKey: "k", BaseURL: srv.URL, Model: "claude-haiku-5-5"})
	if _, err := c.Complete(context.Background(), llm.Request{User: "x", Schema: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["fallbacks"]; ok {
		t.Error("fallbacks sent for a model without server-side fallback")
	}
}

func TestCompleteRefusal(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := server(t, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],
		"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},
		"usage":{"input_tokens":1,"output_tokens":0}}`, &body, &hdr)
	defer srv.Close()
	_, err := New(Config{APIKey: "k", BaseURL: srv.URL}).Complete(context.Background(),
		llm.Request{User: "x", Schema: json.RawMessage(`{"type":"object"}`)})
	if !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
