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
	"strings"
)

// EventSink receives provider-neutral events emitted while a backend streams
// an assistant response. All callbacks are optional and run on the caller's
// goroutine unless an adapter documents otherwise.
type EventSink struct {
	OnText  func(delta string)
	OnThink func(delta string)
	OnRetry func(RetryEvent)
}

// Backend is the execution contract consumed by the agent. Implementations
// translate Request and Message to and from their provider's wire protocol.
// Model discovery is deliberately not part of this interface; see
// CatalogBackend.
type Backend interface {
	Stream(context.Context, Request, EventSink) (Message, Usage, error)
	Complete(context.Context, Request) (Message, Usage, error)
}

// ProtocolBackend exposes the compiled wire protocol selected for a backend.
// Agent telemetry uses this optional capability to report the adapter without
// coupling the agent loop to concrete provider implementations.
type ProtocolBackend interface {
	Backend
	AdapterProtocol() Protocol
}

// CatalogBackend is an optional capability for providers that expose model
// discovery. A backend does not need this capability to serve a configured
// model, which keeps local endpoints without /models usable.
type CatalogBackend interface {
	Models(context.Context) ([]ModelInfo, error)
}

// ProbeBackend is an optional authenticated health check for providers whose
// profile deliberately does not use model discovery. It must perform one
// bounded protocol-native request and return only whether authentication and
// endpoint access succeeded.
type ProbeBackend interface {
	Probe(context.Context, string) error
}

const (
	// These are real, broadly available model IDs used only when a profile has
	// no catalog from which auth can select a model. Public catalogs pass their
	// first advertised ID instead.
	authProbeModel      = "gpt-4o-mini"
	anthropicProbeModel = "claude-3-5-haiku-latest"
	probeAuthErrorType  = "autherror"
)

// authenticatedProbe sends one bounded request using a real model ID. The
// provider's error type, rather than the HTTP status alone, distinguishes an
// invalid credential from a valid credential paired with a model/request
// error. The body is retained only for an explicit AuthError so callers can
// provide useful, redacted validation detail.
func authenticatedProbe(ctx context.Context, client *http.Client, endpoint string, body []byte, setHeaders func(*http.Request) error) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := setHeaders(req); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	if !isAuthenticationError(body) {
		return nil
	}
	return &HTTPError{Status: resp.Status, Body: string(bytes.TrimSpace(body))}
}

func isAuthenticationError(body []byte) bool {
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(envelope.Error.Type)) == probeAuthErrorType
}

func probeModel(modelID, fallback string) string {
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		return modelID
	}
	return fallback
}

// Protocol names the compiled wire adapter selected for a provider.
type Protocol string

const (
	// ProtocolOpenAIChatCompletions is the canonical protocol name used by
	// provider profiles.
	ProtocolOpenAIChatCompletions Protocol = "openai-chat-completions"

	// ProtocolOpenAICompletions is the legacy JSON config spelling retained
	// while existing provider entries are normalized in memory.
	ProtocolOpenAICompletions Protocol = "openai-completions"

	// ProtocolAnthropicMessages selects Anthropic's native Messages API.
	ProtocolAnthropicMessages Protocol = "anthropic-messages"

	// ProtocolOpenAIResponses selects OpenAI's Responses API. It is a separate
	// adapter because Responses uses output items and a flattened function-tool
	// shape rather than Chat Completions messages.
	ProtocolOpenAIResponses Protocol = "openai-responses"
)

// BackendConfig contains the validated runtime inputs needed to construct a
// compiled backend. Secret resolution belongs to the config layer; APIKey is
// already resolved by the time this factory is called.
type BackendConfig struct {
	Protocol   Protocol
	BaseURL    string
	APIKey     string
	HTTP       *http.Client
	MaxRetries int
	Headers    map[string]string
	AuthKind   string
	AuthHeader string
}

// NewBackend selects a compiled adapter for cfg.Protocol. The current JSONC
// provider format uses openai-completions; the canonical profile spelling is
// openai-chat-completions. Unknown protocols fail explicitly instead of being
// guessed as an OpenAI-compatible endpoint.
func NewBackend(cfg BackendConfig) (Backend, error) {
	switch cfg.Protocol {
	case "", ProtocolOpenAIChatCompletions, ProtocolOpenAICompletions:
		if cfg.BaseURL == "" {
			return nil, errors.New("llm: backend base url is required")
		}
		client := New(cfg.BaseURL, cfg.APIKey)
		if cfg.HTTP != nil {
			client.HTTP = cfg.HTTP
		}
		client.MaxRetries = cfg.MaxRetries
		client.Headers = maps.Clone(cfg.Headers)
		client.AuthKind = cfg.AuthKind
		client.AuthHeader = cfg.AuthHeader
		backend := NewOpenAIBackend(client)
		if cfg.Protocol != "" {
			backend.protocol = cfg.Protocol
		}
		return backend, nil
	case ProtocolAnthropicMessages:
		if cfg.BaseURL == "" {
			return nil, errors.New("llm: backend base url is required")
		}
		client := NewAnthropic(cfg.BaseURL, cfg.APIKey)
		if cfg.HTTP != nil {
			client.HTTP = cfg.HTTP
		}
		client.MaxRetries = cfg.MaxRetries
		if cfg.Headers != nil {
			client.Headers = maps.Clone(cfg.Headers)
		}
		client.AuthKind = cfg.AuthKind
		client.AuthHeader = cfg.AuthHeader
		backend := NewAnthropicBackend(client)
		if cfg.Protocol != "" {
			backend.protocol = cfg.Protocol
		}
		return backend, nil
	case ProtocolOpenAIResponses:
		if cfg.BaseURL == "" {
			return nil, errors.New("llm: backend base url is required")
		}
		client := NewOpenAIResponses(cfg.BaseURL, cfg.APIKey)
		if cfg.HTTP != nil {
			client.HTTP = cfg.HTTP
		}
		client.MaxRetries = cfg.MaxRetries
		client.Headers = maps.Clone(cfg.Headers)
		client.AuthKind = cfg.AuthKind
		client.AuthHeader = cfg.AuthHeader
		backend := NewOpenAIResponsesBackend(client)
		if cfg.Protocol != "" {
			backend.protocol = cfg.Protocol
		}
		return backend, nil
	default:
		return nil, fmt.Errorf("llm: protocol %q has no adapter", cfg.Protocol)
	}
}

// OpenAIBackend adapts the existing OpenAI-compatible chat-completions
// client to the provider-neutral Backend contract.
type OpenAIBackend struct {
	client   *Client
	protocol Protocol
}

// NewOpenAIBackend wraps an OpenAI-compatible client as a Backend.
func NewOpenAIBackend(client *Client) *OpenAIBackend {
	return &OpenAIBackend{client: client, protocol: ProtocolOpenAIChatCompletions}
}

// AdapterProtocol reports the compiled protocol used by this backend.
func (b *OpenAIBackend) AdapterProtocol() Protocol {
	if b == nil || b.protocol == "" {
		return ProtocolOpenAIChatCompletions
	}
	return b.protocol
}

// Stream implements Backend.
func (b *OpenAIBackend) Stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if b == nil || b.client == nil {
		return Message{}, Usage{}, errors.New("llm: nil openai backend")
	}
	return b.client.stream(ctx, req, sink)
}

// Complete implements Backend.
func (b *OpenAIBackend) Complete(ctx context.Context, req Request) (Message, Usage, error) {
	if b == nil || b.client == nil {
		return Message{}, Usage{}, errors.New("llm: nil openai backend")
	}
	return b.client.complete(ctx, req, EventSink{OnRetry: b.client.OnRetry})
}

// Models implements CatalogBackend.
func (b *OpenAIBackend) Models(ctx context.Context) ([]ModelInfo, error) {
	if b == nil || b.client == nil {
		return nil, errors.New("llm: nil openai backend")
	}
	return b.client.Models(ctx)
}

// Probe implements ProbeBackend using the OpenAI-compatible model endpoint.
// Catalog-less profiles use the result only as an auth check and do not cache
// the returned models.
func (b *OpenAIBackend) Probe(ctx context.Context, modelID string) error {
	if b == nil || b.client == nil {
		return errors.New("llm: nil openai backend")
	}
	return b.client.Probe(ctx, modelID)
}

var _ Backend = (*OpenAIBackend)(nil)
var _ CatalogBackend = (*OpenAIBackend)(nil)

// OpenAIResponsesBackend adapts OpenAI's Responses API to the provider-
// neutral Backend contract.
type OpenAIResponsesBackend struct {
	client   *OpenAIResponsesClient
	protocol Protocol
}

// NewOpenAIResponsesBackend wraps an OpenAI Responses client as a Backend.
func NewOpenAIResponsesBackend(client *OpenAIResponsesClient) *OpenAIResponsesBackend {
	return &OpenAIResponsesBackend{client: client, protocol: ProtocolOpenAIResponses}
}

// AdapterProtocol reports the compiled protocol used by this backend.
func (b *OpenAIResponsesBackend) AdapterProtocol() Protocol {
	if b == nil || b.protocol == "" {
		return ProtocolOpenAIResponses
	}
	return b.protocol
}

// Stream implements Backend.
func (b *OpenAIResponsesBackend) Stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if b == nil || b.client == nil {
		return Message{}, Usage{}, errors.New("llm: nil openai responses backend")
	}
	return b.client.stream(ctx, req, sink)
}

// Complete implements Backend.
func (b *OpenAIResponsesBackend) Complete(ctx context.Context, req Request) (Message, Usage, error) {
	if b == nil || b.client == nil {
		return Message{}, Usage{}, errors.New("llm: nil openai responses backend")
	}
	return b.client.complete(ctx, req, EventSink{OnRetry: b.client.OnRetry})
}

// Models implements CatalogBackend using the Responses provider's shared
// OpenAI-compatible model catalog endpoint.
func (b *OpenAIResponsesBackend) Models(ctx context.Context) ([]ModelInfo, error) {
	if b == nil || b.client == nil {
		return nil, errors.New("llm: nil openai responses backend")
	}
	return b.client.Models(ctx)
}

// Probe implements ProbeBackend using a bounded Responses request.
func (b *OpenAIResponsesBackend) Probe(ctx context.Context, modelID string) error {
	if b == nil || b.client == nil {
		return errors.New("llm: nil openai responses backend")
	}
	return b.client.Probe(ctx, modelID)
}

var _ Backend = (*OpenAIResponsesBackend)(nil)
var _ CatalogBackend = (*OpenAIResponsesBackend)(nil)

// AnthropicBackend adapts the native Anthropic Messages client to the
// provider-neutral Backend contract.
type AnthropicBackend struct {
	client   *AnthropicClient
	protocol Protocol
}

// NewAnthropicBackend wraps an Anthropic Messages client as a Backend.
func NewAnthropicBackend(client *AnthropicClient) *AnthropicBackend {
	return &AnthropicBackend{client: client, protocol: ProtocolAnthropicMessages}
}

// AdapterProtocol reports the compiled protocol used by this backend.
func (b *AnthropicBackend) AdapterProtocol() Protocol {
	if b == nil || b.protocol == "" {
		return ProtocolAnthropicMessages
	}
	return b.protocol
}

// Stream implements Backend.
func (b *AnthropicBackend) Stream(ctx context.Context, req Request, sink EventSink) (Message, Usage, error) {
	if b == nil || b.client == nil {
		return Message{}, Usage{}, errors.New("llm: nil anthropic backend")
	}
	return b.client.stream(ctx, req, sink)
}

// Complete implements Backend.
func (b *AnthropicBackend) Complete(ctx context.Context, req Request) (Message, Usage, error) {
	if b == nil || b.client == nil {
		return Message{}, Usage{}, errors.New("llm: nil anthropic backend")
	}
	return b.client.complete(ctx, req, EventSink{OnRetry: b.client.OnRetry})
}

// Models implements CatalogBackend.
func (b *AnthropicBackend) Models(ctx context.Context) ([]ModelInfo, error) {
	if b == nil || b.client == nil {
		return nil, errors.New("llm: nil anthropic backend")
	}
	return b.client.Models(ctx)
}

// Probe implements ProbeBackend using Anthropic's authenticated model
// endpoint. Catalog-less profiles use the result only as an auth check and do
// not cache the returned models.
func (b *AnthropicBackend) Probe(ctx context.Context, modelID string) error {
	if b == nil || b.client == nil {
		return errors.New("llm: nil anthropic backend")
	}
	return b.client.Probe(ctx, modelID)
}

var _ Backend = (*AnthropicBackend)(nil)
var _ CatalogBackend = (*AnthropicBackend)(nil)
