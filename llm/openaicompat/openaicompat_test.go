// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lonelycode/corruption-tracker/llm"
)

func TestComplete(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("{\"model\":\"m\",\"choices\":[{\"finish_reason\":\"stop\",\"message\":{\"content\":\"```json\\n{\\\"ok\\\":true}\\n```\"}}],\"usage\":{\"total_tokens\":3}}"))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL + "/v1/", APIKey: "k", Model: "m"})
	resp, err := c.Complete(context.Background(), llm.Request{System: "s", User: "u", SchemaName: "n",
		Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"number","maximum":1}}}`), MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Content) != `{"ok":true}` || resp.TotalTokens != 3 || auth != "Bearer k" {
		t.Errorf("resp %+v auth %q", resp, auth)
	}
	rf := body["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if rf["name"] != "n" || rf["strict"] != true {
		t.Errorf("response_format = %v", rf)
	}
	if b, _ := json.Marshal(rf["schema"]); string(b) != `{"properties":{"x":{"type":"number"}},"type":"object"}` {
		t.Errorf("schema not stripped: %s", b)
	}
}

func TestCompleteHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := New(Config{BaseURL: srv.URL}).Complete(context.Background(),
		llm.Request{User: "u", Schema: json.RawMessage(`{"type":"object"}`)}); err == nil {
		t.Error("401 should be an error")
	}
}
