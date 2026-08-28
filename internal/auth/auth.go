// Package auth contains the shared provider-profile authentication flow used
// by the CLI and TUI.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/provider"
)

// ValidationTimeout bounds the authenticated request made by an auth flow.
const ValidationTimeout = 15 * time.Second

// Result is the validated profile and the optional model response used to
// seed its catalog. Key is intentionally not retained here: callers own the
// short-lived key they received from the masked/CLI input boundary.
type Result struct {
	Name              string
	Profile           provider.Resolved
	Models            []llm.ModelInfo
	Validated         bool
	NeedsConfirmation bool
	// CatalogErr is a non-fatal failure fetching an optional public catalog
	// after the credential itself was validated by ProbeBackend.
	CatalogErr error
}

// ResolveProfile resolves an auth command's profile id and includes the
// available IDs in an unknown-id error so the user can correct the command.
func ResolveProfile(profiles provider.Profiles, id string) (provider.Resolved, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return provider.Resolved{}, fmt.Errorf("provider profile id is required (available: %s)", availableProfiles(profiles))
	}
	if _, ok := profiles.Lookup(id); !ok {
		return provider.Resolved{}, fmt.Errorf("unknown provider %q (available: %s)", id, availableProfiles(profiles))
	}
	return profiles.Resolve(provider.Instance{Name: id, Profile: id})
}

// Authenticate validates key against the selected profile. Catalog-enabled
// profiles use their catalog capability and return that same response for
// cache seeding. Catalog-less profiles use ProbeBackend and return no models.
// A backend with neither capability returns NeedsConfirmation instead of
// silently claiming the credential works.
func Authenticate(ctx context.Context, profiles provider.Profiles, id, key string, maxRetries int) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolved, err := ResolveProfile(profiles, id)
	if err != nil {
		return Result{Name: strings.TrimSpace(id)}, err
	}
	result := Result{Name: resolved.Name, Profile: resolved}
	if !resolved.RequiresAPIKey() {
		return result, fmt.Errorf("provider %q takes no API key", resolved.Name)
	}
	key = config.TrimKey(key)
	if key == "" {
		return result, fmt.Errorf("provider %q needs an API key (%s)", resolved.Name, KeyHint(resolved))
	}

	backend, err := newBackend(resolved, key, maxRetries)
	if err != nil {
		return result, fmt.Errorf("provider %q: %w", resolved.Name, err)
	}

	validationCtx, cancel := context.WithTimeout(ctx, ValidationTimeout)
	defer cancel()
	if resolved.Catalog.Public {
		probe, ok := backend.(llm.ProbeBackend)
		if !ok {
			result.NeedsConfirmation = true
			return result, nil
		}
		var models []llm.ModelInfo
		var catalogErr error
		if catalog, ok := backend.(llm.CatalogBackend); ok && catalogEnabled(resolved.Catalog.Kind) {
			// Public catalogs do not authenticate. Fetch one before probing so
			// the probe can use a real model ID instead of a sentinel that some
			// providers reject as an authentication failure.
			models, catalogErr = catalog.Models(validationCtx)
		}
		probeModel := ""
		if len(models) > 0 {
			probe, probeModel, err = routedProbe(profiles, resolved, key, maxRetries, models)
			if err != nil {
				return result, newValidationError(resolved.Name, key, err)
			}
		}
		if err := probe.Probe(validationCtx, probeModel); err != nil {
			return result, newValidationError(resolved.Name, key, err)
		}
		result.Validated = true
		result.Models = models
		if catalogErr != nil {
			result.CatalogErr = newValidationError(resolved.Name, key, catalogErr)
		}
		return result, nil
	}
	if catalogEnabled(resolved.Catalog.Kind) {
		if catalog, ok := backend.(llm.CatalogBackend); ok {
			models, err := catalog.Models(validationCtx)
			if err != nil {
				return result, newValidationError(resolved.Name, key, err)
			}
			result.Models = models
			result.Validated = true
			return result, nil
		}
	}
	if probe, ok := backend.(llm.ProbeBackend); ok {
		if err := probe.Probe(validationCtx, ""); err != nil {
			return result, newValidationError(resolved.Name, key, err)
		}
		result.Validated = true
		return result, nil
	}
	result.NeedsConfirmation = true
	return result, nil
}

func newBackend(resolved provider.Resolved, key string, maxRetries int) (llm.Backend, error) {
	return llm.NewBackend(llm.BackendConfig{
		Protocol:   llm.Protocol(resolved.Protocol),
		BaseURL:    resolved.BaseURL,
		APIKey:     key,
		Headers:    resolved.DefaultHeaders,
		AuthKind:   resolved.Auth.Kind,
		AuthHeader: resolved.Auth.Header,
		MaxRetries: maxRetries,
	})
}

// routedProbe selects the first advertised model whose profile route has a
// compiled probe-capable adapter. Public catalogs can contain models for a
// protocol ghg does not support yet (for example OpenAI Responses), so those
// entries must not prevent auth from validating against a supported sibling.
func routedProbe(profiles provider.Profiles, resolved provider.Resolved, key string, maxRetries int, models []llm.ModelInfo) (llm.ProbeBackend, string, error) {
	var lastErr error
	hadModel := false
	for _, model := range models {
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		hadModel = true
		probeResolved, err := profiles.ResolveModel(provider.Instance{
			Name:     resolved.Name,
			Profile:  resolved.Profile.ID,
			BaseURL:  resolved.BaseURL,
			Protocol: resolved.Protocol,
		}, modelID)
		if err != nil {
			return nil, "", err
		}
		probeBackend, err := newBackend(probeResolved, key, maxRetries)
		if err != nil {
			lastErr = err
			continue
		}
		probe, ok := probeBackend.(llm.ProbeBackend)
		if !ok {
			lastErr = fmt.Errorf("protocol %q has no authentication probe", probeResolved.Protocol)
			continue
		}
		return probe, modelID, nil
	}
	if !hadModel {
		return nil, "", errors.New("public catalog contains no model id for authentication probe")
	}
	if lastErr == nil {
		lastErr = errors.New("public catalog contains no supported authentication probe")
	}
	return nil, "", lastErr
}

// KeyHint returns a safe setup hint that contains no credential material.
func KeyHint(resolved provider.Resolved) string {
	if resolved.Docs.KeysURL != "" {
		return "get one at " + resolved.Docs.KeysURL
	}
	return "configure credentials for " + resolved.Profile.DisplayName
}

func availableProfiles(profiles provider.Profiles) string {
	ids := profiles.IDs()
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

func catalogEnabled(kind string) bool {
	return kind == provider.CatalogOpenAIModels || kind == provider.CatalogAnthropicModels
}

type validationError struct {
	provider string
	err      error
	detail   string
}

func newValidationError(name, key string, err error) error {
	return &validationError{provider: name, err: err, detail: redact(err.Error(), key)}
}

func (e *validationError) Error() string {
	return fmt.Sprintf("provider %q validation failed: %s", e.provider, e.detail)
}

func (e *validationError) Unwrap() error { return e.err }

func redact(value, secret string) string {
	if secret == "" {
		return value
	}
	value = strings.ReplaceAll(value, "Bearer "+secret, "[redacted]")
	return strings.ReplaceAll(value, secret, "[redacted]")
}
