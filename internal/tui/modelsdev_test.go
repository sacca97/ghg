package tui

import (
	"testing"

	"github.com/sacca97/ghg/internal/config"
)

func TestModelsDevWantedIncludesListedModels(t *testing.T) {
	m := &model{cfg: &config.Config{
		Models: map[string]config.Model{
			"configured": {ID: "configured-api", Context: 128000},
			"missing":    {ID: "missing-api"},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleTiny: {Model: "role-only"},
		},
	}}
	wanted := m.modelsDevWanted(map[string]config.Catalog{
		"provider": {Models: []config.ModelInfoLite{
			{ID: "catalog-known", ContextLength: 64000},
			{ID: "catalog-missing"},
		}},
	})

	for _, id := range []string{"configured-api", "missing-api", "role-only", "catalog-known", "catalog-missing"} {
		if _, ok := wanted[id]; !ok {
			t.Errorf("wanted does not include %q", id)
		}
	}
	for _, id := range []string{"configured", "unknown"} {
		if _, ok := wanted[id]; ok {
			t.Errorf("wanted unexpectedly includes %q", id)
		}
	}
}

func TestEnrichCatalogMetadataAddsReasoningOptions(t *testing.T) {
	cat := config.Catalog{Models: []config.ModelInfoLite{
		{ID: "deepseek-v4-flash"},
		{ID: "toggle-only"},
		{ID: "no-controls"},
		{ID: "provider-known", ReasoningEfforts: []string{"low"}},
	}}
	metadata := config.ModelsDevCache{
		Providers: map[string]map[string]int{
			"opencode": {"deepseek-v4-flash": 1048576},
		},
		Reasoning: map[string]map[string]config.ModelsDevReasoning{
			"opencode": {
				"deepseek-v4-flash": {Toggle: true, Efforts: []string{"high", "max"}},
				"toggle-only":       {Toggle: true},
				"no-controls":       {},
				"provider-known":    {Toggle: true, Efforts: []string{"high"}},
			},
		},
	}

	got, changed := enrichCatalogMetadata(cat, metadata, []string{"opencode"})
	if !changed {
		t.Fatal("metadata enrichment should report changes")
	}
	deepseek := got.Find("deepseek-v4-flash")
	if deepseek == nil || deepseek.ContextLength != 1048576 || !deepseek.ReasoningKnown || !deepseek.ReasoningToggle || len(deepseek.ReasoningEfforts) != 2 || deepseek.ReasoningEfforts[1] != "max" {
		t.Fatalf("deepseek metadata = %+v", deepseek)
	}
	toggle := got.Find("toggle-only")
	if toggle == nil || !toggle.ReasoningKnown || !toggle.ReasoningToggle {
		t.Fatalf("toggle metadata = %+v", toggle)
	}
	plain := got.Find("no-controls")
	if plain == nil || !plain.ReasoningKnown || plain.ReasoningToggle || len(plain.ReasoningEfforts) != 0 {
		t.Fatalf("no-controls metadata = %+v", plain)
	}
	providerKnown := got.Find("provider-known")
	if providerKnown == nil || providerKnown.ReasoningEfforts[0] != "low" || !providerKnown.ReasoningToggle {
		t.Fatalf("provider effort should remain authoritative while toggle is added: %+v", providerKnown)
	}
}
