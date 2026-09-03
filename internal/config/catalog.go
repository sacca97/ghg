package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
)

// catalogTTL is how long a provider's fetched model list stays fresh.
const catalogTTL = 24 * time.Hour

// Catalog is the cached model list of one models.
type Catalog struct {
	FetchedAt time.Time       `json:"fetchedAt"`
	BaseURL   string          `json:"baseUrl"`
	Models    []ModelInfoLite `json:"models"`
}

// ModelInfoLite is the subset of the provider's /models entry ghg uses.
type ModelInfoLite struct {
	ID                  string   `json:"id"`
	ContextLength       int      `json:"contextLength,omitempty"`       // model's context window (input), 0 if unadvertised
	MaxCompletionTokens int      `json:"maxCompletionTokens,omitempty"` // provider's output cap, 0 if unadvertised
	ReasoningEfforts    []string `json:"reasoningEfforts,omitempty"`
	ReasoningKnown      bool     `json:"reasoningKnown,omitempty"`  // models.dev/provider explicitly described the reasoning surface
	ReasoningToggle     bool     `json:"reasoningToggle,omitempty"` // a separate on/off reasoning control is available
	InPrice             float64  `json:"inPrice,omitempty"`         // USD per prompt token, 0 if unadvertised
	OutPrice            float64  `json:"outPrice,omitempty"`        // USD per completion token, 0 if unadvertised
	CacheReadPrice      float64  `json:"cacheReadPrice,omitempty"`  // USD per cached prompt token, 0 = bill at InPrice
	InputModalities     []string `json:"inputModalities,omitempty"` // provider-advertised input types (["text","image"])
}

// SupportsVision reports whether the catalog advertises image input for a model
// id. The bool is tri-state: found==false means the catalog has no entry or the
// entry doesn't advertise modalities, so the caller falls back to config.
func (c Catalog) SupportsVision(id string) (vision, found bool) {
	mi := c.Find(id)
	if mi == nil || len(mi.InputModalities) == 0 {
		return false, false
	}
	return slices.Contains(mi.InputModalities, "image"), true
}

// ContextLength reports the advertised context window for a model id
// (0 when the catalog has no entry for it — callers must fall back).
func (c Catalog) ContextLength(id string) int {
	if mi := c.Find(id); mi != nil {
		return mi.ContextLength
	}
	return 0
}

// MaxCompletionTokens reports the advertised output-token cap for a model id
// (0 when unknown — callers must fall back to the configured context).
func (c Catalog) MaxCompletionTokens(id string) int {
	if mi := c.Find(id); mi != nil {
		return mi.MaxCompletionTokens
	}
	return 0
}

// Pricing reports the advertised per-token USD rates for a model id; ok is
// false when the catalog has no entry for it or the entry has no prices, in
// which case callers should hide cost rather than show $0.
func (c Catalog) Pricing(id string) (in, out, cacheRead float64, ok bool) {
	if mi := c.Find(id); mi != nil {
		return mi.InPrice, mi.OutPrice, mi.CacheReadPrice, mi.InPrice > 0 || mi.OutPrice > 0
	}
	return 0, 0, 0, false
}

// Find returns the catalog entry for a model id (nil when unadvertised).
func (c Catalog) Find(id string) *ModelInfoLite {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}

func catalogPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models.json"), nil
}

// LoadCatalogs reads ~/.ghg/models.json. A missing or unreadable file is
// not an error and yields an empty (non-nil) map, so callers can always write
// into the result.
func LoadCatalogs() map[string]Catalog {
	cats := map[string]Catalog{}
	p, err := catalogPath()
	if err != nil {
		return cats
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return cats
	}
	if json.Unmarshal(data, &cats) != nil || cats == nil {
		return map[string]Catalog{}
	}
	return cats
}

// SaveCatalogs writes ~/.ghg/models.json.
func SaveCatalogs(cats map[string]Catalog) error {
	p, err := catalogPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cats, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o600)
}

// ModelInfoLites converts provider model records into the catalog-cache shape.
// Keeping this conversion beside the cache prevents each onboarding surface
// from drifting when a capability field is added.
func ModelInfoLites(infos []models.ModelInfo) []ModelInfoLite {
	lites := make([]ModelInfoLite, len(infos))
	for i, mi := range infos {
		lites[i] = ModelInfoLite{
			ID:                  mi.ID,
			ContextLength:       mi.ContextLength,
			MaxCompletionTokens: mi.MaxCompletionTokens,
			ReasoningEfforts:    mi.ReasoningEfforts,
			ReasoningKnown:      len(mi.ReasoningEfforts) > 0,
			InputModalities:     mi.InputModalities,
		}
		if mi.Pricing != nil {
			lites[i].InPrice, lites[i].OutPrice, lites[i].CacheReadPrice = mi.Pricing.Rates()
		}
	}
	return lites
}

// SaveCatalog merges one provider's freshly validated model list into the
// catalog cache. A successful auth flow uses the same response for validation
// and seeding, so it never needs a second discovery request.
func SaveCatalog(name, baseURL string, infos []models.ModelInfo) error {
	cats := LoadCatalogs()
	cats[name] = Catalog{FetchedAt: time.Now(), BaseURL: baseURL, Models: ModelInfoLites(infos)}
	return SaveCatalogs(cats)
}

// Stale reports whether the cached catalog should be refetched.
func (c Catalog) Stale() bool { return time.Since(c.FetchedAt) > catalogTTL }

// resolveFromCatalog synthesizes a Model for an id advertised in a provider's
// cached /models catalog but absent from cfg.Models.
func (c *Config) resolveFromCatalog(model, provider string) (Model, string, error) {
	type hit struct {
		prov string
		mi   *ModelInfoLite
	}
	var hits []hit
	for name, cat := range LoadCatalogs() {
		if provider != "" && name != provider {
			continue
		}
		if _, ok := c.Providers[name]; !ok {
			continue
		}
		if mi := cat.Find(model); mi != nil {
			hits = append(hits, hit{name, mi})
		}
	}
	if len(hits) == 0 {
		return Model{}, "", fmt.Errorf("unknown model %q (models: %s)", model, keys(c.Models))
	}
	if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.prov
		}
		return Model{}, "", fmt.Errorf("model %q is advertised by multiple providers (%s); pass a provider to disambiguate (-p / /model %s <provider>)",
			model, strings.Join(names, ", "), model)
	}
	h := hits[0]
	m := Model{
		Providers: []string{h.prov},
		ID:        model,
		Context:   h.mi.ContextLength,
		MaxOut:    h.mi.MaxCompletionTokens,
		Vision:    slices.Contains(h.mi.InputModalities, "image"),
	}
	return m, h.prov, nil
}

// CatalogWantedModels returns IDs present in provider catalogs.
func CatalogWantedModels(cats map[string]Catalog) map[string]struct{} {
	wanted := make(map[string]struct{})
	for _, cat := range cats {
		for _, model := range cat.Models {
			if id := strings.TrimSpace(model.ID); id != "" {
				wanted[id] = struct{}{}
			}
		}
	}
	return wanted
}

// CatalogWantedModels adds configured and role-selected model IDs to the
// catalog IDs used for metadata refreshes.
func (c *Config) CatalogWantedModels(cats map[string]Catalog) map[string]struct{} {
	wanted := CatalogWantedModels(cats)
	if c == nil {
		return wanted
	}
	for name, model := range c.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			id = strings.TrimSpace(name)
		}
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	for _, role := range c.Roles {
		id := strings.TrimSpace(role.Model)
		if id == "" {
			continue
		}
		if model, ok := c.Models[id]; ok && strings.TrimSpace(model.ID) != "" {
			wanted[strings.TrimSpace(model.ID)] = struct{}{}
		} else {
			wanted[id] = struct{}{}
		}
	}
	for _, name := range []string{c.DefaultModel, c.CompactModel} {
		id := strings.TrimSpace(name)
		if id == "" {
			continue
		}
		if model, ok := c.Models[id]; ok && strings.TrimSpace(model.ID) != "" {
			wanted[strings.TrimSpace(model.ID)] = struct{}{}
		} else {
			wanted[id] = struct{}{}
		}
	}
	return wanted
}

// EnrichCatalogMetadata fills missing catalog context and reasoning metadata.
func EnrichCatalogMetadata(cat Catalog, metadata ModelsDevCache, providerIDs []string) (Catalog, bool) {
	changed := false
	for i := range cat.Models {
		if cat.Models[i].ContextLength <= 0 {
			if n := metadata.ContextLength(cat.Models[i].ID, providerIDs...); n > 0 {
				cat.Models[i].ContextLength = n
				changed = true
			}
		}

		info, ok := metadata.ReasoningFor(cat.Models[i].ID, providerIDs...)
		if !ok {
			continue
		}
		if !cat.Models[i].ReasoningKnown && len(cat.Models[i].ReasoningEfforts) == 0 {
			cat.Models[i].ReasoningEfforts = slices.Clone(info.Efforts)
			cat.Models[i].ReasoningKnown = true
			changed = true
		}
		if info.Toggle && !cat.Models[i].ReasoningToggle {
			cat.Models[i].ReasoningToggle = true
			changed = true
		}
	}
	return cat, changed
}
