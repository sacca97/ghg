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
	"github.com/sacca97/ghg/internal/models"
)

// ValidationTimeout bounds the authenticated request made by an auth flow.
const ValidationTimeout = 15 * time.Second

// Result is the validated profile and the optional model response used to
// seed its catalog. Key is intentionally not retained here: callers own the
// short-lived key they received from the masked/CLI input boundary.
type Result struct {
	Name              string
	Profile           models.Resolved
	Models            []models.ModelInfo
	Validated         bool
	NeedsConfirmation bool
	// CatalogErr is a non-fatal failure fetching an optional public catalog
	// after the credential itself was validated by ProbeBackend.
	CatalogErr error
}

// ResolveProfile resolves an auth command's profile id and includes the
// available IDs in an unknown-id error so the user can correct the command.
func ResolveProfile(profiles models.Profiles, id string) (models.Resolved, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return models.Resolved{}, fmt.Errorf("provider profile id is required (available: %s)", availableProfiles(profiles))
	}
	if _, ok := profiles.Lookup(id); !ok {
		return models.Resolved{}, fmt.Errorf("unknown provider %q (available: %s)", id, availableProfiles(profiles))
	}
	return profiles.Resolve(models.Instance{Name: id, Profile: id})
}

// Authenticate validates key against the selected profile. Catalog-enabled
// profiles use their catalog capability and return that same response for
// cache seeding. Catalog-less profiles use ProbeBackend and return no models.
// A backend with neither capability returns NeedsConfirmation instead of
// silently claiming the credential works.
func Authenticate(ctx context.Context, profiles models.Profiles, id, key string, maxRetries int) (Result, error) {
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

	backend, err := NewBackend(resolved, key, "", maxRetries)
	if err != nil {
		return result, fmt.Errorf("provider %q: %w", resolved.Name, err)
	}

	validationCtx, cancel := context.WithTimeout(ctx, ValidationTimeout)
	defer cancel()
	if resolved.Catalog.Public {
		probe, ok := backend.(models.ProbeBackend)
		if !ok {
			result.NeedsConfirmation = true
			return result, nil
		}
		var modelInfos []models.ModelInfo
		var catalogErr error
		if catalog, ok := backend.(models.CatalogBackend); ok && catalogEnabled(resolved.Catalog.Kind) {
			// Public catalogs do not authenticate. Fetch one before probing so
			// the probe can use a real model ID instead of a sentinel that some
			// providers reject as an authentication failure.
			modelInfos, catalogErr = catalog.Models(validationCtx)
		}
		probeModel := ""
		if len(modelInfos) > 0 {
			probe, probeModel, err = routedProbe(profiles, resolved, key, maxRetries, modelInfos)
			if err != nil {
				return result, newValidationError(resolved.Name, key, err)
			}
		}
		if err := probe.Probe(validationCtx, probeModel); err != nil {
			return result, newValidationError(resolved.Name, key, err)
		}
		result.Validated = true
		result.Models = modelInfos
		if catalogErr != nil {
			result.CatalogErr = newValidationError(resolved.Name, key, catalogErr)
		}
		return result, nil
	}
	if catalogEnabled(resolved.Catalog.Kind) {
		if catalog, ok := backend.(models.CatalogBackend); ok {
			models, err := catalog.Models(validationCtx)
			if err != nil {
				return result, newValidationError(resolved.Name, key, err)
			}
			result.Models = models
			result.Validated = true
			return result, nil
		}
	}
	if probe, ok := backend.(models.ProbeBackend); ok {
		if err := probe.Probe(validationCtx, ""); err != nil {
			return result, newValidationError(resolved.Name, key, err)
		}
		result.Validated = true
		return result, nil
	}
	result.NeedsConfirmation = true
	return result, nil
}

// routedProbe selects the first advertised model whose profile route has a
// compiled probe-capable adapter. Public catalogs can contain models for a
// protocol ghg does not support yet (for example OpenAI Responses), so those
// entries must not prevent auth from validating against a supported sibling.
func routedProbe(profiles models.Profiles, resolved models.Resolved, key string, maxRetries int, modelInfos []models.ModelInfo) (models.ProbeBackend, string, error) {
	var lastErr error
	hadModel := false
	for _, model := range modelInfos {
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		hadModel = true
		probeResolved, err := profiles.ResolveModel(models.Instance{
			Name:     resolved.Name,
			Profile:  resolved.Profile.ID,
			BaseURL:  resolved.BaseURL,
			Protocol: resolved.Protocol,
		}, modelID)
		if err != nil {
			return nil, "", err
		}
		probeBackend, err := NewBackend(probeResolved, key, "", maxRetries)
		if err != nil {
			lastErr = err
			continue
		}
		probe, ok := probeBackend.(models.ProbeBackend)
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
func KeyHint(resolved models.Resolved) string {
	if resolved.Docs.KeysURL != "" {
		return "get one at " + resolved.Docs.KeysURL
	}
	return "configure credentials for " + resolved.Profile.DisplayName
}

func availableProfiles(profiles models.Profiles) string {
	ids := profiles.IDs()
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

func catalogEnabled(kind string) bool {
	return kind == models.CatalogOpenAIModels || kind == models.CatalogAnthropicModels
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
