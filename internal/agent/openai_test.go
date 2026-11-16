package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIClientUsesChatCompletionsToolProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request path or auth: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "test-model" || request.ToolChoice != "auto" || len(request.Tools) != 1 || request.Tools[0].Type != "function" {
			t.Errorf("unexpected OpenAI request: %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": "call-1", "type": "function",
					"function": map[string]string{"name": "query_logs", "arguments": `{"limit":5}`},
				}},
			}}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13},
		})
	}))
	defer server.Close()

	client := NewOpenAIClient(server.URL+"/v1", "test-key", "test-model", time.Second)
	message, usage, err := client.Complete(context.Background(), []Message{NewTextMessage("user", "investigate")}, []ToolDefinition{{
		Type: "function", Function: FunctionDefinition{Name: "query_logs", Description: "Query logs", Parameters: map[string]any{"type": "object"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Arguments != `{"limit":5}` || usage.TotalTokens != 13 {
		t.Fatalf("message = %+v, usage = %+v", message, usage)
	}
}

func TestOpenAIClientRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}}},
		})
	}))
	defer server.Close()
	client := NewOpenAIClient(server.URL, "key", "model", time.Second)
	message, _, err := client.Complete(context.Background(), []Message{NewTextMessage("user", "go")}, nil)
	if err != nil || message.Content == nil || *message.Content != "done" || calls.Load() != 2 {
		t.Fatalf("message = %+v, calls = %d, err = %v", message, calls.Load(), err)
	}
}

func TestOpenAIClientDoesNotRetryBadRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "invalid tools"}})
	}))
	defer server.Close()
	client := NewOpenAIClient(server.URL, "key", "model", time.Second)
	_, _, err := client.Complete(context.Background(), []Message{NewTextMessage("user", "go")}, nil)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("calls = %d, err = %v", calls.Load(), err)
	}
}
