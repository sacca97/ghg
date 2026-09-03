package config

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
)

// CatalogBackendFactory builds the protocol adapter used for model discovery.
// The optional factory lets auth supply OAuth-aware adapters without making
// config depend on auth.
type CatalogBackendFactory func(models.Resolved, string, string, int) (models.Backend, error)

// FetchCatalogs refreshes configured provider catalogs and the metadata needed
// to enrich their selected models.
func FetchCatalogs(ctx context.Context, cfg *Config, profiles models.Profiles, force bool, factories ...CatalogBackendFactory) (map[string]Catalog, error) {
	cats := LoadCatalogs()
	if cfg == nil {
		return cats, fmt.Errorf("catalog fetch: config is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	factory := CatalogBackendFactory(func(resolved models.Resolved, key, api string, maxRetries int) (models.Backend, error) {
		return models.NewBackend(resolved, models.BackendOptions{APIKey: key, MaxRetries: maxRetries, ProtocolOverride: models.Protocol(api)})
	})
	if len(factories) > 0 && factories[0] != nil {
		factory = factories[0]
	}

	dirty := false
	for name, provider := range cfg.Providers {
		if err := ctx.Err(); err != nil {
			return cats, err
		}
		resolved, err := profiles.Resolve(ProviderInstance(name, provider))
		if err != nil {
			LogEvent("catalog.fetch", name+" skipped: "+err.Error())
			continue
		}
		if resolved.Catalog.Kind != models.CatalogOpenAIModels && resolved.Catalog.Kind != models.CatalogAnthropicModels {
			continue
		}
		if cached, ok := cats[name]; ok && !force && !cached.Stale() && cached.BaseURL == resolved.BaseURL {
			continue
		}

		key := ""
		if resolved.RequiresAPIKey() {
			key, err = provider.ResolveKey()
			if err != nil || key == "" {
				continue
			}
		}
		maxRetries := cfg.MaxRetries
		backend, err := factory(resolved, key, "", maxRetries)
		if err != nil {
			continue
		}
		catalog, ok := backend.(models.CatalogBackend)
		if !ok {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		infos, fetchErr := catalog.Models(requestCtx)
		cancel()
		if fetchErr != nil {
			continue
		}
		cats[name] = Catalog{FetchedAt: time.Now(), BaseURL: resolved.BaseURL, Models: ModelInfoLites(infos)}
		dirty = true
	}

	metadata := LoadModelsDev()
	wanted := cfg.CatalogWantedModels(cats)
	needsMetadata := force || metadata.Stale() || metadata.Version < modelsDevCacheVersion
	if !needsMetadata {
		for id := range wanted {
			if !metadata.HasModel(id) {
				needsMetadata = true
				break
			}
		}
	}
	if len(wanted) > 0 && needsMetadata {
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		fresh, fetchErr := FetchModelsDev(requestCtx, wanted)
		cancel()
		if fetchErr == nil {
			metadata = fresh
			_ = SaveModelsDev(metadata)
		}
	}
	for name, catalog := range cats {
		enriched, changed := EnrichCatalogMetadata(catalog, metadata, ModelsDevProviderIDs(profiles, name, cfg.Providers[name]))
		if changed {
			cats[name] = enriched
			dirty = true
		}
	}
	if dirty {
		if err := SaveCatalogs(cats); err != nil {
			return cats, err
		}
	}
	return cats, nil
}

// ProviderInstance converts a JSONC provider into the profile resolver input.
func ProviderInstance(name string, provider Provider) models.Instance {
	return models.Instance{Name: name, Profile: provider.Profile, BaseURL: provider.BaseURL, Protocol: models.Protocol(provider.API)}
}

// ModelsDevProviderIDs returns the exact provider IDs to try for one instance.
func ModelsDevProviderIDs(profiles models.Profiles, name string, provider Provider) []string {
	ids := []string{name}
	resolved, err := profiles.Resolve(ProviderInstance(name, provider))
	if err != nil {
		return ids
	}
	ids = ids[:0]
	for _, id := range []string{resolved.Catalog.ModelsDev, resolved.Profile.ID, name} {
		id = strings.TrimSpace(id)
		if id == "" || containsString(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
