package models

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	defaultAnthropicMaxTokens = 8192
	anthropicModelsPageSize   = 1000
	maxAnthropicModels        = 10000
	maxAnthropicSSELine       = 10 * 1024 * 1024
)

// AnthropicClient talks to the native Anthropic Messages API.
type AnthropicClient struct {
	transport
}

// newAnthropicClient creates an Anthropic Messages adapter. BaseURL is
// normally the profile endpoint ending in /v1.
func newAnthropicClient(baseURL, apiKey string) *AnthropicClient {
	transport := newTransport(baseURL, apiKey)
	transport.Headers = map[string]string{"anthropic-version": "2023-06-01"}
	return &AnthropicClient{
		transport: transport,
	}
}

// AdapterProtocol reports the Anthropic Messages adapter selected by this client.
func (c *AnthropicClient) AdapterProtocol() Protocol { return ProtocolAnthropicMessages }

// Stream implements Backend.
func (c *AnthropicClient) Stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	return c.stream(ctx, req, sink)
}

// Complete implements Backend.
func (c *AnthropicClient) Complete(ctx context.Context, req Request) (Message, Usage, error) {
	msg, usage, err := c.complete(ctx, req, EventSink{OnRetry: c.OnRetry})
	return msg, usage, err
}

type anthropicRequest struct {
	Model        string              `json:"model"`
	MaxTokens    int                 `json:"max_tokens"`
	System       []anthropicBlock    `json:"system,omitempty"`
	Messages     []anthropicMessage  `json:"messages"`
	Tools        []anthropicTool     `json:"tools,omitempty"`
	Stream       bool                `json:"stream,omitempty"`
	Thinking     *anthropicThinking  `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputCfg `json:"output_config,omitempty"`
}

type anthropicMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Source       *anthropicImageSource  `json:"source,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        json.RawMessage        `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      any                    `json:"content,omitempty"`
	IsError      bool                   `json:"is_error,omitempty"`
	Thinking     string                 `json:"thinking,omitempty"`
	Signature    string                 `json:"signature,omitempty"`
	Data         string                 `json:"data,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type anthropicOutputCfg struct {
	Effort string `json:"effort"`
}

// newAnthropicRequest translates the neutral request into the Messages wire
// shape. Anthropic keeps system content at the top level and represents tool
// results as user content blocks, so this is deliberately not a JSON tag-only
// conversion.
func newAnthropicRequest(req Request, stream bool) (anthropicRequest, error) {
	msgs := repairToolHistory(stripAuthoredPreserveBlocks(req.Messages))
	wire := anthropicRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Stream:    stream,
	}
	if wire.MaxTokens <= 0 {
		wire.MaxTokens = defaultAnthropicMaxTokens
	}

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			blocks, err := anthropicSystemBlocks(msg)
			if err != nil {
				return anthropicRequest{}, err
			}
			wire.System = append(wire.System, blocks...)
		case "user":
			blocks, err := anthropicUserBlocks(msg)
			if err != nil {
				return anthropicRequest{}, err
			}
			appendAnthropicMessage(&wire.Messages, "user", blocks)
		case "assistant":
			blocks, err := anthropicAssistantBlocks(msg)
			if err != nil {
				return anthropicRequest{}, err
			}
			appendAnthropicMessage(&wire.Messages, "assistant", blocks)
		case "tool":
			if err := appendAnthropicToolResult(&wire.Messages, msg); err != nil {
				return anthropicRequest{}, err
			}
		default:
			return anthropicRequest{}, fmt.Errorf("models: anthropic cannot translate message role %q", msg.Role)
		}
	}
	if len(wire.Messages) == 0 {
		return anthropicRequest{}, errors.New("models: anthropic request needs at least one user or assistant message")
	}

	var err error
	wire.Tools, err = anthropicTools(req.Tools)
	if err != nil {
		return anthropicRequest{}, err
	}
	wire.Thinking, wire.OutputConfig = anthropicReasoningRequest(req)
	applyAnthropicCachePolicy(&wire)
	return wire, nil
}

func anthropicSystemBlocks(msg Message) ([]anthropicBlock, error) {
	blocks := make([]anthropicBlock, 0, 1+len(msg.Parts))
	textAdded := false
	if msg.Content != "" || len(msg.Parts) == 0 {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: msg.Content})
		textAdded = true
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			if textAdded && part.Text == msg.Content {
				continue
			}
			blocks = append(blocks, anthropicBlock{Type: "text", Text: part.Text})
			textAdded = true
		case "image_url":
			return nil, errors.New("models: anthropic system content cannot contain images")
		default:
			return nil, fmt.Errorf("models: anthropic does not support content part %q in system content", part.Type)
		}
	}
	return blocks, nil
}

func anthropicUserBlocks(msg Message) ([]json.RawMessage, error) {
	blocks := make([]json.RawMessage, 0, 1+len(msg.Parts))
	textAdded := false
	if msg.Content != "" || len(msg.Parts) == 0 {
		raw, err := marshalAnthropicBlock(anthropicBlock{Type: "text", Text: msg.Content})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, raw)
		textAdded = true
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			if textAdded && part.Text == msg.Content {
				continue
			}
			raw, err := marshalAnthropicBlock(anthropicBlock{Type: "text", Text: part.Text})
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, raw)
			textAdded = true
		case "image_url":
			if part.ImageURL == nil {
				return nil, errors.New("models: anthropic image part has no URL")
			}
			block, err := anthropicImageBlock(part.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			raw, err := marshalAnthropicBlock(block)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, raw)
		default:
			return nil, fmt.Errorf("models: anthropic does not support content part %q", part.Type)
		}
	}
	return blocks, nil
}

func anthropicAssistantBlocks(msg Message) ([]json.RawMessage, error) {
	if len(msg.ProviderBlocks) > 0 {
		blocks := make([]json.RawMessage, 0, len(msg.ProviderBlocks))
		for _, raw := range msg.ProviderBlocks {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &header); err != nil || header.Type == "" {
				if err == nil {
					err = errors.New("missing type")
				}
				return nil, fmt.Errorf("models: invalid preserved anthropic block: %w", err)
			}
			blocks = append(blocks, append(json.RawMessage(nil), raw...))
		}
		return blocks, nil
	}

	blocks := make([]json.RawMessage, 0, 1+len(msg.ToolCalls))
	if msg.Content != "" || len(msg.ToolCalls) == 0 {
		raw, err := marshalAnthropicBlock(anthropicBlock{Type: "text", Text: msg.Content})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, raw)
	}
	for _, call := range msg.ToolCalls {
		input := strings.TrimSpace(call.Function.Arguments)
		if input == "" {
			input = "{}"
		}
		if !json.Valid([]byte(input)) {
			return nil, fmt.Errorf("models: anthropic tool %q has invalid JSON input", call.Function.Name)
		}
		raw, err := marshalAnthropicBlock(anthropicBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: json.RawMessage(input),
		})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, raw)
	}
	return blocks, nil
}

func appendAnthropicToolResult(messages *[]anthropicMessage, msg Message) error {
	if msg.ToolCallID == "" {
		return errors.New("models: anthropic tool result has no tool_use id")
	}
	raw, err := marshalAnthropicBlock(anthropicBlock{
		Type:      "tool_result",
		ToolUseID: msg.ToolCallID,
		Content:   msg.Content,
		IsError:   msg.ExitCode != 0,
	})
	if err != nil {
		return err
	}
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == "user" && anthropicMessageOnlyToolResults((*messages)[len(*messages)-1]) {
		last := &(*messages)[len(*messages)-1]
		last.Content = append(last.Content, raw)
		return nil
	}
	if len(*messages) == 0 || (*messages)[len(*messages)-1].Role != "assistant" {
		return fmt.Errorf("models: anthropic tool result %q is not after an assistant tool_use", msg.ToolCallID)
	}
	*messages = append(*messages, anthropicMessage{Role: "user", Content: []json.RawMessage{raw}})
	return nil
}

func appendAnthropicMessage(messages *[]anthropicMessage, role string, blocks []json.RawMessage) {
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == role {
		last := &(*messages)[len(*messages)-1]
		last.Content = append(last.Content, blocks...)
		return
	}
	*messages = append(*messages, anthropicMessage{Role: role, Content: blocks})
}

func anthropicMessageOnlyToolResults(msg anthropicMessage) bool {
	if len(msg.Content) == 0 {
		return false
	}
	for _, raw := range msg.Content {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &block) != nil || block.Type != "tool_result" {
			return false
		}
	}
	return true
}

func anthropicTools(tools []Tool) ([]anthropicTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		name := tool.Function.Name
		if name == "" {
			return nil, errors.New("models: anthropic tool name is required")
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("models: anthropic tool %q has invalid input schema", name)
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: tool.Function.Description,
			InputSchema: append(json.RawMessage(nil), schema...),
		})
	}
	return out, nil
}

func anthropicReasoning(effort string) (*anthropicThinking, *anthropicOutputCfg) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "off", "none":
		return &anthropicThinking{Type: "disabled"}, nil
	case "low", "medium", "high", "xhigh", "max":
		// Current Messages models use adaptive thinking plus output_config.effort.
		// Keeping both fields here lets the neutral effort setting control the
		// whole response while still requesting thinking blocks when supported.
		return &anthropicThinking{Type: "adaptive", Display: "summarized"}, &anthropicOutputCfg{Effort: strings.ToLower(strings.TrimSpace(effort))}
	default:
		return nil, nil
	}
}

func anthropicReasoningRequest(req Request) (*anthropicThinking, *anthropicOutputCfg) {
	if req.ReasoningEnabled == nil {
		return anthropicReasoning(req.ReasoningEffort)
	}
	if !*req.ReasoningEnabled {
		return &anthropicThinking{Type: "disabled"}, nil
	}
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort == "" || effort == "on" {
		// Anthropic's binary thinking form requires a budget. The neutral
		// toggle is used only when models.dev advertises no graded effort.
		maxTokens := req.MaxTokens
		if maxTokens <= 0 {
			maxTokens = defaultAnthropicMaxTokens
		}
		budget := maxTokens / 2
		if budget < 1024 {
			budget = 1024
		}
		if budget >= maxTokens {
			budget = maxTokens - 1
		}
		return &anthropicThinking{Type: "enabled", BudgetTokens: budget}, nil
	}
	return anthropicReasoning(effort)
}

func applyAnthropicCachePolicy(req *anthropicRequest) {
	// Tools and the first system block are the stable prompt prefix in the
	// ghg. Per-round todo/summary/output blocks are appended later and
	// intentionally remain after these breakpoints. The final conversation
	// block is the rolling breakpoint for the completed conversation prefix.
	if len(req.Tools) > 0 {
		req.Tools[len(req.Tools)-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
	}
	if len(req.System) > 0 {
		req.System[0].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if len(req.Messages[i].Content) == 0 {
			continue
		}
		last := &req.Messages[i].Content
		(*last)[len(*last)-1] = addAnthropicCacheControl((*last)[len(*last)-1])
		break
	}
}

func addAnthropicCacheControl(raw json.RawMessage) json.RawMessage {
	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil || block == nil {
		return raw
	}
	block["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
	updated, err := json.Marshal(block)
	if err != nil {
		return raw
	}
	return updated
}

func anthropicImageBlock(rawURL string) (anthropicBlock, error) {
	if rawURL == "" {
		return anthropicBlock{}, errors.New("models: anthropic image URL is empty")
	}
	if !strings.HasPrefix(rawURL, "data:") {
		return anthropicBlock{
			Type:   "image",
			Source: &anthropicImageSource{Type: "url", URL: rawURL},
		}, nil
	}
	header, encoded, ok := strings.Cut(rawURL, ",")
	if !ok {
		return anthropicBlock{}, errors.New("models: malformed image data URL")
	}
	header = strings.TrimPrefix(header, "data:")
	parts := strings.Split(header, ";")
	if len(parts) < 2 || parts[len(parts)-1] != "base64" || parts[0] == "" {
		return anthropicBlock{}, errors.New("models: anthropic images require base64 data URLs")
	}
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		return anthropicBlock{}, fmt.Errorf("models: malformed image data URL: %w", err)
	}
	return anthropicBlock{
		Type: "image",
		Source: &anthropicImageSource{
			Type:      "base64",
			MediaType: parts[0],
			Data:      encoded,
		},
	}, nil
}

func marshalAnthropicBlock(block anthropicBlock) (json.RawMessage, error) {
	b, err := json.Marshal(block)
	if err != nil {
		return nil, fmt.Errorf("models: marshal anthropic content block: %w", err)
	}
	return b, nil
}

// Stream sends a native Messages streaming request. As with the OpenAI
// client, retries are allowed only before the first visible callback.
func (c *AnthropicClient) stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if req.SessionID != "" {
		ctx = WithSessionID(ctx, req.SessionID)
	}
	wire, err := newAnthropicRequest(req, true)
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

func (c *AnthropicClient) streamOnce(ctx context.Context, body []byte, onText, onThink func(string)) (Message, Usage, error) {
	endpoint, err := c.endpoint("/messages")
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
		return Message{}, Usage{}, anthropicHTTPError(resp)
	}
	return consumeAnthropicSSE(resp.Body, onText, onThink)
}

// Complete performs a non-streaming Messages request for compaction and
// other one-shot calls.
func (c *AnthropicClient) complete(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if req.SessionID != "" {
		ctx = WithSessionID(ctx, req.SessionID)
	}
	wire, err := newAnthropicRequest(req, false)
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

func (c *AnthropicClient) completeOnce(ctx context.Context, body []byte) (Message, Usage, error) {
	endpoint, err := c.endpoint("/messages")
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
		return Message{}, Usage{}, anthropicHTTPError(resp)
	}
	var response anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Message{}, Usage{}, err
		}
		return Message{}, Usage{}, nonRetryable{fmt.Errorf("malformed anthropic response: %w", err)}
	}
	usage := anthropicUsageValue(response.Usage)
	msg, err := messageFromAnthropicBlocks(response.Content, response.StopReason)
	if err != nil {
		return Message{}, Usage{}, err
	}
	return msg, usage, nil
}

func (c *AnthropicClient) endpoint(suffix string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", errors.New("models: anthropic base url is required")
	}
	return base + suffix, nil
}

func anthropicHTTPError(resp *http.Response) error {
	return readHTTPError(resp)
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	PromptTokens             int `json:"prompt_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) inputTokens() int {
	if u.InputTokens > 0 {
		return u.InputTokens
	}
	return u.PromptTokens
}

func (u anthropicUsage) outputTokens() int {
	if u.OutputTokens > 0 {
		return u.OutputTokens
	}
	return u.CompletionTokens
}

func mergeAnthropicUsage(dst *anthropicUsage, src anthropicUsage) {
	if in := src.inputTokens(); in > 0 {
		dst.InputTokens = in
	}
	if out := src.outputTokens(); out > 0 {
		dst.OutputTokens = out
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
}

type anthropicResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      anthropicUsage    `json:"usage"`
}

func anthropicUsageValue(raw anthropicUsage) Usage {
	usage := Usage{
		PromptTokens:        raw.inputTokens() + raw.CacheCreationInputTokens + raw.CacheReadInputTokens,
		CompletionTokens:    raw.outputTokens(),
		CacheCreationTokens: raw.CacheCreationInputTokens,
	}
	if raw.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: raw.CacheReadInputTokens}
	}
	return usage
}

type anthropicResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
}

func messageFromAnthropicBlocks(blocks []json.RawMessage, stopReason string) (Message, error) {
	msg := Message{Role: "assistant", StopReason: stopReason}
	msg.ProviderBlocks = make([]json.RawMessage, 0, len(blocks))
	for _, raw := range blocks {
		var block anthropicResponseBlock
		if err := json.Unmarshal(raw, &block); err != nil || block.Type == "" {
			if err == nil {
				err = errors.New("missing type")
			}
			return Message{}, nonRetryable{fmt.Errorf("malformed anthropic content block: %w", err)}
		}
		msg.ProviderBlocks = append(msg.ProviderBlocks, append(json.RawMessage(nil), raw...))
		switch block.Type {
		case "text":
			msg.Content += block.Text
		case "tool_use":
			input := strings.TrimSpace(string(block.Input))
			if input == "" {
				input = "{}"
			}
			if !json.Valid([]byte(input)) {
				return Message{}, nonRetryable{fmt.Errorf("malformed anthropic tool input for %q", block.Name)}
			}
			call := ToolCall{ID: block.ID, Type: "function"}
			call.Function.Name = block.Name
			call.Function.Arguments = input
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	if stopReason == "max_tokens" && len(msg.ToolCalls) > 0 {
		msg.ToolCalls = nil
		msg.Content += "\n[response truncated by max_tokens; tool calls discarded]"
	}
	return msg, nil
}

type anthropicEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`
	Message      *anthropicEventMessage `json:"message"`
	ContentBlock json.RawMessage        `json:"content_block"`
	Delta        *anthropicEventDelta   `json:"delta"`
	Usage        *anthropicUsage        `json:"usage"`
	Error        *anthropicEventError   `json:"error"`
}

type anthropicEventMessage struct {
	Usage      anthropicUsage    `json:"usage"`
	StopReason string            `json:"stop_reason"`
	Content    []json.RawMessage `json:"content"`
}

type anthropicEventDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type anthropicEventError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicStreamBlock struct {
	typ       string
	raw       map[string]json.RawMessage
	text      string
	thinking  string
	signature string
	partial   string
	stopped   bool
}

func consumeAnthropicSSE(r io.Reader, onText, onThink func(string)) (Message, Usage, error) {
	blocks := map[int]*anthropicStreamBlock{}
	var inputUsage anthropicUsage
	stopReason := ""
	sawStop := false

	handle := func(data []byte) error {
		if string(data) == "[DONE]" {
			sawStop = true
			return nil
		}
		var event anthropicEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nonRetryable{fmt.Errorf("malformed anthropic SSE event: %w", err)}
		}
		if event.Type == "" {
			return nonRetryable{errors.New("malformed anthropic SSE event: missing type")}
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				mergeAnthropicUsage(&inputUsage, event.Message.Usage)
				if event.Message.StopReason != "" {
					stopReason = event.Message.StopReason
				}
			}
		case "content_block_start":
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(event.ContentBlock, &raw); err != nil {
				return nonRetryable{fmt.Errorf("malformed anthropic content block start: %w", err)}
			}
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.ContentBlock, &header); err != nil || header.Type == "" {
				if err == nil {
					err = errors.New("missing type")
				}
				return nonRetryable{fmt.Errorf("malformed anthropic content block start: %w", err)}
			}
			if _, exists := blocks[event.Index]; exists {
				return nonRetryable{fmt.Errorf("duplicate anthropic content block index %d", event.Index)}
			}
			state := &anthropicStreamBlock{typ: header.Type, raw: raw}
			state.text = rawString(raw["text"])
			state.thinking = rawString(raw["thinking"])
			state.signature = rawString(raw["signature"])
			blocks[event.Index] = state
		case "content_block_delta":
			state := blocks[event.Index]
			if state == nil || state.stopped || event.Delta == nil {
				return nonRetryable{fmt.Errorf("malformed anthropic content block delta at index %d", event.Index)}
			}
			delta := event.Delta
			switch delta.Type {
			case "text_delta":
				state.text += delta.Text
				if delta.Text != "" {
					if onText != nil {
						onText(delta.Text)
					}
				}
			case "thinking_delta":
				state.thinking += delta.Thinking
				if delta.Thinking != "" && onThink != nil {
					onThink(delta.Thinking)
				}
			case "signature_delta":
				state.signature += delta.Signature
			case "input_json_delta":
				state.partial += delta.PartialJSON
			}
		case "content_block_stop":
			state := blocks[event.Index]
			if state == nil || state.stopped {
				return nonRetryable{fmt.Errorf("malformed anthropic content block stop at index %d", event.Index)}
			}
			state.stopped = true
		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
			if event.Usage != nil {
				mergeAnthropicUsage(&inputUsage, *event.Usage)
			}
		case "message_stop":
			sawStop = true
			if event.Usage != nil {
				mergeAnthropicUsage(&inputUsage, *event.Usage)
			}
		case "error":
			message := "anthropic stream error"
			if event.Error != nil {
				message = event.Error.Message
				if event.Error.Type != "" {
					message = event.Error.Type + ": " + message
				}
			}
			if isTransientErrorMessage(message) {
				return errors.New(message)
			}
			return nonRetryable{errors.New(message)}
		case "ping":
			// Keep-alive event; no state change.
		default:
			// Unknown event types are forward-compatible. Content and message
			// events above are the complete set needed for assembly.
		}
		return nil
	}

	if err := scanAnthropicSSE(r, handle); err != nil {
		return Message{}, anthropicUsageValue(inputUsage), err
	}
	if !sawStop {
		return Message{}, anthropicUsageValue(inputUsage), io.ErrUnexpectedEOF
	}
	for index, state := range blocks {
		if !state.stopped {
			return Message{}, anthropicUsageValue(inputUsage), nonRetryable{fmt.Errorf("anthropic content block %d did not stop", index)}
		}
	}
	indices := slices.Sorted(maps.Keys(blocks))
	content := make([]json.RawMessage, 0, len(indices))
	for _, index := range indices {
		raw, err := finishAnthropicStreamBlock(blocks[index], stopReason)
		if err != nil {
			return Message{}, anthropicUsageValue(inputUsage), nonRetryable{err}
		}
		content = append(content, raw)
	}
	usage := anthropicUsageValue(inputUsage)
	msg, err := messageFromAnthropicBlocks(content, stopReason)
	if err != nil {
		return Message{}, usage, err
	}
	return msg, usage, nil
}

func scanAnthropicSSE(r io.Reader, handle func([]byte) error) error {
	return scanSSE(r, maxAnthropicSSELine, func(_ string, data []byte) error {
		return handle(data)
	}, func(line string) error {
		return nonRetryable{fmt.Errorf("malformed anthropic SSE line %q", line)}
	})
}

func rawString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

func finishAnthropicStreamBlock(state *anthropicStreamBlock, stopReason string) (json.RawMessage, error) {
	if state.raw == nil {
		state.raw = map[string]json.RawMessage{"type": json.RawMessage(`"unknown"`)}
	}
	setString := func(name, value string) {
		encoded, _ := json.Marshal(value)
		state.raw[name] = encoded
	}
	switch state.typ {
	case "text":
		setString("text", state.text)
	case "thinking":
		setString("thinking", state.thinking)
		setString("signature", state.signature)
	case "tool_use":
		if state.partial != "" {
			if !json.Valid([]byte(state.partial)) {
				if stopReason == "max_tokens" {
					state.raw["input"] = json.RawMessage(`{}`)
				} else {
					return nil, fmt.Errorf("malformed anthropic tool input")
				}
			} else {
				state.raw["input"] = json.RawMessage(state.partial)
			}
		} else if len(state.raw["input"]) == 0 {
			state.raw["input"] = json.RawMessage(`{}`)
		}
	}
	b, err := json.Marshal(state.raw)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Models implements Anthropic's paginated GET /models endpoint and maps its
// capability metadata onto the catalog shape shared by the UI.
func (c *AnthropicClient) Models(ctx context.Context) ([]ModelInfo, error) {
	var out []ModelInfo
	after := ""
	for len(out) < maxAnthropicModels {
		endpoint, err := c.modelsEndpoint(after)
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
		var page anthropicModelsPage
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			err = anthropicHTTPError(resp)
			_ = resp.Body.Close()
			return nil, err
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if err != nil {
			return nil, nonRetryable{fmt.Errorf("malformed anthropic models response: %w", err)}
		}
		for _, model := range page.Data {
			if model.ID == "" {
				return nil, nonRetryable{errors.New("malformed anthropic model: missing id")}
			}
			out = append(out, modelInfoFromAnthropic(model))
			if len(out) == maxAnthropicModels {
				return out, nil
			}
		}
		if !page.HasMore {
			return out, nil
		}
		if page.LastID == "" || page.LastID == after {
			return nil, nonRetryable{errors.New("malformed anthropic models pagination")}
		}
		after = page.LastID
	}
	return out, nil
}

// Probe performs one authenticated Messages request with a real model id and
// a one-token output bound. An empty modelID uses a stable Anthropic model for
// profiles that do not expose a catalog.
func (c *AnthropicClient) Probe(ctx context.Context, modelID string) error {
	wire, err := newAnthropicRequest(Request{
		Model:     probeModel(modelID, anthropicProbeModel),
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
	endpoint, err := c.endpoint("/messages")
	if err != nil {
		return err
	}
	return authenticatedProbe(ctx, c.httpClient(), endpoint, body, c.setRequestHeaders)
}

type anthropicModelsPage struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	LastID  string           `json:"last_id"`
}

type anthropicModel struct {
	ID             string `json:"id"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
	Capabilities   struct {
		ImageInput anthropicCapability `json:"image_input"`
		Thinking   anthropicCapability `json:"thinking"`
		Effort     anthropicCapability `json:"effort"`
	} `json:"capabilities"`
}

type anthropicCapability struct {
	Supported bool     `json:"supported"`
	Levels    []string `json:"levels"`
}

func (c *AnthropicClient) modelsEndpoint(after string) (string, error) {
	endpoint, err := c.endpoint("/models")
	if err != nil {
		return "", err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := u.Query()
	query.Set("limit", fmt.Sprint(anthropicModelsPageSize))
	if after != "" {
		query.Set("after_id", after)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func modelInfoFromAnthropic(model anthropicModel) ModelInfo {
	info := ModelInfo{
		ID:                  model.ID,
		ContextLength:       model.MaxInputTokens,
		MaxCompletionTokens: model.MaxTokens,
		InputModalities:     []string{"text"},
	}
	if model.Capabilities.ImageInput.Supported {
		info.InputModalities = append(info.InputModalities, "image")
	}
	if model.Capabilities.Effort.Supported {
		info.ReasoningEfforts = append(info.ReasoningEfforts, model.Capabilities.Effort.Levels...)
		if len(info.ReasoningEfforts) == 0 {
			info.ReasoningEfforts = []string{"low", "medium", "high"}
		}
	}
	if len(info.ReasoningEfforts) == 0 && model.Capabilities.Thinking.Supported {
		info.ReasoningEfforts = []string{"low", "medium", "high"}
	}
	return info
}
