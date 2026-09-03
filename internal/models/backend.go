package models

import (
	"context"
	"errors"
	"fmt"
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

// Backend is the execution contract consumed by the agent.
type Backend interface {
	Stream(context.Context, Request, EventSink) (Message, Usage, error)
	Complete(context.Context, Request) (Message, Usage, error)
}

// ProtocolBackend reports the compiled wire protocol used by a backend.
type ProtocolBackend interface {
	Backend
	AdapterProtocol() Protocol
}

// CatalogBackend is implemented by providers that expose model discovery.
type CatalogBackend interface {
	Models(context.Context) ([]ModelInfo, error)
}

// ProbeBackend is an optional authenticated health check.
type ProbeBackend interface {
	Probe(context.Context, string) error
}

const (
	authProbeModel      = "gpt-4o-mini"
	anthropicProbeModel = "claude-3-5-haiku-latest"
	probeAuthErrorType  = "autherror"
)

func probeModel(modelID, fallback string) string {
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		return modelID
	}
	return fallback
}

// Protocol names the compiled wire adapter selected for a provider.
type Protocol string

const (
	ProtocolOpenAIChatCompletions Protocol = "openai-chat-completions"
	ProtocolOpenAICompletions     Protocol = "openai-completions"
	ProtocolAnthropicMessages     Protocol = "anthropic-messages"
	ProtocolOpenAIResponses       Protocol = "openai-responses"
)

// BackendOptions contains secret and transport inputs for a resolved profile.
// Profile behavior stays in Resolved; credentials stay at this boundary.
type BackendOptions struct {
	APIKey           string
	ProtocolOverride Protocol
	HTTP             *http.Client
	MaxRetries       int
	Authorizer       RequestAuthorizer
}

// NewBackend selects the adapter for a resolved profile. The legacy
// openai-completions spelling is normalized before selection.
func NewBackend(resolved Resolved, opts BackendOptions) (Backend, error) {
	protocol := normalizeProtocol(resolved.Protocol)
	if opts.ProtocolOverride != "" {
		protocol = normalizeProtocol(opts.ProtocolOverride)
	}
	if strings.TrimSpace(resolved.BaseURL) == "" {
		return nil, errors.New("models: backend base url is required")
	}

	switch protocol {
	case "", ProtocolOpenAIChatCompletions:
		client := newClient(resolved.BaseURL, opts.APIKey)
		if opts.HTTP != nil {
			client.HTTP = opts.HTTP
		}
		client.MaxRetries = opts.MaxRetries
		client.Headers = maps.Clone(resolved.DefaultHeaders)
		client.AuthKind = resolved.Auth.Kind
		client.AuthHeader = resolved.Auth.Header
		return client, nil
	case ProtocolAnthropicMessages:
		client := newAnthropicClient(resolved.BaseURL, opts.APIKey)
		if opts.HTTP != nil {
			client.HTTP = opts.HTTP
		}
		client.MaxRetries = opts.MaxRetries
		if resolved.DefaultHeaders != nil {
			client.Headers = maps.Clone(resolved.DefaultHeaders)
		}
		client.AuthKind = resolved.Auth.Kind
		client.AuthHeader = resolved.Auth.Header
		return client, nil
	case ProtocolOpenAIResponses:
		client := newOpenAIResponses(resolved.BaseURL, opts.APIKey)
		if opts.HTTP != nil {
			client.HTTP = opts.HTTP
		}
		client.MaxRetries = opts.MaxRetries
		client.Headers = maps.Clone(resolved.DefaultHeaders)
		client.AuthKind = resolved.Auth.Kind
		client.AuthHeader = resolved.Auth.Header
		client.Authorizer = opts.Authorizer
		if resolved.Auth.Kind == AuthCodexSubscription {
			client.flavor = responsesCodexSubscription
		}
		return client, nil
	default:
		return nil, fmt.Errorf("models: protocol %q has no adapter", protocol)
	}
}
