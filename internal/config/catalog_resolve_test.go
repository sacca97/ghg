package config

import (
	"strings"
	"testing"
)

// catalogFixture isolates GHG_HOME and seeds one cached provider catalog.
func catalogFixture(t *testing.T, prov string, models ...ModelInfoLite) {
	t.Helper()
	t.Setenv("GHG_HOME", t.TempDir())
	if err := SaveCatalogs(map[string]Catalog{prov: {Models: models}}); err != nil {
		t.Fatal(err)
	}
}

func cfgWithProviders(names ...string) *Config {
	c := &Config{
		Providers: map[string]Provider{},
		Models:    map[string]Model{},
	}
	for _, n := range names {
		c.Providers[n] = Provider{BaseURL: "https://" + n, API: "openai-completions"}
	}
	return c
}

// A model advertised by exactly one provider's catalog resolves without a
// config entry, with capabilities populated from the catalog.
func TestResolveCatalogFallbackSingleProvider(t *testing.T) {
	catalogFixture(t, "inference", ModelInfoLite{
		ID:                  "glm-5.2",
		ContextLength:       1000000,
		MaxCompletionTokens: 128000,
		ReasoningEfforts:    []string{"low", "high"},
		InputModalities:     []string{"text"},
	})
	cfg := cfgWithProviders("inference")

	route, err := cfg.Resolve("glm-5.2", "")
	if err != nil {
		t.Fatal(err)
	}
	if route.Provider.BaseURL != "https://inference" || route.APIID != "glm-5.2" || route.ProviderName != "inference" || route.ModelName != "glm-5.2" {
		t.Fatalf("route: %+v", route)
	}
	if route.Model.Context != 1000000 || route.Model.MaxOut != 128000 {
		t.Errorf("synthetic model should carry catalog context/maxOut, got %+v", route.Model)
	}
	if route.Model.Vision {
		t.Error("text-only catalog entry must not mark the model vision-capable")
	}
	if len(route.Model.Providers) != 1 || route.Model.Providers[0] != "inference" {
		t.Errorf("routing: %+v", route.Model.Providers)
	}
}

// Vision is populated from the catalog's input_modalities.
func TestResolveCatalogFallbackVision(t *testing.T) {
	catalogFixture(t, "inference", ModelInfoLite{
		ID:              "kimi-k9",
		ContextLength:   256000,
		InputModalities: []string{"text", "image"},
	})
	cfg := cfgWithProviders("inference")
	route, err := cfg.Resolve("kimi-k9", "")
	if err != nil || !route.Model.Vision {
		t.Fatalf("image-capable catalog entry should resolve with Vision=true: %+v %v", route.Model, err)
	}
}

// A model id advertised by several providers without a pinned provider errors
// naming the candidates; pinning with a provider disambiguates.
func TestResolveCatalogFallbackAmbiguous(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := SaveCatalogs(map[string]Catalog{
		"alpha": {Models: []ModelInfoLite{{ID: "shared-model", ContextLength: 1000}}},
		"beta":  {Models: []ModelInfoLite{{ID: "shared-model", ContextLength: 2000}}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithProviders("alpha", "beta")

	_, err := cfg.Resolve("shared-model", "")
	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("ambiguity error must name both providers, got %v", err)
	}
	if !strings.Contains(err.Error(), "disambiguate") {
		t.Fatalf("error should instruct how to disambiguate, got %v", err)
	}

	route, err := cfg.Resolve("shared-model", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if route.Provider.BaseURL != "https://beta" || route.Model.Context != 2000 || route.ProviderName != "beta" {
		t.Fatalf("pinned provider should win with its catalog entry: %+v", route)
	}
}

func TestResolveCatalogFallbackOwnerWinsOverDefaultProvider(t *testing.T) {
	catalogFixture(t, "provider-b", ModelInfoLite{
		ID:            "b-only-model",
		ContextLength: 128000,
	})
	cfg := cfgWithProviders("provider-a", "provider-b")
	cfg.DefaultProvider = "provider-a"

	route, err := cfg.Resolve("b-only-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if route.ProviderName != "provider-b" || route.Provider.BaseURL != "https://provider-b" {
		t.Fatalf("expected provider-b to win for b-only-model, got providerName=%q baseURL=%q", route.ProviderName, route.Provider.BaseURL)
	}
}

// Config entries stay authoritative: a configured model never falls through
// to the catalog, even when the catalog advertises the same id differently.
func TestResolveConfigWinsOverCatalog(t *testing.T) {
	catalogFixture(t, "inference", ModelInfoLite{ID: "kimi-k3", ContextLength: 999999, MaxCompletionTokens: 999999})
	cfg := cfgWithProviders("inference")
	cfg.Models["kimi-k3"] = Model{Providers: []string{"inference"}, Context: 131072}

	route, err := cfg.Resolve("kimi-k3", "")
	if err != nil {
		t.Fatal(err)
	}
	if route.Model.Context != 131072 || route.Model.MaxOut != 0 {
		t.Errorf("config entry must win over catalog values, got %+v", route.Model)
	}
	if route.APIID != "kimi-k3" {
		t.Errorf("id should default to the map key, got %q", route.APIID)
	}
}

// Unknown ids still error, and catalogs from unconfigured providers are ignored.
func TestResolveCatalogFallbackMisses(t *testing.T) {
	catalogFixture(t, "ghost", ModelInfoLite{ID: "phantom-model", ContextLength: 1})
	cfg := cfgWithProviders("inference")

	if _, err := cfg.Resolve("nope", ""); err == nil {
		t.Fatal("unknown model must still error")
	}
	if _, err := cfg.Resolve("phantom-model", ""); err == nil {
		t.Fatal("catalog for an unconfigured provider must not resolve")
	}
}
