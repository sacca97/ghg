package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func anthropicClientForTest(t *testing.T, handler http.Handler) (*AnthropicClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	client := NewAnthropic(srv.URL, "anthropic-test-key")
	client.AuthKind = "header"
	client.AuthHeader = "x-api-key"
	client.Headers = map[string]string{"anthropic-version": "2023-06-01"}
	client.MaxRetries = 1
	return client, srv
}

func writeAnthropicEvent(w http.ResponseWriter, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}

func writeAnthropicFragmentedEvent(w http.ResponseWriter, payload string) {
	flusher, _ := w.(http.Flusher)
	_, _ = io.WriteString(w, "data: ")
	for start := 0; start < len(payload); {
		end := start + 5
		if end > len(payload) {
			end = len(payload)
		}
		_, _ = io.WriteString(w, payload[start:end])
		if flusher != nil {
			flusher.Flush()
		}
		start = end
	}
	_, _ = io.WriteString(w, "\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func TestAnthropicRequestTranslation(t *testing.T) {
	tool := NewTool("read", "Read a file", `{"type":"object","properties":{"path":{"type":"string"}}}`)
	call := ToolCall{ID: "call-1", Type: "function"}
	call.Function.Name = "read"
	call.Function.Arguments = `{"path":"README.md"}`
	providerBlocks := []json.RawMessage{
		json.RawMessage(`{"type":"thinking","thinking":"decide","signature":"sig-1"}`),
		json.RawMessage(`{"type":"tool_use","id":"call-1","name":"read","input":{"path":"README.md"}}`),
	}
	request := Request{
		Model: "claude-test",
		Messages: []Message{
			{Role: "system", Content: "You are concise."},
			{Role: "user", Content: "Look at this", Parts: []ContentPart{ImagePart("png", []byte{1, 2})}},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{call}, ProviderBlocks: providerBlocks},
			{Role: "tool", Content: "file contents", ToolCallID: "call-1", ExitCode: 1},
		},
		Tools:           []Tool{tool},
		MaxTokens:       123,
		ReasoningEffort: "medium",
	}
	wire, err := newAnthropicRequest(request, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model     string           `json:"model"`
		MaxTokens int              `json:"max_tokens"`
		Stream    bool             `json:"stream"`
		System    []map[string]any `json:"system"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools    []map[string]any `json:"tools"`
		Thinking map[string]any   `json:"thinking"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-test" || got.MaxTokens != 123 || !got.Stream {
		t.Fatalf("request header fields: %+v", got)
	}
	if len(got.System) != 1 || got.System[0]["cache_control"] == nil {
		t.Fatalf("system cache breakpoint missing: %+v", got.System)
	}
	if got.Thinking["type"] != "adaptive" || got.Thinking["display"] != "summarized" {
		t.Fatalf("thinking config: %+v", got.Thinking)
	}
	if len(got.Tools) != 1 || got.Tools[0]["name"] != "read" || got.Tools[0]["cache_control"] == nil {
		t.Fatalf("tools: %+v", got.Tools)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" || got.Messages[2].Role != "user" {
		t.Fatalf("messages: %+v", got.Messages)
	}
	if got.Messages[0].Content[0]["type"] != "text" || got.Messages[0].Content[1]["type"] != "image" {
		t.Fatalf("multimodal user content: %+v", got.Messages[0].Content)
	}
	imageSource, ok := got.Messages[0].Content[1]["source"].(map[string]any)
	if !ok || imageSource["type"] != "base64" || imageSource["data"] != "AQI=" {
		t.Fatalf("image source: %+v", got.Messages[0].Content[1])
	}
	if got.Messages[1].Content[0]["type"] != "thinking" || got.Messages[1].Content[1]["type"] != "tool_use" {
		t.Fatalf("assistant blocks: %+v", got.Messages[1].Content)
	}
	toolResult := got.Messages[2].Content[0]
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call-1" || toolResult["is_error"] != true {
		t.Fatalf("tool result: %+v", toolResult)
	}
	cache, ok := toolResult["cache_control"].(map[string]any)
	if !ok || cache["type"] != "ephemeral" {
		t.Fatalf("rolling cache breakpoint missing: %+v", toolResult)
	}

	request.Messages = append(request.Messages, Message{Role: "user", Content: "Continue."})
	next, err := newAnthropicRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Messages) != 3 || len(next.Messages[2].Content) != 2 {
		t.Fatalf("next request messages: %+v", next.Messages)
	}
	var oldToolResult, nextUser map[string]any
	if err := json.Unmarshal(next.Messages[2].Content[0], &oldToolResult); err != nil {
		t.Fatal(err)
	}
	if _, ok := oldToolResult["cache_control"]; ok {
		t.Fatalf("old rolling breakpoint remained: %s", next.Messages[2].Content[0])
	}
	if err := json.Unmarshal(next.Messages[2].Content[1], &nextUser); err != nil {
		t.Fatal(err)
	}
	lastCache, ok := nextUser["cache_control"].(map[string]any)
	if !ok || lastCache["type"] != "ephemeral" {
		t.Fatalf("new rolling cache breakpoint missing: %s", next.Messages[2].Content[1])
	}
	for i := range providerBlocks {
		if string(next.Messages[1].Content[i]) != string(providerBlocks[i]) {
			t.Fatalf("preserved provider block %d changed: %s", i, next.Messages[1].Content[i])
		}
	}
}

func TestAnthropicReasoningToggle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{name: "enabled", enabled: true, want: "enabled"},
		{name: "disabled", enabled: false, want: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled := tc.enabled
			thinking, output := anthropicReasoningRequest(Request{ReasoningEnabled: &enabled})
			if thinking == nil || thinking.Type != tc.want || output != nil {
				t.Fatalf("thinking = %+v, output = %+v", thinking, output)
			}
			if tc.enabled && thinking.BudgetTokens <= 0 {
				t.Fatalf("enabled thinking needs a budget: %+v", thinking)
			}
		})
	}
}

func TestAnthropicStreamAssemblesFragmentedThinkingAndUsage(t *testing.T) {
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" || r.Header.Get("x-api-key") != "anthropic-test-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeAnthropicFragmentedEvent(w, `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":4}}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ing"}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_stop","index":0}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ans"}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"wer"}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"content_block_stop","index":1}`)
		writeAnthropicFragmentedEvent(w, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
		writeAnthropicFragmentedEvent(w, `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	var textOut, thinkOut strings.Builder
	msg, usage, err := client.stream(context.Background(), Request{Model: "claude-test", Messages: []Message{{Role: "user", Content: "question"}}}, EventSink{
		OnText:  func(delta string) { textOut.WriteString(delta) },
		OnThink: func(delta string) { thinkOut.WriteString(delta) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if textOut.String() != "answer" || thinkOut.String() != "thinking" || msg.Content != "answer" {
		t.Fatalf("assembled response text=%q thinking=%q msg=%q", textOut.String(), thinkOut.String(), msg.Content)
	}
	if msg.StopReason != "end_turn" || usage.PromptTokens != 16 || usage.CompletionTokens != 7 || usage.Cached() != 4 || usage.CacheCreationTokens != 2 {
		t.Fatalf("stop/usage: stop=%q usage=%+v", msg.StopReason, usage)
	}
	if len(msg.ProviderBlocks) != 2 || !strings.Contains(string(msg.ProviderBlocks[0]), `"signature":"sig"`) {
		t.Fatalf("preserved blocks: %s", msg.ProviderBlocks)
	}
}

func TestAnthropicStreamParallelToolUses(t *testing.T) {
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeAnthropicEvent(w, `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`)
		writeAnthropicEvent(w, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-a","name":"read","input":{}}}`)
		writeAnthropicEvent(w, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-b","name":"bash","input":{}}}`)
		writeAnthropicEvent(w, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`)
		writeAnthropicEvent(w, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`)
		writeAnthropicEvent(w, `{"type":"content_block_stop","index":0}`)
		writeAnthropicEvent(w, `{"type":"content_block_stop","index":1}`)
		writeAnthropicEvent(w, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
		writeAnthropicEvent(w, `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	msg, _, err := client.stream(context.Background(), Request{Model: "claude-test", Messages: []Message{{Role: "user", Content: "inspect"}}}, EventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 2 || msg.StopReason != "tool_use" {
		t.Fatalf("tool response: %+v", msg)
	}
	if msg.ToolCalls[0].ID != "call-a" || msg.ToolCalls[0].Function.Name != "read" || msg.ToolCalls[0].Function.Arguments != `{"path":"a"}` {
		t.Fatalf("first tool call: %+v", msg.ToolCalls[0])
	}
	if msg.ToolCalls[1].ID != "call-b" || msg.ToolCalls[1].Function.Name != "bash" || msg.ToolCalls[1].Function.Arguments != `{"command":"pwd"}` {
		t.Fatalf("second tool call: %+v", msg.ToolCalls[1])
	}
}

func TestAnthropicStreamTerminalUsageMerged(t *testing.T) {
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Compatibility proxies like OpenCode send empty message_start usage, then report final tokens in message_delta
		writeAnthropicEvent(w, `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`)
		writeAnthropicEvent(w, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`)
		writeAnthropicEvent(w, `{"type":"content_block_stop","index":0}`)
		writeAnthropicEvent(w, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":33159,"output_tokens":1194}}`)
		writeAnthropicEvent(w, `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	_, usage, err := client.stream(context.Background(), Request{Model: "qwen3.8-max", Messages: []Message{{Role: "user", Content: "hi"}}}, EventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokens != 33159 || usage.CompletionTokens != 1194 {
		t.Fatalf("expected 33159 prompt tokens and 1194 completion tokens, got %+v", usage)
	}
}

func TestAnthropicCompleteAndMaxTokenToolDiscard(t *testing.T) {
	var streamField any
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		streamField = body["stream"]
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","id":"call-1","name":"read","input":{"path":"x"}}],"stop_reason":"max_tokens","usage":{"input_tokens":4,"output_tokens":8}}`)
	}))
	defer srv.Close()

	msg, usage, err := client.complete(context.Background(), Request{Model: "claude-test", Messages: []Message{{Role: "user", Content: "go"}}, MaxTokens: 9}, EventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if streamField != nil || len(msg.ToolCalls) != 0 || !strings.Contains(msg.Content, "truncated") {
		t.Fatalf("completion stream/tool handling: stream=%v msg=%+v", streamField, msg)
	}
	if usage.PromptTokens != 4 || usage.CompletionTokens != 8 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestAnthropicRetryBoundariesAndTypedErrors(t *testing.T) {
	noSleep(t)
	for _, tt := range []struct {
		name       string
		status     int
		body       string
		wantCalls  int32
		contextErr bool
	}{
		{name: "context limit", status: http.StatusBadRequest, body: `{"type":"error","error":{"message":"prompt is too long"}}`, wantCalls: 1, contextErr: true},
		{name: "rate limit", status: http.StatusTooManyRequests, body: "slow", wantCalls: 2},
		{name: "server", status: http.StatusBadGateway, body: "gateway", wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			client.MaxRetries = 2
			defer srv.Close()
			_, _, err := client.stream(context.Background(), Request{Model: "claude-test", Messages: []Message{{Role: "user", Content: "x"}}}, EventSink{})
			if err == nil {
				t.Fatal("expected error")
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || !strings.Contains(httpErr.Error(), fmt.Sprint(tt.status)) {
				t.Fatalf("typed error: %T %v", err, err)
			}
			if calls.Load() != tt.wantCalls {
				t.Fatalf("calls: %d, want %d", calls.Load(), tt.wantCalls)
			}
			if got := IsContextLimit(err); got != tt.contextErr {
				t.Fatalf("context limit: %v, want %v", got, tt.contextErr)
			}
		})
	}
}

func TestAnthropicMalformedEventDoesNotRetry(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeAnthropicEvent(w, "not-json")
	}))
	client.MaxRetries = 3
	defer srv.Close()

	_, _, err := client.stream(context.Background(), Request{Model: "claude-test", Messages: []Message{{Role: "user", Content: "x"}}}, EventSink{})
	if err == nil || !strings.Contains(err.Error(), "malformed anthropic SSE event") {
		t.Fatalf("error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("malformed event retried %d times", calls.Load())
	}
}

func TestAnthropicCancellationStopsRequest(t *testing.T) {
	started := make(chan struct{})
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := client.stream(ctx, Request{Model: "claude-test", Messages: []Message{{Role: "user", Content: "x"}}}, EventSink{})
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not stop after cancellation")
	}
}

func TestAnthropicMessageProviderBlocksRoundTrip(t *testing.T) {
	original := Message{
		Role:       "assistant",
		Content:    "answer",
		StopReason: "end_turn",
		ProviderBlocks: []json.RawMessage{
			json.RawMessage(`{"type":"redacted_thinking","data":"opaque"}`),
			json.RawMessage(`{"type":"text","text":"answer"}`),
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.StopReason != original.StopReason || len(got.ProviderBlocks) != 2 || string(got.ProviderBlocks[0]) != string(original.ProviderBlocks[0]) {
		t.Fatalf("round trip: %+v", got)
	}

	openAI := newOpenAIRequest(Request{Messages: []Message{original}}, true)
	openAIData, err := json.Marshal(openAI)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(openAIData), "provider_blocks") || strings.Contains(string(openAIData), "stop_reason") {
		t.Fatalf("opaque provider state leaked to OpenAI: %s", openAIData)
	}
}

func TestAnthropicModels(t *testing.T) {
	var page atomic.Int32
	client, srv := anthropicClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" || r.Header.Get("x-api-key") != "anthropic-test-key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if page.Add(1) == 1 {
			if r.URL.Query().Get("after_id") != "" || r.URL.Query().Get("limit") != "1000" {
				http.Error(w, "bad pagination", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-a","max_input_tokens":200000,"max_tokens":8192,"capabilities":{"image_input":{"supported":true},"thinking":{"supported":true}}}],"has_more":true,"last_id":"claude-a"}`)
			return
		}
		if r.URL.Query().Get("after_id") != "claude-a" {
			http.Error(w, "missing cursor", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-b","max_input_tokens":100000,"max_tokens":4096,"capabilities":{"effort":{"supported":true,"levels":["low","medium","high"]}}}],"has_more":false,"last_id":"claude-b"}`)
	}))
	defer srv.Close()

	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if page.Load() != 2 || len(models) != 2 {
		t.Fatalf("pages/models: %d %+v", page.Load(), models)
	}
	if models[0].ID != "claude-a" || models[0].ContextLength != 200000 || models[0].MaxCompletionTokens != 8192 || !models[0].SupportsVision() || len(models[0].ReasoningEfforts) != 3 {
		t.Fatalf("first model: %+v", models[0])
	}
	if models[1].ID != "claude-b" || strings.Join(models[1].ReasoningEfforts, ",") != "low,medium,high" {
		t.Fatalf("second model: %+v", models[1])
	}
}
