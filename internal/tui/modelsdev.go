package tui

import (
	"strings"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/provider"
)

// modelsDevProviderIDs returns the most specific models.dev provider IDs
// first. Profiles can declare an alias when their runtime ID differs from the
// public metadata provider, as opencode does.
func modelsDevProviderIDs(resolved provider.Resolved, instanceName string) []string {
	ids := make([]string, 0, 3)
	for _, id := range []string{resolved.Catalog.ModelsDev, resolved.Profile.ID, instanceName} {
		if id == "" {
			continue
		}
		duplicate := false
		for _, existing := range ids {
			if existing == id {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ids = append(ids, id)
		}
	}
	return ids
}

func (m *model) modelsDevProviderIDs(instanceName string) []string {
	if m.cfg == nil {
		return []string{instanceName}
	}
	prov, ok := m.cfg.Providers[instanceName]
	if !ok {
		return []string{instanceName}
	}
	resolved, err := m.profiles.Resolve(provider.Instance{
		Name: instanceName, Profile: prov.Profile, BaseURL: prov.BaseURL, Protocol: prov.API,
	})
	if err != nil {
		return []string{instanceName}
	}
	return modelsDevProviderIDs(resolved, instanceName)
}

func (m *model) reasoningToggleFor(provName, apiID string) bool {
	if cat, ok := m.catalogs[provName]; ok {
		if info := cat.Find(apiID); info != nil && info.ReasoningToggle {
			return true
		}
	}
	metadata := config.LoadModelsDev()
	info, ok := metadata.ReasoningFor(apiID, m.modelsDevProviderIDs(provName)...)
	return ok && info.Toggle
}

// modelsDevWanted returns the model IDs for which public models.dev metadata
// may be useful. The upstream endpoint is one all-provider snapshot, so
// filtering here keeps the local cache small and avoids retaining unrelated
// models. Catalog models are all included because the same record can provide
// both a missing context window and the model's reasoning options.
func (m *model) modelsDevWanted(cats map[string]config.Catalog) map[string]struct{} {
	wanted := make(map[string]struct{})
	for _, cat := range cats {
		for _, mdl := range cat.Models {
			if id := strings.TrimSpace(mdl.ID); id != "" {
				wanted[id] = struct{}{}
			}
		}
	}
	if m == nil || m.cfg == nil {
		return wanted
	}
	for name, mdl := range m.cfg.Models {
		id := strings.TrimSpace(mdl.ID)
		if id == "" {
			id = strings.TrimSpace(name)
		}
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	for _, role := range m.cfg.Roles {
		if id := strings.TrimSpace(role.Model); id != "" {
			if mdl, ok := m.cfg.Models[id]; ok && strings.TrimSpace(mdl.ID) != "" {
				wanted[strings.TrimSpace(mdl.ID)] = struct{}{}
			} else {
				wanted[id] = struct{}{}
			}
		}
	}
	for _, name := range []string{m.cfg.DefaultModel, m.cfg.CompactModel} {
		id := strings.TrimSpace(name)
		if id == "" {
			continue
		}
		if mdl, ok := m.cfg.Models[id]; ok && strings.TrimSpace(mdl.ID) != "" {
			wanted[strings.TrimSpace(mdl.ID)] = struct{}{}
		} else {
			wanted[id] = struct{}{}
		}
	}
	return wanted
}

// enrichCatalogMetadata fills missing provider catalog context and reasoning
// metadata. Provider-advertised values remain authoritative; models.dev fills
// gaps and can add a separate toggle to an already-advertised effort list.
func enrichCatalogMetadata(cat config.Catalog, metadata config.ModelsDevCache, providerIDs []string) (config.Catalog, bool) {
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
			cat.Models[i].ReasoningEfforts = append([]string(nil), info.Efforts...)
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
