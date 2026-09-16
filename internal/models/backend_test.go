package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testChatClient(t *testing.T, baseURL, apiKey string) *Client {
	t.Helper()
	backend, err := NewBackend(Resolved{
		BaseURL:  baseURL,
		Protocol: ProtocolOpenAIChatCompletions,
		Auth:     Auth{Kind: AuthBearer, Header: "Authorization"},
	}, BackendOptions{APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	return backend.(*Client)
}

func testAnthropicClient(t *testing.T, baseURL, apiKey string) *AnthropicClient {
	t.Helper()
	backend, err := NewBackend(Resolved{
		BaseURL:        baseURL,
		Protocol:       ProtocolAnthropicMessages,
		Auth:           Auth{Kind: AuthHeader, Header: "x-api-key"},
		DefaultHeaders: map[string]string{"anthropic-version": "2023-06-01"},
	}, BackendOptions{APIKey: apiKey, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return backend.(*AnthropicClient)
}

func testResponsesClient(t *testing.T, baseURL, apiKey string) *OpenAIResponsesClient {
	t.Helper()
	backend, err := NewBackend(Resolved{
		BaseURL:  baseURL,
		Protocol: ProtocolOpenAIResponses,
		Auth:     Auth{Kind: AuthBearer, Header: "Authorization"},
	}, BackendOptions{APIKey: apiKey, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return backend.(*OpenAIResponsesClient)
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testHandlerTransport(handler http.Handler) http.RoundTripper {
	return testRoundTripper(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Result(), nil
	})
}

func testChatClientWithHandler(t *testing.T, apiKey string, handler http.Handler) *Client {
	t.Helper()
	client := testChatClient(t, "http://provider.test", apiKey)
	client.HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	return client
}

func testAnthropicClientWithHandler(t *testing.T, handler http.Handler) *AnthropicClient {
	t.Helper()
	client := testAnthropicClient(t, "http://provider.test", "anthropic-test-key")
	client.HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	return client
}

func testResponsesClientWithHandler(t *testing.T, handler http.Handler) *OpenAIResponsesClient {
	t.Helper()
	client := testResponsesClient(t, "http://provider.test", "responses-test-key")
	client.HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	return client
}

func runStream(backend Backend, ctx context.Context, req Request, onText, onThink func(string)) (Message, Usage, error) {
	return backend.Stream(ctx, req, EventSink{OnText: onText, OnThink: onThink})
}

func runStreamWithRetry(backend Backend, ctx context.Context, req Request, onRetry func(RetryEvent)) (Message, Usage, error) {
	return backend.Stream(ctx, req, EventSink{OnRetry: onRetry})
}

func TestNewTransportUsesBoundedDefaultTimeout(t *testing.T) {
	if got := newTransport("http://provider.test", "key").HTTP.Timeout; got != 3*time.Minute {
		t.Fatalf("default request timeout = %s, want 3m", got)
	}
}

func completeText(backend Backend, ctx context.Context, req Request) (string, Usage, error) {
	msg, usage, err := backend.Complete(ctx, req)
	return msg.TextContent(), usage, err
}

func TestNewBackend(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		baseURL  string
		wantErr  bool
	}{
		{name: "canonical openai protocol", protocol: ProtocolOpenAIChatCompletions, baseURL: "http://example.test"},
		{name: "legacy openai protocol", protocol: ProtocolOpenAICompletions, baseURL: "http://example.test"},
		{name: "empty protocol uses current adapter", baseURL: "http://example.test"},
		{name: "empty base url", protocol: ProtocolOpenAIChatCompletions, wantErr: true},
		{name: "unknown protocol", protocol: Protocol("made-up"), baseURL: "http://example.test", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, err := NewBackend(Resolved{
				Protocol: tt.protocol,
				BaseURL:  tt.baseURL,
			}, BackendOptions{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewBackend() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if _, ok := backend.(*Client); !ok {
				t.Fatalf("backend type = %T, want *Client", backend)
			}
			if _, ok := backend.(CatalogBackend); !ok {
				t.Fatalf("backend %T should expose optional catalog capability", backend)
			}
		})
	}
}

func TestNewBackendAnthropicMessages(t *testing.T) {
	backend, err := NewBackend(Resolved{
		Protocol:       ProtocolAnthropicMessages,
		BaseURL:        "https://api.anthropic.com/v1",
		DefaultHeaders: map[string]string{"anthropic-version": "2023-06-01"},
		Auth:           Auth{Kind: "header", Header: "x-api-key"},
	}, BackendOptions{APIKey: "key", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	anthropic, ok := backend.(*AnthropicClient)
	if !ok {
		t.Fatalf("backend type = %T, want *AnthropicClient", backend)
	}
	if _, ok := backend.(CatalogBackend); !ok {
		t.Fatalf("backend %T should expose optional catalog capability", backend)
	}
	if anthropic.AuthKind != "header" || anthropic.AuthHeader != "x-api-key" || anthropic.MaxRetries != 1 || anthropic.Headers["anthropic-version"] != "2023-06-01" {
		t.Fatalf("factory config not applied: %+v", anthropic)
	}
}

func TestNewBackendOpenAIResponses(t *testing.T) {
	backend, err := NewBackend(Resolved{
		Protocol:       ProtocolOpenAIResponses,
		BaseURL:        "https://api.example/v1",
		DefaultHeaders: map[string]string{"x-provider": "test"},
		Auth:           Auth{Kind: "bearer", Header: "Authorization"},
	}, BackendOptions{APIKey: "key", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	responses, ok := backend.(*OpenAIResponsesClient)
	if !ok {
		t.Fatalf("backend type = %T, want *OpenAIResponsesClient", backend)
	}
	if _, ok := backend.(CatalogBackend); !ok {
		t.Fatal("Responses backend should expose optional catalog capability")
	}
	if responses.AuthKind != "bearer" || responses.AuthHeader != "Authorization" || responses.MaxRetries != 1 || responses.Headers["x-provider"] != "test" || responses.flavor != responsesPublicAPI {
		t.Fatalf("factory config not applied: %+v", responses)
	}
}

func TestBackendSendsConfiguredSessionHeader(t *testing.T) {
	var got string
	backend, err := NewBackend(Resolved{
		Profile:  Profile{SessionHeader: "x-opencode-session"},
		Protocol: ProtocolOpenAIChatCompletions,
		BaseURL:  "http://provider.test",
		Auth:     Auth{Kind: AuthBearer, Header: "Authorization"},
	}, BackendOptions{APIKey: "key", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	client := backend.(*Client)
	client.HTTP = &http.Client{Transport: testHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-opencode-session")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))}
	if _, _, err := client.Complete(context.Background(), Request{
		Model:     "model",
		Messages:  []Message{{Role: "user", Content: "hello"}},
		SessionID: "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	if got != "session-1" {
		t.Fatalf("x-opencode-session = %q, want session-1", got)
	}
}

func TestOpenAIBackendStreamUsesRequestLocalEvents(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	client := testChatClientWithHandler(t, "k", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "temporary gateway failure")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	client.MaxRetries = 2
	var legacyRetries atomic.Int32
	client.OnRetry = func(RetryEvent) { legacyRetries.Add(1) }
	backend := client
	var textOut, thinkOut strings.Builder
	var requestRetries atomic.Int32
	msg, _, err := backend.Stream(context.Background(), Request{Model: "m"}, EventSink{
		OnText:  func(delta string) { _, _ = textOut.WriteString(delta) },
		OnThink: func(delta string) { _, _ = thinkOut.WriteString(delta) },
		OnRetry: func(RetryEvent) { requestRetries.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.TextContent() != "answer" || textOut.String() != "answer" {
		t.Fatalf("message/text = %q/%q, want answer", msg.TextContent(), textOut.String())
	}
	if thinkOut.String() != "think" {
		t.Fatalf("thinking = %q, want think", thinkOut.String())
	}
	if requestRetries.Load() != 1 {
		t.Fatalf("request-local retries = %d, want 1", requestRetries.Load())
	}
	if legacyRetries.Load() != 0 {
		t.Fatalf("adapter used the shared legacy retry hook %d times", legacyRetries.Load())
	}
}

func TestOpenAIBackendCompleteReturnsMessage(t *testing.T) {
	backend := testChatClientWithHandler(t, "k", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"summary"}}]}`))
	}))

	msg, _, err := backend.Complete(context.Background(), Request{Model: "summary-model"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != "assistant" || msg.TextContent() != "summary" {
		t.Fatalf("completion message = %+v", msg)
	}
}

func TestOpenAIBackendCompleteReturnsToolCalls(t *testing.T) {
	args := `{"goal":"ship it"}`
	backend := testChatClientWithHandler(t, "k", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_plan","arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`, args)
	}))

	msg, usage, err := backend.Complete(context.Background(), Request{Model: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != "tool_calls" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "submit_plan" {
		t.Fatalf("completion tool call = %+v", msg)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Fatalf("completion usage = %+v", usage)
	}
}

func TestOpenAIBackendAppliesProfileAuthAndHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "secret" {
			t.Errorf("profile auth header = %q, want secret", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("profile default header = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unexpected bearer header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model"}]}`))
	})

	backend, err := NewBackend(Resolved{
		Protocol:       ProtocolOpenAIChatCompletions,
		BaseURL:        "http://provider.test",
		Auth:           Auth{Kind: "header", Header: "x-api-key"},
		DefaultHeaders: map[string]string{"anthropic-version": "2023-06-01"},
	}, BackendOptions{APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	backend.(*Client).HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	catalog, ok := backend.(CatalogBackend)
	if !ok {
		t.Fatal("backend should expose catalog capability")
	}
	if _, err := catalog.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIBackendProbeUsesRealModelAndRejectsAuthError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("probe request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("probe body: %v", err)
		}
		if body.Model != "real-model" || body.MaxTokens != 1 || len(body.Messages) != 1 {
			t.Errorf("probe should use the requested real model with a one-token bound: %+v", body)
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":{"type":"AuthError","message":"invalid key"}}`)
			return
		}
		// Some providers use 401 for an unknown model too. The error type,
		// not the status alone, must decide whether the key is rejected.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"type":"ModelError","message":"invalid model"}}`)
	})

	good, err := NewBackend(Resolved{
		Protocol: ProtocolOpenAIChatCompletions, BaseURL: "http://provider.test",
		Auth: Auth{Kind: "bearer", Header: "Authorization"},
	}, BackendOptions{APIKey: "good"})
	if err != nil {
		t.Fatal(err)
	}
	good.(*Client).HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	if err := good.(ProbeBackend).Probe(context.Background(), "real-model"); err != nil {
		t.Fatalf("non-auth probe response should be accepted: %v", err)
	}

	bad, err := NewBackend(Resolved{
		Protocol: ProtocolOpenAIChatCompletions, BaseURL: "http://provider.test",
		Auth: Auth{Kind: "bearer", Header: "Authorization"},
	}, BackendOptions{APIKey: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	bad.(*Client).HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	if err := bad.(ProbeBackend).Probe(context.Background(), "real-model"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("AuthError probe response should reject the key: %v", err)
	}
}

func TestAnthropicBackendProbeUsesNativeMessagesEndpoint(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Errorf("anthropic probe request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic probe headers = x-api-key %q version %q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"ModelError","message":"invalid model"}}`)
	})

	backend, err := NewBackend(Resolved{
		Protocol: ProtocolAnthropicMessages, BaseURL: "http://provider.test",
		Auth:           Auth{Kind: "header", Header: "x-api-key"},
		DefaultHeaders: map[string]string{"anthropic-version": "2023-06-01"},
	}, BackendOptions{APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	backend.(*AnthropicClient).HTTP = &http.Client{Transport: testHandlerTransport(handler)}
	if err := backend.(ProbeBackend).Probe(context.Background(), "claude-real"); err != nil {
		t.Fatalf("non-401 anthropic probe response should be accepted: %v", err)
	}
}

func TestAuthenticatedProbeRejectsTypedAuthErrorRegardlessOfStatus(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"type":"AuthError","message":"invalid key"}}`)
	})

	err := authenticatedProbe(context.Background(), &http.Client{Transport: testHandlerTransport(handler)}, "http://provider.test", []byte(`{}`), func(*http.Request) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("typed AuthError should reject the credential even on 400: %v", err)
	}
}
