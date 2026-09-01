package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

const maxOpenAIResponsesSSELine = 10 * 1024 * 1024

// OpenAIResponsesClient talks to the OpenAI Responses API. It deliberately
// has its own client instead of reusing Client: the two APIs have different
// request history, tool, response, and streaming event shapes.
type OpenAIResponsesClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Headers map[string]string

	AuthKind   string
	AuthHeader string
	MaxRetries int
	OnRetry    func(RetryEvent)
}

// NewOpenAIResponses creates an OpenAI Responses client.
func NewOpenAIResponses(baseURL, apiKey string) *OpenAIResponsesClient {
	return &OpenAIResponsesClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *OpenAIResponsesClient) attempts() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return DefaultMaxAttempts
}

func (c *OpenAIResponsesClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *OpenAIResponsesClient) setRequestHeaders(req *http.Request) error {
	return applyRequestHeaders(req, c.Headers, c.APIKey, c.AuthKind, c.AuthHeader)
}

func (c *OpenAIResponsesClient) endpoint(suffix string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", errors.New("llm: openai responses base url is required")
	}
	return base + suffix, nil
}

// Note on continuation (previous_response_id):
// previous_response_id is intentionally deferred:
// - It requires provider-documented support (OpenCode and CommandCode currently do not document or use it).
// - It requires preserving response IDs and guaranteeing every subsequent request is a strict append-only continuation.
// - Compaction, rewind, model/provider switching, system-prompt changes, retries, and resume must invalidate the chain.
// Stateless full-history replay with a stable prompt prefix and session-scoped prompt_cache_key is the proven production approach.
type openAIResponsesRequest struct {
	Model           string                    `json:"model"`
	Instructions    string                    `json:"instructions,omitempty"`
	Input           []json.RawMessage         `json:"input,omitempty"`
	Tools           []openAIResponsesTool     `json:"tools,omitempty"`
	MaxOutputTokens int                       `json:"max_output_tokens,omitempty"`
	Reasoning       *openAIResponsesReasoning `json:"reasoning,omitempty"`
	Stream          bool                      `json:"stream,omitempty"`
	Store           *bool                     `json:"store,omitempty"`
	PromptCacheKey  string                    `json:"prompt_cache_key,omitempty"`
}

type openAIResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIResponsesReasoning struct {
	Effort string `json:"effort"`
}

type openAIResponsesInputMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content any    `json:"content"`
	Status  string `json:"status,omitempty"`
}

type openAIResponsesInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openAIResponsesInputImage struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail,omitempty"`
}

type openAIResponsesFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status,omitempty"`
}

type openAIResponsesFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// newOpenAIResponsesRequest translates the neutral request into Responses
// input items. Assistant output items are retained verbatim when available;
// this is required for reasoning models whose reasoning item is part of the
// next tool-call turn's context.
func newOpenAIResponsesRequest(req Request, stream bool) (openAIResponsesRequest, error) {
	msgs := repairToolHistory(stripAuthoredPreserveBlocks(req.Messages))
	storeFalse := false
	wire := openAIResponsesRequest{
		Model:           req.Model,
		MaxOutputTokens: req.MaxTokens,
		Stream:          stream,
		Store:           &storeFalse,
		PromptCacheKey:  strings.TrimSpace(req.SessionID),
	}

	var instructions []string
	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			text, err := responsesSystemText(msg)
			if err != nil {
				return openAIResponsesRequest{}, err
			}
			if text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			raw, err := responsesUserMessage(msg)
			if err != nil {
				return openAIResponsesRequest{}, err
			}
			wire.Input = append(wire.Input, raw)
		case "assistant":
			items, err := responsesAssistantItems(msg)
			if err != nil {
				return openAIResponsesRequest{}, err
			}
			wire.Input = append(wire.Input, items...)
		case "tool":
			raw, err := responsesToolResult(msg)
			if err != nil {
				return openAIResponsesRequest{}, err
			}
			wire.Input = append(wire.Input, raw)
		default:
			return openAIResponsesRequest{}, fmt.Errorf("llm: openai responses cannot translate message role %q", msg.Role)
		}
	}
	wire.Instructions = strings.Join(instructions, "\n\n")

	var err error
	wire.Tools, err = openAIResponsesTools(req.Tools)
	if err != nil {
		return openAIResponsesRequest{}, err
	}
	wire.Reasoning = openAIResponsesReasoningFor(req.ReasoningEffort)
	if len(wire.Input) == 0 && wire.Instructions == "" {
		return openAIResponsesRequest{}, errors.New("llm: openai responses request needs input or instructions")
	}
	return wire, nil
}

func responsesSystemText(msg Message) (string, error) {
	parts := make([]string, 0, 1+len(msg.Parts))
	if msg.Content != "" || len(msg.Parts) == 0 {
		parts = append(parts, msg.Content)
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			if len(parts) > 0 && part.Text == msg.Content {
				continue
			}
			parts = append(parts, part.Text)
		default:
			return "", fmt.Errorf("llm: openai responses system content does not support part %q", part.Type)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func responsesUserMessage(msg Message) (json.RawMessage, error) {
	content := make([]json.RawMessage, 0, 1+len(msg.Parts))
	textAdded := false
	appendText := func(text string) error {
		raw, err := json.Marshal(openAIResponsesInputText{Type: "input_text", Text: text})
		if err != nil {
			return fmt.Errorf("llm: marshal openai responses text: %w", err)
		}
		content = append(content, raw)
		textAdded = true
		return nil
	}
	if msg.Content != "" || len(msg.Parts) == 0 {
		if err := appendText(msg.Content); err != nil {
			return nil, err
		}
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			if textAdded && part.Text == msg.Content {
				continue
			}
			if err := appendText(part.Text); err != nil {
				return nil, err
			}
		case "image_url":
			if part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
				return nil, errors.New("llm: openai responses image part has no URL")
			}
			raw, err := json.Marshal(openAIResponsesInputImage{
				Type: "input_image", ImageURL: part.ImageURL.URL, Detail: "auto",
			})
			if err != nil {
				return nil, fmt.Errorf("llm: marshal openai responses image: %w", err)
			}
			content = append(content, raw)
		default:
			return nil, fmt.Errorf("llm: openai responses does not support content part %q", part.Type)
		}
	}
	return json.Marshal(openAIResponsesInputMessage{Type: "message", Role: "user", Content: content})
}

func responsesAssistantItems(msg Message) ([]json.RawMessage, error) {
	if len(msg.ProviderBlocks) > 0 {
		items := make([]json.RawMessage, 0, len(msg.ProviderBlocks))
		native := true
		for _, raw := range msg.ProviderBlocks {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &header); err != nil || header.Type == "" {
				if err == nil {
					err = errors.New("missing type")
				}
				return nil, fmt.Errorf("llm: invalid preserved openai responses output item: %w", err)
			}
			// ProviderBlocks can survive a model/provider switch. Anthropic
			// blocks have a different meaning and must fall through to the
			// neutral assistant/tool-call representation instead of being sent
			// as Responses output items.
			if header.Type == "text" || header.Type == "thinking" || header.Type == "redacted_thinking" || header.Type == "tool_use" || header.Type == "tool_result" {
				native = false
			}
			items = append(items, append(json.RawMessage(nil), raw...))
		}
		if native {
			return items, nil
		}
	}

	items := make([]json.RawMessage, 0, 1+len(msg.ToolCalls))
	if msg.Content != "" || len(msg.ToolCalls) == 0 {
		content, err := json.Marshal([]openAIResponsesInputText{{Type: "output_text", Text: msg.Content}})
		if err != nil {
			return nil, fmt.Errorf("llm: marshal openai responses assistant text: %w", err)
		}
		raw, err := json.Marshal(openAIResponsesInputMessage{
			Type: "message", Role: "assistant", Content: json.RawMessage(content), Status: "completed",
		})
		if err != nil {
			return nil, fmt.Errorf("llm: marshal openai responses assistant message: %w", err)
		}
		items = append(items, raw)
	}
	for _, call := range msg.ToolCalls {
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("llm: openai responses tool %q has invalid JSON arguments", call.Function.Name)
		}
		raw, err := json.Marshal(openAIResponsesFunctionCall{
			Type: "function_call", ID: call.ID, CallID: call.ID,
			Name: call.Function.Name, Arguments: args, Status: "completed",
		})
		if err != nil {
			return nil, fmt.Errorf("llm: marshal openai responses function call: %w", err)
		}
		items = append(items, raw)
	}
	return items, nil
}

func responsesToolResult(msg Message) (json.RawMessage, error) {
	if strings.TrimSpace(msg.ToolCallID) == "" {
		return nil, errors.New("llm: openai responses tool result has no call_id")
	}
	return json.Marshal(openAIResponsesFunctionCallOutput{
		Type: "function_call_output", CallID: msg.ToolCallID, Output: msg.Content,
	})
}

func openAIResponsesTools(tools []Tool) ([]openAIResponsesTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openAIResponsesTool, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			return nil, errors.New("llm: openai responses tool name is required")
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("llm: openai responses tool %q has invalid parameters", name)
		}
		out = append(out, openAIResponsesTool{
			Type: "function", Name: name, Description: tool.Function.Description,
			Parameters: append(json.RawMessage(nil), schema...),
		})
	}
	return out, nil
}

func openAIResponsesReasoningFor(effort string) *openAIResponsesReasoning {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return nil
	}
	if effort == "off" {
		effort = "none"
	}
	return &openAIResponsesReasoning{Effort: effort}
}

// Stream is the callback-compatible convenience form for direct Responses
// client users. Backend callers should use the request-local EventSink path.
func (c *OpenAIResponsesClient) Stream(ctx context.Context, req Request, onText, onThink func(string)) (Message, Usage, error) {
	return c.stream(ctx, req, EventSink{OnText: onText, OnThink: onThink, OnRetry: c.OnRetry})
}

// Complete is the direct-client convenience form matching Client.Complete.
func (c *OpenAIResponsesClient) Complete(ctx context.Context, req Request) (string, Usage, error) {
	msg, usage, err := c.complete(ctx, req, EventSink{OnRetry: c.OnRetry})
	return msg.TextContent(), usage, err
}

func (c *OpenAIResponsesClient) stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	wire, err := newOpenAIResponsesRequest(req, true)
	if err != nil {
		return Message{}, Usage{}, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return Message{}, Usage{}, err
	}

	var last error
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		emitted := false
		wrapText, wrapThink := sink.OnText, sink.OnThink
		if sink.OnText != nil {
			wrapText = func(delta string) {
				emitted = true
				sink.OnText(delta)
			}
		}
		if sink.OnThink != nil {
			wrapThink = func(delta string) {
				emitted = true
				sink.OnThink(delta)
			}
		}
		msg, usage, err := c.streamOnce(ctx, body, wrapText, wrapThink)
		if err == nil {
			return msg, usage, nil
		}
		last = err
		if emitted || !retryable(err) || attempt == c.attempts() {
			break
		}
		delay := backoff(attempt)
		if sink.OnRetry != nil {
			sink.OnRetry(RetryEvent{Attempt: attempt, Max: c.attempts(), Delay: delay, Err: err})
		}
		if err := sleep(ctx, delay); err != nil {
			return Message{}, Usage{}, err
		}
	}
	return Message{}, Usage{}, last
}

func (c *OpenAIResponsesClient) streamOnce(ctx context.Context, body []byte, onText, onThink func(string)) (Message, Usage, error) {
	endpoint, err := c.endpoint("/responses")
	if err != nil {
		return Message{}, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.setRequestHeaders(req); err != nil {
		return Message{}, Usage{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Message{}, Usage{}, openAIResponsesHTTPError(resp)
	}
	return consumeOpenAIResponsesSSE(resp.Body, onText, onThink)
}

func (c *OpenAIResponsesClient) complete(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	wire, err := newOpenAIResponsesRequest(req, false)
	if err != nil {
		return Message{}, Usage{}, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return Message{}, Usage{}, err
	}

	var last error
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		msg, usage, err := c.completeOnce(ctx, body)
		if err == nil {
			return msg, usage, nil
		}
		last = err
		if !retryable(err) || attempt == c.attempts() {
			break
		}
		delay := backoff(attempt)
		if sink.OnRetry != nil {
			sink.OnRetry(RetryEvent{Attempt: attempt, Max: c.attempts(), Delay: delay, Err: err})
		}
		if err := sleep(ctx, delay); err != nil {
			return Message{}, Usage{}, err
		}
	}
	return Message{}, Usage{}, last
}

func (c *OpenAIResponsesClient) completeOnce(ctx context.Context, body []byte) (Message, Usage, error) {
	endpoint, err := c.endpoint("/responses")
	if err != nil {
		return Message{}, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.setRequestHeaders(req); err != nil {
		return Message{}, Usage{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Message{}, Usage{}, openAIResponsesHTTPError(resp)
	}
	var response openAIResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Message{}, Usage{}, nonRetryable{fmt.Errorf("malformed openai responses response: %w", err)}
	}
	if err := openAIResponsesResponseError(response); err != nil {
		return Message{}, Usage{}, err
	}
	msg, err := messageFromOpenAIResponses(response)
	if err != nil {
		return Message{}, Usage{}, err
	}
	return msg, openAIResponsesUsageValue(response.Usage), nil
}

// Models fetches the OpenAI-compatible GET /models endpoint used by many
// Responses providers, including OpenCode Go.
func (c *OpenAIResponsesClient) Models(ctx context.Context) ([]ModelInfo, error) {
	endpoint, err := c.endpoint("/models")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if err := c.setRequestHeaders(req); err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, openAIResponsesHTTPError(resp)
	}
	var list struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, nonRetryable{fmt.Errorf("malformed openai responses models response: %w", err)}
	}
	return list.Data, nil
}

// Probe performs one authenticated Responses request with a real model ID
// and a one-token output bound.
func (c *OpenAIResponsesClient) Probe(ctx context.Context, modelID string) error {
	wire, err := newOpenAIResponsesRequest(Request{
		Model:     probeModel(modelID, authProbeModel),
		Messages:  []Message{{Role: "user", Content: "ghg authentication probe"}},
		MaxTokens: 1,
	}, false)
	if err != nil {
		return err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	endpoint, err := c.endpoint("/responses")
	if err != nil {
		return err
	}
	return authenticatedProbe(ctx, c.httpClient(), endpoint, body, c.setRequestHeaders)
}

type openAIResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

type openAIResponsesError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type openAIResponsesResponse struct {
	ID                string                `json:"id"`
	Status            string                `json:"status"`
	Output            []json.RawMessage     `json:"output"`
	Usage             *openAIResponsesUsage `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Error *openAIResponsesError `json:"error,omitempty"`
}

func openAIResponsesUsageValue(raw *openAIResponsesUsage) Usage {
	if raw == nil {
		return Usage{}
	}
	usage := Usage{PromptTokens: raw.InputTokens, CompletionTokens: raw.OutputTokens}
	if raw.InputTokensDetails != nil && raw.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: raw.InputTokensDetails.CachedTokens}
	}
	return usage
}

func openAIResponsesResponseError(response openAIResponsesResponse) error {
	if response.Error != nil {
		message := response.Error.Message
		if response.Error.Type != "" {
			message = response.Error.Type + ": " + message
		}
		if message == "" {
			message = "openai responses request failed"
		}
		return nonRetryable{errors.New(message)}
	}
	if response.Status == "failed" {
		return nonRetryable{errors.New("openai responses request failed")}
	}
	return nil
}

type openAIResponsesOutputItem struct {
	Type      string                      `json:"type"`
	ID        string                      `json:"id"`
	CallID    string                      `json:"call_id"`
	Role      string                      `json:"role"`
	Name      string                      `json:"name"`
	Arguments string                      `json:"arguments"`
	Status    string                      `json:"status"`
	Content   []openAIResponsesOutputPart `json:"content"`
}

type openAIResponsesOutputPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

func messageFromOpenAIResponses(response openAIResponsesResponse) (Message, error) {
	stopReason := response.Status
	incomplete := response.Status == "incomplete"
	if incomplete && response.IncompleteDetails != nil && response.IncompleteDetails.Reason != "" {
		stopReason = response.IncompleteDetails.Reason
	}
	return messageFromOpenAIResponsesOutput(response.Output, stopReason, incomplete)
}

func messageFromOpenAIResponsesOutput(output []json.RawMessage, stopReason string, discardTools bool) (Message, error) {
	msg := Message{Role: "assistant", StopReason: stopReason}
	msg.ProviderBlocks = make([]json.RawMessage, 0, len(output))
	discardedToolCalls := false
	for _, raw := range output {
		var item openAIResponsesOutputItem
		if err := json.Unmarshal(raw, &item); err != nil || item.Type == "" {
			if err == nil {
				err = errors.New("missing type")
			}
			return Message{}, nonRetryable{fmt.Errorf("malformed openai responses output item: %w", err)}
		}
		msg.ProviderBlocks = append(msg.ProviderBlocks, append(json.RawMessage(nil), raw...))
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text", "text":
					msg.Content += part.Text
				case "refusal":
					msg.Content += part.Refusal
				}
			}
		case "function_call":
			if discardTools {
				discardedToolCalls = true
				continue
			}
			id := strings.TrimSpace(item.CallID)
			if id == "" {
				id = strings.TrimSpace(item.ID)
			}
			if id == "" {
				return Message{}, nonRetryable{fmt.Errorf("malformed openai responses function call %q: missing call_id", item.Name)}
			}
			args := strings.TrimSpace(item.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return Message{}, nonRetryable{fmt.Errorf("malformed openai responses function arguments for %q", item.Name)}
			}
			call := ToolCall{ID: id, Type: "function"}
			call.Function.Name = item.Name
			call.Function.Arguments = args
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	if discardedToolCalls {
		msg.ToolCalls = nil
		msg.Content += "\n[response truncated; tool calls discarded]"
	}
	return msg, nil
}

type openAIResponsesStreamEvent struct {
	Type        string                `json:"type"`
	Response    json.RawMessage       `json:"response"`
	Item        json.RawMessage       `json:"item"`
	OutputIndex *int                  `json:"output_index"`
	ItemID      string                `json:"item_id"`
	Delta       string                `json:"delta"`
	Text        string                `json:"text"`
	CallID      string                `json:"call_id"`
	Name        string                `json:"name"`
	Arguments   string                `json:"arguments"`
	Error       *openAIResponsesError `json:"error"`
}

type openAIResponsesStreamCall struct {
	Index     int
	ItemID    string
	CallID    string
	Name      string
	Arguments string
}

func consumeOpenAIResponsesSSE(r io.Reader, onText, onThink func(string)) (Message, Usage, error) {
	items := make(map[int]json.RawMessage)
	calls := make(map[string]*openAIResponsesStreamCall)
	var fallbackText strings.Builder
	var fallbackThinking strings.Builder
	var usage openAIResponsesUsage
	var final *openAIResponsesResponse
	sawTerminal := false
	itemIndices := make(map[string]int)

	ensureCall := func(key string, index int) *openAIResponsesStreamCall {
		call := calls[key]
		if call == nil {
			call = &openAIResponsesStreamCall{Index: index, ItemID: key}
			calls[key] = call
		}
		if index >= 0 {
			call.Index = index
		}
		return call
	}

	handle := func(eventName string, data []byte) error {
		if string(data) == "[DONE]" {
			sawTerminal = true
			return nil
		}
		var event openAIResponsesStreamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nonRetryable{fmt.Errorf("malformed openai responses SSE event: %w", err)}
		}
		if event.Type == "" {
			event.Type = eventName
		}
		if event.Type == "" {
			return nonRetryable{errors.New("malformed openai responses SSE event: missing type")}
		}

		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				fallbackText.WriteString(event.Delta)
				if onText != nil {
					onText(event.Delta)
				}
			}
		case "response.output_text.done":
			if fallbackText.Len() == 0 && event.Text != "" {
				fallbackText.WriteString(event.Text)
				if onText != nil {
					onText(event.Text)
				}
			}
		case "response.refusal.delta":
			if event.Delta != "" {
				fallbackText.WriteString(event.Delta)
				if onText != nil {
					onText(event.Delta)
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if event.Delta != "" {
				fallbackThinking.WriteString(event.Delta)
				if onThink != nil {
					onThink(event.Delta)
				}
			}
		case "response.reasoning_summary_text.done", "response.reasoning_text.done":
			if fallbackThinking.Len() == 0 && event.Text != "" {
				fallbackThinking.WriteString(event.Text)
				if onThink != nil {
					onThink(event.Text)
				}
			}
		case "response.output_item.added", "response.output_item.done":
			if len(event.Item) == 0 {
				return nonRetryable{fmt.Errorf("malformed openai responses %s event: missing item", event.Type)}
			}
			var item openAIResponsesOutputItem
			if err := json.Unmarshal(event.Item, &item); err != nil || item.Type == "" {
				if err == nil {
					err = errors.New("missing type")
				}
				return nonRetryable{fmt.Errorf("malformed openai responses output item: %w", err)}
			}
			index := len(items)
			if event.OutputIndex != nil {
				index = *event.OutputIndex
			} else if item.ID != "" {
				if existing, ok := itemIndices[item.ID]; ok {
					index = existing
				}
			}
			items[index] = append(json.RawMessage(nil), event.Item...)
			if item.ID != "" {
				itemIndices[item.ID] = index
			}
			if item.Type == "function_call" {
				key := strings.TrimSpace(item.ID)
				if key == "" {
					key = strings.TrimSpace(event.ItemID)
				}
				if key == "" {
					key = fmt.Sprintf("output-%d", index)
				}
				call := ensureCall(key, index)
				call.ItemID = firstNonEmpty(item.ID, event.ItemID, call.ItemID)
				call.CallID = item.CallID
				call.Name = item.Name
				if item.Arguments != "" {
					call.Arguments = item.Arguments
				}
			}
		case "response.function_call_arguments.delta":
			key := strings.TrimSpace(event.ItemID)
			if key == "" {
				return nonRetryable{errors.New("malformed openai responses function arguments event: missing item_id")}
			}
			call := ensureCall(key, -1)
			call.CallID = firstNonEmpty(event.CallID, call.CallID)
			call.Name = firstNonEmpty(event.Name, call.Name)
			call.Arguments += event.Delta
		case "response.function_call_arguments.done":
			key := strings.TrimSpace(event.ItemID)
			if key == "" {
				return nonRetryable{errors.New("malformed openai responses function arguments event: missing item_id")}
			}
			call := ensureCall(key, -1)
			call.CallID = firstNonEmpty(event.CallID, call.CallID)
			call.Name = firstNonEmpty(event.Name, call.Name)
			if event.Arguments != "" {
				call.Arguments = event.Arguments
			}
		case "response.completed", "response.incomplete", "response.done":
			if len(event.Response) > 0 {
				var response openAIResponsesResponse
				if err := json.Unmarshal(event.Response, &response); err != nil {
					return nonRetryable{fmt.Errorf("malformed openai responses completion: %w", err)}
				}
				if err := openAIResponsesResponseError(response); err != nil {
					return err
				}
				final = &response
				if response.Usage != nil {
					usage = *response.Usage
				}
			}
			sawTerminal = true
		case "response.failed", "response.error", "error":
			message := "openai responses stream failed"
			if event.Error != nil {
				message = event.Error.Message
				if event.Error.Type != "" {
					message = event.Error.Type + ": " + message
				}
			}
			if message == "" {
				message = "openai responses stream failed"
			}
			return nonRetryable{errors.New(message)}
		case "response.created", "response.in_progress", "response.content_part.added", "response.content_part.done":
			// Lifecycle events carry no text/tool state needed by the neutral
			// message.
		}
		return nil
	}

	if err := scanSSE(r, maxOpenAIResponsesSSELine, handle, func(line string) error {
		return nonRetryable{fmt.Errorf("malformed openai responses SSE line %q", line)}
	}); err != nil {
		return Message{}, openAIResponsesUsageValue(&usage), err
	}
	if !sawTerminal {
		return Message{}, openAIResponsesUsageValue(&usage), io.ErrUnexpectedEOF
	}

	if final != nil && len(final.Output) > 0 {
		msg, err := messageFromOpenAIResponses(*final)
		if err != nil {
			return Message{}, openAIResponsesUsageValue(final.Usage), err
		}
		return msg, openAIResponsesUsageValue(final.Usage), nil
	}
	for _, call := range calls {
		if call.Index < 0 {
			call.Index = len(items)
		}
		args := strings.TrimSpace(call.Arguments)
		if args == "" {
			args = "{}"
		}
		raw, err := json.Marshal(openAIResponsesFunctionCall{
			Type: "function_call", ID: call.ItemID, CallID: firstNonEmpty(call.CallID, call.ItemID),
			Name: call.Name, Arguments: args, Status: "completed",
		})
		if err != nil {
			return Message{}, openAIResponsesUsageValue(&usage), err
		}
		items[call.Index] = raw
	}
	indices := slices.Sorted(maps.Keys(items))
	output := make([]json.RawMessage, 0, len(indices))
	for _, index := range indices {
		output = append(output, items[index])
	}
	stopReason := "completed"
	if final != nil {
		stopReason = final.Status
	}
	msg, err := messageFromOpenAIResponsesOutput(output, stopReason, false)
	if err != nil {
		return Message{}, openAIResponsesUsageValue(&usage), err
	}
	if msg.Content == "" {
		msg.Content = fallbackText.String()
	}
	return msg, openAIResponsesUsageValue(&usage), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func openAIResponsesHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &HTTPError{Status: resp.Status, Body: strings.TrimSpace(string(body))}
}
