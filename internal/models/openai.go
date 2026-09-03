// Package models contains provider-neutral messages plus streaming clients for
// the compiled wire adapters.
package models

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Message is one chat message. Content is a string; ToolCalls set on assistant
// messages, ToolCallID on role "tool" results. A user message may also carry
// image Parts (multimodal/vision) — when Parts is non-empty it is sent as the
// content array and Content is mirrored as a text part so both stay in sync.
type Message struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Parts      []ContentPart `json:"-"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	// ProviderBlocks preserves opaque provider-native assistant blocks that
	// must be replayed unchanged on a later turn. Anthropic uses this for
	// thinking, redacted-thinking, and tool-use blocks whose signatures are
	// part of the provider's conversation protocol.
	ProviderBlocks []json.RawMessage `json:"provider_blocks,omitempty"`
	// StopReason is provider metadata retained with the assembled response.
	// It is not sent back as a message field; the owning adapter consumes it.
	StopReason string `json:"stop_reason,omitempty"`
	// Name is the function name on role "tool" messages. OpenAI ignores it,
	// but Moonshot/Kimi requires it ("tool messages need a resolvable tool
	// name") — without it every tool-using turn 400s.
	Name string `json:"name,omitempty"`
	// Authored marks a user message the human actually typed and submitted, as
	// opposed to one ghg injected on their behalf (steered background-task
	// results, goal-check continuations). Internal only — never sent to the
	// provider. Used so input-history recall cycles only real submissions.
	Authored bool `json:"authored,omitempty"`
	// SentAt is when the human submitted the message (local time). Internal
	// only — never sent to the provider; used by the rewind picker's
	// per-message timestamp. A pointer so omitempty drops it for injected and
	// pre-field messages (a zero time.Time struct is never omitted).
	SentAt *time.Time `json:"sent_at,omitempty"`
	// Usage is the token accounting for the assistant response that produced
	// this message. Internal only — never sent to the provider; powers
	// per-turn cost display and survives session resume (the in-memory
	// session totals do not).
	Usage *Usage `json:"usage,omitempty"`
	// Model records which model produced an assistant message ("id @
	// provider"), so a /model switch mid-session doesn't rewrite history
	// silently. Internal only — never sent to the provider.
	Model string `json:"model,omitempty"`
	// RewoundFrom notes that this message replaced an earlier clipped one
	// (rewind + resubmit). Internal only — never sent to the provider.
	RewoundFrom string `json:"rewound_from,omitempty"`
	// Output points to retained evidence for a bounded tool result. Internal
	// only — stripAuthored clears it before provider serialization.
	Output *OutputRef `json:"output,omitempty"`
	// ExitCode is the best-effort tool execution status for persisted views.
	// Internal only — it is not part of a provider tool message.
	ExitCode int `json:"exit_code,omitempty"`
	// Source identifies the integration that produced a tool result. Internal
	// only; provider requests receive the rendered content, not this field.
	Source string `json:"source,omitempty"`
}

// ContentPart is one element of a multimodal user message: either text or an
// image (as a data-URL). Kimi K3 and OpenAI vision models require `content`
// as an array of these parts rather than a plain string when images are
// attached. The wire shape is {"type":"text","text":...} and
// {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}.
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// TextContent returns the message's text, whether it was set directly
// (Content) or carried in a Parts array (multimodal messages mirror their
// text into both).
func (m Message) TextContent() string {
	if m.Content != "" {
		return m.Content
	}
	for _, p := range m.Parts {
		if p.Type == "text" {
			return p.Text
		}
	}
	return ""
}

// imageDataURL builds a base64 data URL for image bytes of the given format
// extension (png, jpg, gif, webp, bmp). jpg is emitted as image/jpeg.
func imageDataURL(ext string, data []byte) string {
	mime := "image/" + ext
	if ext == "jpg" {
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ImagePart builds an image ContentPart from raw bytes and a format extension.
func ImagePart(ext string, data []byte) ContentPart {
	p := ContentPart{Type: "image_url"}
	p.ImageURL = &struct {
		URL string `json:"url"`
	}{URL: imageDataURL(ext, data)}
	return p
}

// messageWire is the JSON shape of a Message. Content is `any` so it can be a
// plain string (text-only) or a []ContentPart array (multimodal). The internal
// fields are omitempty and cleared by stripAuthored before a provider request,
// so they only ever appear in the persisted session store.
type messageWire struct {
	Role           string            `json:"role"`
	Content        any               `json:"content"`
	ToolCalls      []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID     string            `json:"tool_call_id,omitempty"`
	ProviderBlocks []json.RawMessage `json:"provider_blocks,omitempty"`
	StopReason     string            `json:"stop_reason,omitempty"`
	Name           string            `json:"name,omitempty"`
	Authored       bool              `json:"authored,omitempty"`
	SentAt         *time.Time        `json:"sent_at,omitempty"`
	Usage          *Usage            `json:"usage,omitempty"`
	Model          string            `json:"model,omitempty"`
	RewoundFrom    string            `json:"rewound_from,omitempty"`
	Output         *OutputRef        `json:"output,omitempty"`
	LegacyOutput   *OutputRef        `json:"artifact,omitempty"`
	ExitCode       int               `json:"exit_code,omitempty"`
	Source         string            `json:"source,omitempty"`
}

// MarshalJSON sends Content as a plain string for text-only messages and as a
// content-parts array (text + images) for multimodal ones.
func (m Message) MarshalJSON() ([]byte, error) {
	w := messageWire{
		Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID,
		ProviderBlocks: m.ProviderBlocks, StopReason: m.StopReason, Name: m.Name,
		Authored: m.Authored, SentAt: m.SentAt, Usage: m.Usage,
		Model: m.Model, RewoundFrom: m.RewoundFrom, Output: m.Output, ExitCode: m.ExitCode, Source: m.Source,
	}
	if len(m.Parts) > 0 {
		parts := m.Parts
		if m.Content != "" {
			// keep the text part first so the model reads it before the images
			parts = append([]ContentPart{{Type: "text", Text: m.Content}}, parts...)
		}
		w.Content = parts
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts both the plain-string and content-parts wire forms.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		messageWire
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role, m.ToolCalls, m.ToolCallID, m.Name = raw.Role, raw.ToolCalls, raw.ToolCallID, raw.Name
	m.ProviderBlocks, m.StopReason = raw.ProviderBlocks, raw.StopReason
	m.Authored, m.SentAt, m.Usage, m.Model, m.RewoundFrom = raw.Authored, raw.SentAt, raw.Usage, raw.Model, raw.RewoundFrom
	m.Output, m.ExitCode, m.Source = raw.Output, raw.ExitCode, raw.Source
	if m.Output == nil {
		m.Output = raw.LegacyOutput
	}
	m.Content = ""
	m.Parts = nil
	if len(raw.Content) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return err
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			m.Content = p.Text
		case "image_url":
			m.Parts = append(m.Parts, p)
		}
	}
	return nil
}

// ToolCall is a model-requested tool invocation. DurationMs and ExitCode are
// ghg-internal execution bookkeeping (never sent to the provider): how long
// the tool ran and how it finished, for a future /tools perf view.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	Output     *OutputRef `json:"output,omitempty"`
	DurationMs int64      `json:"duration_ms,omitempty"`
	ExitCode   int        `json:"exit_code,omitempty"`
}

// stripAuthored returns a copy of msgs with the internal Authored marker and
// SentAt timestamp cleared — they're ghg-local bookkeeping (input-history
// recall, the rewind picker) and must never reach the provider. It copies
// because req.Messages typically aliases the caller's conversation slice,
// which must keep the fields for storage/recall.
func stripAuthored(msgs []Message) []Message {
	return stripAuthoredWithBlocks(msgs, false)
}

// stripAuthoredPreserveBlocks clears ghg bookkeeping while retaining
// provider-native blocks for the adapter that owns them. The generic OpenAI
// serializer uses stripAuthored so one provider's opaque blocks never leak to
// another provider.
func stripAuthoredPreserveBlocks(msgs []Message) []Message {
	out := stripAuthoredWithBlocks(msgs, true)
	// Anthropic represents tool failures with tool_result.is_error. ExitCode
	// is still ghg metadata and is consumed only while this adapter builds
	// that block; it is never serialized directly.
	for i := range out {
		if msgs[i].Role == "tool" {
			out[i].ExitCode = msgs[i].ExitCode
		}
	}
	return out
}

func stripAuthoredWithBlocks(msgs []Message, preserveBlocks bool) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Authored = false
		out[i].SentAt = nil
		out[i].Usage = nil
		out[i].Model = ""
		out[i].RewoundFrom = ""
		out[i].Output = nil
		out[i].ExitCode = 0
		out[i].Source = ""
		out[i].StopReason = ""
		if !preserveBlocks {
			out[i].ProviderBlocks = nil
		}
		for j := range out[i].ToolCalls {
			out[i].ToolCalls[j].Output = nil
			out[i].ToolCalls[j].DurationMs = 0
			out[i].ToolCalls[j].ExitCode = 0
		}
	}
	// Backfill tool-message Name from the owning call (older sessions predate
	// the field; providers that require it only look at Name).
	names := map[string]string{}
	for _, m := range out {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Function.Name
			}
		}
	}
	for i := range out {
		if out[i].Role == "tool" && out[i].Name == "" {
			out[i].Name = names[out[i].ToolCallID]
		}
	}
	return out
}

// repairToolHistory patches message-pairing defects that strict providers
// (Kimi K3, Gemini) reject with a 400 before the first token:
//
//   - assistant tool_calls with no following tool result (interrupted turn)
//     get a synthetic "(interrupted before execution)" result per call
//   - tool messages whose tool_call_id has no owning assistant tool_call
//     (compaction/rewind trimmed the caller) are flattened into plain user
//     context — the model loses the ID pairing but keeps the information
//
// Idempotent: a well-formed conversation comes through unchanged.
func repairToolHistory(msgs []Message) []Message {
	answered := make(map[string]bool, len(msgs))
	callName := make(map[string]string, len(msgs))
	for i, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			answered[tc.ID] = false
			callName[tc.ID] = tc.Function.Name
			for _, r := range msgs[i+1:] {
				if r.Role == "tool" && r.ToolCallID == tc.ID {
					answered[tc.ID] = true
					break
				}
				if r.Role == "assistant" || r.Role == "user" {
					break // results always immediately follow their call
				}
			}
		}
	}
	out := make([]Message, 0, len(msgs))
	var pending []string // unanswered call IDs from the last assistant message
	flush := func() {    // synthetics land after any real results in the run
		for _, id := range pending {
			out = append(out, Message{
				Role:       "tool",
				Content:    "(interrupted before execution)",
				ToolCallID: id,
				Name:       callName[id],
			})
		}
		pending = nil
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			if _, ok := answered[m.ToolCallID]; !ok {
				flush()
				// orphan: flatten into user context rather than drop the info
				out = append(out, Message{
					Role:    "user",
					Content: "[earlier tool result]\n" + m.Content,
				})
				continue
			}
			out = append(out, m)
			continue
		}
		flush()
		out = append(out, m)
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if !answered[tc.ID] {
					pending = append(pending, tc.ID)
				}
			}
		}
	}
	flush()
	return out
}

// Tool is a tool definition advertised to the model.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// NewTool builds a Tool from name, description, and a JSON Schema string.
func NewTool(name, desc, schema string) Tool {
	t := Tool{Type: "function"}
	t.Function.Name = name
	t.Function.Description = desc
	t.Function.Parameters = json.RawMessage(schema)
	return t
}

// Client talks to one provider endpoint.
type Client struct {
	transport
}

func newClient(baseURL, apiKey string) *Client {
	return &Client{transport: newTransport(baseURL, apiKey)}
}

// Request is a provider-neutral assistant request. Wire-only transport flags
// such as stream and stream_options are added by the selected adapter.
type Request struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	// ReasoningEnabled is an internal capability signal. Adapters lower it to
	// their protocol-specific toggle field; it must never be sent verbatim.
	ReasoningEnabled *bool `json:"-"`
}

// openAIRequest is the OpenAI wire shape for a provider-neutral Request.
// Stream controls transport behavior and therefore belongs to the adapter,
// not to the request passed between the agent and a Backend.
type openAIRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Thinking        *struct {
		Type string `json:"type"`
	} `json:"thinking,omitempty"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

func newOpenAIRequest(req Request, stream bool) openAIRequest {
	messages := stripAuthored(req.Messages)
	if stream {
		messages = repairToolHistory(messages)
	}
	wire := openAIRequest{
		Model:           req.Model,
		Messages:        messages,
		Tools:           req.Tools,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
		Stream:          stream,
	}
	if req.ReasoningEnabled != nil {
		typ := "disabled"
		if *req.ReasoningEnabled {
			typ = "enabled"
		}
		wire.Thinking = &struct {
			Type string `json:"type"`
		}{Type: typ}
	}
	if stream {
		wire.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	return wire
}

// Usage is the token accounting the provider reports for one request
// (prompt = input, completion = output). CachedTokens counts the slice of
// the prompt served from the provider's prompt cache, while
// CacheCreationTokens records provider-reported cache writes. Providers that
// omit usage leave all fields zero — the session totals just skip those calls.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	InputTokens      int `json:"input_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	// CacheCreationTokens is provider-reported prompt-cache write usage. The
	// existing Cached method continues to expose cache reads for cost/status
	// accounting; providers without cache-write reporting leave this zero.
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	// PromptTokensDetails nests the cache hit count (OpenAI-compatible).
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// Cached is the prompt-token count served from cache (0 when unreported).
func (u Usage) Cached() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// AddCached increments CachedTokens, allocating PromptTokensDetails if nil.
func (u *Usage) AddCached(tokens int) {
	if tokens <= 0 {
		return
	}
	if u.PromptTokensDetails == nil {
		u.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{}
	}
	u.PromptTokensDetails.CachedTokens += tokens
}

// Add accumulates token counts from another Usage into u.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	if cached := other.Cached(); cached > 0 {
		u.AddCached(cached)
	}
}

// Chunk delta payload from the SSE stream.
type delta struct {
	Content string `json:"content"`
	// ReasoningContent carries thinking tokens on reasoning models (deepseek,
	// grok, kimi, claude all emit it; claude also nests it in thinking_blocks).
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type chunk struct {
	Choices []struct {
		Delta        delta  `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *apiError `json:"error"`
	Usage *Usage    `json:"usage"`
}

type apiError struct {
	Message string `json:"message"`
}

// ModelInfo is one entry from the provider's GET /models list. Fields beyond
// the OpenAI spec (context_length, reasoning_efforts, pricing) are omitted
// by APIs that don't supply them.
type ModelInfo struct {
	ID                  string   `json:"id"`
	ContextLength       int      `json:"context_length,omitempty"`
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	ReasoningEfforts    []string `json:"reasoning_efforts,omitempty"`
	Pricing             *Pricing `json:"pricing,omitempty"`
	// InputModalities lists the input types the model accepts (OpenRouter
	// shape: ["text","image"]). Nil when the provider doesn't advertise it.
	InputModalities []string `json:"input_modalities,omitempty"`
}

// SupportsVision reports whether the model advertises image input.
func (mi ModelInfo) SupportsVision() bool {
	return slices.Contains(mi.InputModalities, "image")
}

// Pricing is the provider's per-token USD rates as decimal strings
// (OpenAI-compatible catalog shape). Nil Pricing on ModelInfo means the
// provider doesn't advertise prices.
type Pricing struct {
	Prompt         string `json:"prompt"`
	Completion     string `json:"completion"`
	InputCacheRead string `json:"input_cache_read,omitempty"`
}

// Rates parses the decimal-string prices into floats (0 for missing or
// unparseable fields).
func (p Pricing) Rates() (in, out, cacheRead float64) {
	in, _ = strconv.ParseFloat(p.Prompt, 64)
	out, _ = strconv.ParseFloat(p.Completion, 64)
	cacheRead, _ = strconv.ParseFloat(p.InputCacheRead, 64)
	return
}

// SessionCost returns the USD spend for cumulative usage u at per-token
// rates. Cached prompt tokens are billed at the cache-read rate when
// advertised, else at the full input rate (pi models.ts calculateCost has
// the same shape, plus a cache-write term OpenAI-compatible usage lacks).
func SessionCost(u Usage, in, out, cacheRead float64) float64 {
	cached := u.Cached()
	if cacheRead == 0 {
		cacheRead = in
	}
	return float64(u.PromptTokens-cached)*in +
		float64(cached)*cacheRead +
		float64(u.CompletionTokens)*out
}

// Models fetches GET /models from the provider.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	hr, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if err := c.setRequestHeaders(hr); err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(hr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, readHTTPError(resp)
	}
	var list struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list.Data, nil
}

// Probe performs one authenticated chat-completions request with a real model
// id and a one-token output bound. An empty modelID uses a stable OpenAI model
// for profiles that do not expose a catalog.
func (c *Client) Probe(ctx context.Context, modelID string) error {
	body, err := json.Marshal(openAIRequest{
		Model: probeModel(modelID, authProbeModel),
		Messages: []Message{{
			Role:    "user",
			Content: "ghg authentication probe",
		}},
		MaxTokens: 1,
	})
	if err != nil {
		return err
	}
	return authenticatedProbe(ctx, c.httpClient(), c.BaseURL+"/chat/completions", body, c.setRequestHeaders)
}

// AdapterProtocol reports the Chat Completions adapter selected by this client.
func (c *Client) AdapterProtocol() Protocol { return ProtocolOpenAIChatCompletions }

// Stream implements Backend.
func (c *Client) Stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	return c.stream(ctx, req, sink)
}

// stream is the request-local event-sink implementation used by the
// provider-neutral OpenAI adapter. Unlike Client.OnRetry, the sink is not
// shared mutable state and is safe for concurrent backend calls.
func (c *Client) stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if req.SessionID != "" {
		ctx = WithSessionID(ctx, req.SessionID)
	}
	body, err := json.Marshal(newOpenAIRequest(req, true))
	if err != nil {
		return Message{}, Usage{}, err
	}
	var last error
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		emitted := false // true once any visible delta reached the caller
		wrapText, wrapThink := sink.OnText, sink.OnThink
		if sink.OnText != nil {
			wrapText = func(s string) { emitted = true; sink.OnText(s) }
		}
		if sink.OnThink != nil {
			wrapThink = func(s string) { emitted = true; sink.OnThink(s) }
		}
		msg, usage, err := c.streamOnce(ctx, body, wrapText, wrapThink)
		if err == nil {
			return msg, usage, nil
		}
		last = err
		// Retry only transient failures the caller hasn't seen output from.
		if emitted || !retryable(err) || attempt == c.attempts() {
			break
		}
		delay := backoff(attempt)
		if sink.OnRetry != nil {
			sink.OnRetry(RetryEvent{Attempt: attempt, Max: c.attempts(), Delay: delay, Err: err})
		}
		if serr := sleep(ctx, delay); serr != nil {
			return Message{}, Usage{}, serr
		}
	}
	return Message{}, Usage{}, last
}

// streamOnce performs a single streaming request attempt; the Stream retry
// wrapper calls it per attempt and reads its own `emitted` flag (set by the
// wrapped callbacks) to decide whether a retry would replay visible output.
func (c *Client) streamOnce(ctx context.Context, body []byte, onText, onThink func(string)) (Message, Usage, error) {
	hr, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if err := c.setRequestHeaders(hr); err != nil {
		return Message{}, Usage{}, err
	}
	resp, err := c.httpClient().Do(hr)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Message{}, Usage{}, readHTTPError(resp)
	}

	msg := Message{Role: "assistant"}
	var usage Usage      // from the terminal chunk (include_usage); zero if omitted
	var calls []ToolCall // indexed by stream tool_call index
	finish := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			if isTransientErrorMessage(ch.Error.Message) {
				return Message{}, usage, fmt.Errorf("api error: %s", ch.Error.Message)
			}
			return Message{}, usage, nonRetryable{fmt.Errorf("api error: %s", ch.Error.Message)}
		}
		if ch.Usage != nil {
			u := *ch.Usage // the terminal usage chunk carries empty choices
			if u.PromptTokens == 0 && u.InputTokens > 0 {
				u.PromptTokens = u.InputTokens
			}
			if u.CompletionTokens == 0 && u.OutputTokens > 0 {
				u.CompletionTokens = u.OutputTokens
			}
			usage = u
		}
		if len(ch.Choices) == 0 {
			continue
		}
		if fr := ch.Choices[0].FinishReason; fr != "" {
			finish = fr
		}
		d := ch.Choices[0].Delta
		if d.ReasoningContent != "" {
			if onThink != nil {
				onThink(d.ReasoningContent)
			}
		}
		if d.Content != "" {
			msg.Content += d.Content
			if onText != nil {
				onText(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			for len(calls) <= tc.Index {
				calls = append(calls, ToolCall{Type: "function"})
			}
			cur := &calls[tc.Index]
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Function.Name += tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, usage, err
	}
	// Never execute tool calls from a max_tokens-truncated response: the
	// streamed JSON arguments may be silently incomplete.
	if finish == "length" && len(calls) > 0 {
		calls = nil
		msg.Content += "\n[response truncated by max_tokens; tool calls discarded]"
	}
	msg.ToolCalls = calls
	return msg, usage, nil
}

// Complete sends a non-streaming chat request and returns the assistant text
// content plus the reported usage. It's used internally by compaction's
// summary call, where streaming would just add UI noise for a one-shot
// synthesis.
func (c *Client) Complete(ctx context.Context, req Request) (Message, Usage, error) {
	msg, usage, err := c.complete(ctx, req, EventSink{OnRetry: c.OnRetry})
	return msg, usage, err
}

// complete is the request-local completion implementation used by the
// provider-neutral OpenAI adapter.
func (c *Client) complete(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if req.SessionID != "" {
		ctx = WithSessionID(ctx, req.SessionID)
	}
	body, err := json.Marshal(newOpenAIRequest(req, false))
	if err != nil {
		return Message{}, Usage{}, err
	}
	var last error
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		var msg Message
		var usage Usage
		msg, usage, err = c.completeOnce(ctx, body)
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
		if serr := sleep(ctx, delay); serr != nil {
			return Message{}, Usage{}, serr
		}
	}
	return Message{}, Usage{}, last
}

// completeOnce performs one non-streaming request attempt.
func (c *Client) completeOnce(ctx context.Context, body []byte) (Message, Usage, error) {
	hr, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if err := c.setRequestHeaders(hr); err != nil {
		return Message{}, Usage{}, err
	}
	resp, err := c.httpClient().Do(hr)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Message{}, Usage{}, readHTTPError(resp)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Role      string     `json:"role"`
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Message{}, Usage{}, err
	}
	if len(out.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("no choices in completion response")
	}
	var usage Usage
	if out.Usage != nil {
		u := *out.Usage
		if u.PromptTokens == 0 && u.InputTokens > 0 {
			u.PromptTokens = u.InputTokens
		}
		if u.CompletionTokens == 0 && u.OutputTokens > 0 {
			u.CompletionTokens = u.OutputTokens
		}
		usage = u
	}
	choice := out.Choices[0]
	role := choice.Message.Role
	if role == "" {
		role = "assistant"
	}
	return Message{
		Role:       role,
		Content:    choice.Message.Content,
		ToolCalls:  choice.Message.ToolCalls,
		StopReason: choice.FinishReason,
	}, usage, nil
}
