package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responsesClientForTest(t *testing.T, handler http.Handler) (*OpenAIResponsesClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	client := testResponsesClient(t, srv.URL, "responses-test-key")
	return client, srv
}

func writeResponsesEvent(w io.Writer, payload string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", responseEventType(payload), payload)
}

func responseEventType(payload string) string {
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(payload), &event)
	return event.Type
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty(" \t", "fallback"); got != "fallback" {
		t.Fatalf("firstNonEmpty() = %q, want fallback", got)
	}
}

func TestOpenAIResponsesRequestTranslation(t *testing.T) {
	tool := NewTool("read", "Read a file", `{"type":"object","properties":{"path":{"type":"string"}}}`)
	call := ToolCall{ID: "call-1", Type: "function"}
	call.Function.Name = "read"
	call.Function.Arguments = `{"path":"README.md"}`
	wire, err := newOpenAIResponsesRequest(Request{
		Model: "grok-test",
		Messages: []Message{
			{Role: "system", Content: "Be concise."},
			{Role: "user", Content: "Look at this", Parts: []ContentPart{ImagePart("png", []byte{1, 2})}},
			{Role: "assistant", ToolCalls: []ToolCall{call}, ProviderBlocks: []json.RawMessage{
				json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[]}`),
				json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call-1","name":"read","arguments":"{\"path\":\"README.md\"}","status":"completed"}`),
			}},
			{Role: "tool", Content: "file contents", ToolCallID: "call-1"},
		},
		Tools:           []Tool{tool},
		MaxTokens:       123,
		ReasoningEffort: "high",
		SessionID:       "sess-test",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model           string           `json:"model"`
		Instructions    string           `json:"instructions"`
		Input           []map[string]any `json:"input"`
		Tools           []map[string]any `json:"tools"`
		MaxOutputTokens int              `json:"max_output_tokens"`
		Reasoning       map[string]any   `json:"reasoning"`
		Stream          bool             `json:"stream"`
		Store           *bool            `json:"store"`
		PromptCacheKey  string           `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "grok-test" || got.Instructions != "Be concise." || got.MaxOutputTokens != 123 || !got.Stream {
		t.Fatalf("request fields: %+v", got)
	}
	if got.Store == nil || *got.Store != false {
		t.Fatalf("store: got %v, want false", got.Store)
	}
	if got.PromptCacheKey != "sess-test" {
		t.Fatalf("prompt_cache_key: got %q, want sess-test", got.PromptCacheKey)
	}
	if got.Reasoning["effort"] != "high" {
		t.Fatalf("reasoning effort: %+v", got.Reasoning)
	}
	if len(got.Input) != 4 {
		t.Fatalf("input should contain user, preserved reasoning/function call, and tool output: %+v", got.Input)
	}
	if got.Input[0]["type"] != "message" || got.Input[0]["role"] != "user" {
		t.Fatalf("user input item: %+v", got.Input[0])
	}
	content, ok := got.Input[0]["content"].([]any)
	if !ok || len(content) != 2 || content[0].(map[string]any)["type"] != "input_text" || content[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("multimodal input: %+v", got.Input[0]["content"])
	}
	if got.Input[1]["type"] != "reasoning" || got.Input[2]["type"] != "function_call" || got.Input[3]["type"] != "function_call_output" {
		t.Fatalf("preserved response history: %+v", got.Input)
	}
	if len(got.Tools) != 1 || got.Tools[0]["type"] != "function" || got.Tools[0]["name"] != "read" || got.Tools[0]["function"] != nil {
		t.Fatalf("Responses tools must be flattened: %+v", got.Tools)
	}
}

func TestOpenAIResponsesStreamAssemblesTextThinkingAndUsage(t *testing.T) {
	client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer responses-test-key" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponsesEvent(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`)
		writeResponsesEvent(w, `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"think"}`)
		writeResponsesEvent(w, `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"ing"}`)
		writeResponsesEvent(w, `{"type":"response.reasoning_summary_text.done","item_id":"rs_1","text":"thinking"}`)
		writeResponsesEvent(w, `{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`)
		writeResponsesEvent(w, `{"type":"response.output_text.delta","item_id":"msg_1","delta":"ans"}`)
		writeResponsesEvent(w, `{"type":"response.output_text.delta","item_id":"msg_1","delta":"wer"}`)
		writeResponsesEvent(w, `{"type":"response.output_text.done","item_id":"msg_1","text":"answer"}`)
		writeResponsesEvent(w, `{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer"}]}}`)
		writeResponsesEvent(w, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[]},{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":10,"output_tokens":7,"input_tokens_details":{"cached_tokens":4}}}}`)
	}))
	defer srv.Close()

	var textOut, thinkOut strings.Builder
	msg, usage, err := client.stream(context.Background(), Request{
		Model: "grok-test", Messages: []Message{{Role: "user", Content: "question"}},
	}, EventSink{
		OnText:  func(delta string) { textOut.WriteString(delta) },
		OnThink: func(delta string) { thinkOut.WriteString(delta) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if textOut.String() != "answer" || thinkOut.String() != "thinking" || msg.Content != "answer" {
		t.Fatalf("assembled response text=%q thinking=%q msg=%q", textOut.String(), thinkOut.String(), msg.Content)
	}
	if msg.StopReason != "completed" || usage.PromptTokens != 10 || usage.CompletionTokens != 7 || usage.Cached() != 4 {
		t.Fatalf("stop/usage: stop=%q usage=%+v", msg.StopReason, usage)
	}
	if len(msg.ProviderBlocks) != 2 || !strings.Contains(string(msg.ProviderBlocks[0]), `"type":"reasoning"`) {
		t.Fatalf("preserved response items: %s", msg.ProviderBlocks)
	}
}

func TestOpenAIResponsesStreamAssemblesFunctionCall(t *testing.T) {
	client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponsesEvent(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item","type":"function_call","call_id":"call-1","name":"read","arguments":""}}`)
		writeResponsesEvent(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_item","call_id":"call-1","name":"read","delta":"{\"path\":"}`)
		writeResponsesEvent(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_item","delta":"\"README.md\"}"}`)
		writeResponsesEvent(w, `{"type":"response.function_call_arguments.done","item_id":"fc_item","arguments":"{\"path\":\"README.md\"}"}`)
		writeResponsesEvent(w, `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_item","type":"function_call","call_id":"call-1","name":"read","arguments":"{\"path\":\"README.md\"}","status":"completed"}}`)
		writeResponsesEvent(w, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"fc_item","type":"function_call","call_id":"call-1","name":"read","arguments":"{\"path\":\"README.md\"}","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":5}}}`)
	}))
	defer srv.Close()

	msg, usage, err := client.stream(context.Background(), Request{Model: "grok-test", Messages: []Message{{Role: "user", Content: "read"}}}, EventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls: %+v", msg.ToolCalls)
	}
	call := msg.ToolCalls[0]
	if call.ID != "call-1" || call.Function.Name != "read" || call.Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("function call: %+v", call)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 5 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestOpenAIResponsesComplete(t *testing.T) {
	client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request: %v", err)
		}
		if _, exists := body["stream"]; exists {
			t.Errorf("non-streaming request should omit stream: %v", body)
		}
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}],"usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer srv.Close()

	msg, usage, err := client.complete(context.Background(), Request{Model: "grok-test", Messages: []Message{{Role: "user", Content: "summarize"}}, MaxTokens: 12}, EventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != "assistant" || msg.TextContent() != "summary" || usage.PromptTokens != 4 || usage.CompletionTokens != 2 {
		t.Fatalf("completion = %+v usage=%+v", msg, usage)
	}
}

func TestOpenAIResponsesProbeAndModels(t *testing.T) {
	client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer responses-test-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"grok-test","context_length":128000}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/responses":
			var body struct {
				Model           string            `json:"model"`
				MaxOutputTokens int               `json:"max_output_tokens"`
				Input           []json.RawMessage `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("probe body: %v", err)
			}
			if body.Model != "real-model" || body.MaxOutputTokens != 1 || len(body.Input) != 1 {
				t.Errorf("probe body fields: %+v", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"ModelError","message":"invalid model"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	models, err := client.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "grok-test" {
		t.Fatalf("models = %+v, err=%v", models, err)
	}
	if err := client.Probe(context.Background(), "real-model"); err != nil {
		t.Fatalf("model error should not reject authentication: %v", err)
	}
}

func TestOpenAIresponsesCodexSubscriptionModels(t *testing.T) {
	client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{
			"models": [
				{
					"slug": "gpt-5.3-codex",
					"context_window": 272000,
					"supported_in_api": true,
					"visibility": "list",
					"supported_reasoning_levels": [
						{"effort": "low"},
						{"effort": "high"},
						{"effort": "max"}
					]
				},
				{
					"slug": "gpt-hidden",
					"context_window": 128000,
					"supported_in_api": true,
					"visibility": "hidden"
				},
				{
					"slug": "gpt-unsupported",
					"context_window": 128000,
					"supported_in_api": false,
					"visibility": "list"
				}
			]
		}`)
	}))
	defer srv.Close()

	client.flavor = responsesCodexSubscription
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d: %+v", len(models), models)
	}
	m := models[0]
	if m.ID != "gpt-5.3-codex" {
		t.Errorf("ID = %q, want gpt-5.3-codex", m.ID)
	}
	if m.ContextLength != 272000 {
		t.Errorf("ContextLength = %d, want 272000", m.ContextLength)
	}
	if len(m.ReasoningEfforts) != 3 || m.ReasoningEfforts[0] != "low" || m.ReasoningEfforts[2] != "max" {
		t.Errorf("ReasoningEfforts = %+v", m.ReasoningEfforts)
	}
}

func TestOpenAIResponsesModelsRejectsUnknownShape(t *testing.T) {
	t.Run("CodexSubscriptionMissingModels", func(t *testing.T) {
		client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"unrelated": true}`)
		}))
		defer srv.Close()

		client.flavor = responsesCodexSubscription
		_, err := client.Models(context.Background())
		if err == nil {
			t.Fatal("expected error on missing models array, got nil")
		}
		if !strings.Contains(err.Error(), "missing \"models\" array") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("StandardResponsesMissingData", func(t *testing.T) {
		client, srv := responsesClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"unrelated": true}`)
		}))
		defer srv.Close()

		client.flavor = responsesPublicAPI
		_, err := client.Models(context.Background())
		if err == nil {
			t.Fatal("expected error on missing data array, got nil")
		}
		if !strings.Contains(err.Error(), "missing \"data\" array") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}
