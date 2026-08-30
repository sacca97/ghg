package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	modelsDevAPIURL = "https://models.opencode.ai/api.json"
	// Bump when the cache shape or matching rules change so an existing cache
	// is rebuilt with the exact provider/model IDs from models.dev.
	modelsDevCacheVersion = 5
	modelsDevCacheTTL     = 24 * time.Hour
	// The current models.dev catalog is small, but keep a hard ceiling so a
	// broken endpoint cannot make a TUI startup retain an unbounded response.
	modelsDevMaxBytes = 16 << 20
)

// ModelsDevReasoning is the caller-controlled reasoning surface advertised by
// models.dev for one provider/model pair. Efforts contains the values of
// reasoning_options[type=effort]. Toggle is true when the model exposes a
// separate binary reasoning switch. An entry with neither field is still
// meaningful: models.dev knew about the model but did not advertise a
// caller-controlled effort.
type ModelsDevReasoning struct {
	Efforts []string `json:"efforts,omitempty"`
	Toggle  bool     `json:"toggle,omitempty"`
}

// ModelsDevCache contains the model metadata fetched from models.dev. The
// complete upstream catalog is intentionally not retained: ghg only needs
// this provider/model mapping for the status bar, compaction, and reasoning
// effort picker.
type ModelsDevCache struct {
	Version   int                                      `json:"version,omitempty"`
	FetchedAt time.Time                                `json:"fetchedAt"`
	Providers map[string]map[string]int                `json:"providers"`
	Reasoning map[string]map[string]ModelsDevReasoning `json:"reasoning,omitempty"`
}

// Stale reports whether the metadata should be refreshed.
func (c ModelsDevCache) Stale() bool {
	return c.FetchedAt.IsZero() || time.Since(c.FetchedAt) > modelsDevCacheTTL
}

// ContextLength returns the context window for modelID. Exact provider IDs
// win; when none matches, a model is accepted only when every models.dev hit
// agrees on the same value. That keeps a shared model ID from inheriting a
// different provider's limit by accident.
func (c ModelsDevCache) ContextLength(modelID string, providerIDs ...string) int {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return 0
	}
	seen := make(map[string]struct{}, len(providerIDs))
	for _, providerID := range providerIDs {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" {
			continue
		}
		if _, ok := seen[providerID]; ok {
			continue
		}
		seen[providerID] = struct{}{}
		if n := c.Providers[providerID][modelID]; n > 0 {
			return n
		}
	}

	// A configured provider may be a private gateway that is not represented
	// by the public provider IDs. A unique model record is still useful in
	// that case; conflicting records are deliberately treated as unknown.
	found := 0
	for _, models := range c.Providers {
		n := models[modelID]
		if n <= 0 {
			continue
		}
		if found != 0 && found != n {
			return 0
		}
		found = n
	}
	return found
}

// ReasoningFor returns the reasoning metadata for modelID. Exact provider IDs
// win; when none matches, a global fallback is accepted only when every
// models.dev record agrees. Conflicting provider records are treated as
// unknown because the same model ID can expose different controls on each
// host.
func (c ModelsDevCache) ReasoningFor(modelID string, providerIDs ...string) (ModelsDevReasoning, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ModelsDevReasoning{}, false
	}
	seen := make(map[string]struct{}, len(providerIDs))
	for _, providerID := range providerIDs {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" {
			continue
		}
		if _, ok := seen[providerID]; ok {
			continue
		}
		seen[providerID] = struct{}{}
		if info, ok := c.Reasoning[providerID][modelID]; ok {
			return cloneModelsDevReasoning(info), true
		}
	}

	var found ModelsDevReasoning
	have := false
	for _, models := range c.Reasoning {
		info, ok := models[modelID]
		if !ok {
			continue
		}
		if have && !sameModelsDevReasoning(found, info) {
			return ModelsDevReasoning{}, false
		}
		found = info
		have = true
	}
	if !have {
		return ModelsDevReasoning{}, false
	}
	return cloneModelsDevReasoning(found), true
}

// HasModel reports whether the normalized cache contains any metadata for a
// model ID. It lets callers fetch a newly selected model even when the daily
// cache itself is still fresh.
func (c ModelsDevCache) HasModel(modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, models := range c.Providers {
		if _, ok := models[modelID]; ok {
			return true
		}
	}
	for _, models := range c.Reasoning {
		if _, ok := models[modelID]; ok {
			return true
		}
	}
	return false
}

func cloneModelsDevReasoning(info ModelsDevReasoning) ModelsDevReasoning {
	info.Efforts = append([]string(nil), info.Efforts...)
	return info
}

func sameModelsDevReasoning(a, b ModelsDevReasoning) bool {
	return a.Toggle == b.Toggle && slices.Equal(a.Efforts, b.Efforts)
}

func modelsDevPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models-dev.json"), nil
}

// LoadModelsDev reads the local models.dev metadata cache. A missing or
// malformed cache is harmless and returns an empty cache; callers can use a
// stale cache immediately and refresh it in the background.
func LoadModelsDev() ModelsDevCache {
	cache := ModelsDevCache{
		Providers: map[string]map[string]int{},
		Reasoning: map[string]map[string]ModelsDevReasoning{},
	}
	p, err := modelsDevPath()
	if err != nil {
		return cache
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return cache
	}
	if json.Unmarshal(data, &cache) != nil || cache.Providers == nil {
		return ModelsDevCache{
			Providers: map[string]map[string]int{},
			Reasoning: map[string]map[string]ModelsDevReasoning{},
		}
	}
	if cache.Reasoning == nil {
		cache.Reasoning = map[string]map[string]ModelsDevReasoning{}
	}
	return cache
}

// SaveModelsDev writes the normalized models.dev context cache.
func SaveModelsDev(cache ModelsDevCache) error {
	p, err := modelsDevPath()
	if err != nil {
		return err
	}
	if cache.Providers == nil {
		cache.Providers = map[string]map[string]int{}
	}
	if cache.Reasoning == nil {
		cache.Reasoning = map[string]map[string]ModelsDevReasoning{}
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o600)
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID    string `json:"id"`
	Limit struct {
		Context int `json:"context"`
	} `json:"limit"`
	ReasoningOptions *[]modelsDevReasoningOption `json:"reasoning_options"`
}

type modelsDevReasoningOption struct {
	Type   string    `json:"type"`
	Values []*string `json:"values"`
}

// parseModelsDev retains only wanted models from the public models.dev
// /api.json shape. The object key is the canonical model ID in the API, but
// accepting the embedded id too keeps this tolerant of generated snapshots.
func parseModelsDev(data []byte, wanted map[string]struct{}) (ModelsDevCache, error) {
	var payload map[string]modelsDevProvider
	if err := json.Unmarshal(data, &payload); err != nil {
		return ModelsDevCache{}, fmt.Errorf("parse models.dev catalog: %w", err)
	}
	cache := ModelsDevCache{
		Version:   modelsDevCacheVersion,
		FetchedAt: time.Now(),
		Providers: make(map[string]map[string]int),
		Reasoning: make(map[string]map[string]ModelsDevReasoning),
	}
	for providerID, provider := range payload {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" {
			continue
		}
		for modelID, model := range provider.Models {
			modelID = strings.TrimSpace(modelID)
			modelAPIID := strings.TrimSpace(model.ID)
			ids := wantedModelIDs(wanted, modelID, modelAPIID)
			if len(ids) == 0 {
				continue
			}
			for _, id := range ids {
				if model.Limit.Context > 0 {
					if cache.Providers[providerID] == nil {
						cache.Providers[providerID] = map[string]int{}
					}
					cache.Providers[providerID][id] = model.Limit.Context
				}
				if cache.Reasoning[providerID] == nil {
					cache.Reasoning[providerID] = map[string]ModelsDevReasoning{}
				}
				if model.ReasoningOptions != nil {
					cache.Reasoning[providerID][id] = normalizeModelsDevReasoning(*model.ReasoningOptions)
				} else {
					// An omitted reasoning_options field is a known model record,
					// not an unknown capability. Keep an empty entry so the TUI
					// exposes off only instead of inventing default levels.
					cache.Reasoning[providerID][id] = ModelsDevReasoning{}
				}
			}
		}
	}
	return cache, nil
}

func wantedModelIDs(wanted map[string]struct{}, ids ...string) []string {
	if len(wanted) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := wanted[id]; !ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func normalizeModelsDevReasoning(options []modelsDevReasoningOption) ModelsDevReasoning {
	var out ModelsDevReasoning
	for _, option := range options {
		switch strings.ToLower(strings.TrimSpace(option.Type)) {
		case "toggle":
			out.Toggle = true
		case "effort":
			for _, value := range option.Values {
				effort := "none"
				if value != nil {
					effort = strings.ToLower(strings.TrimSpace(*value))
					if effort == "" {
						effort = "none"
					}
				}
				if !slices.Contains(out.Efforts, effort) {
					out.Efforts = append(out.Efforts, effort)
				}
			}
		}
	}
	return out
}

// FetchModelsDev retrieves and normalizes the wanted models from the public
// models.dev catalog. The caller owns the timeout through ctx; the TUI uses a
// ten-second deadline.
func FetchModelsDev(ctx context.Context, wanted map[string]struct{}) (ModelsDevCache, error) {
	return fetchModelsDev(ctx, http.DefaultClient, modelsDevAPIURL, wanted)
}

func fetchModelsDev(ctx context.Context, client *http.Client, endpoint string, wanted map[string]struct{}) (ModelsDevCache, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ModelsDevCache{}, fmt.Errorf("models.dev: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ghg/models-dev")
	resp, err := client.Do(req)
	if err != nil {
		return ModelsDevCache{}, fmt.Errorf("models.dev: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ModelsDevCache{}, fmt.Errorf("models.dev: unexpected HTTP status %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBytes+1))
	if err != nil {
		return ModelsDevCache{}, fmt.Errorf("models.dev: read response: %w", err)
	}
	if len(data) > modelsDevMaxBytes {
		return ModelsDevCache{}, fmt.Errorf("models.dev: response exceeds %d bytes", modelsDevMaxBytes)
	}
	return parseModelsDev(data, wanted)
}
