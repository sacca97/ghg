package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

func TestNewConfiguredForRoleBuildsSharedRoute(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"local": {BaseURL: "http://127.0.0.1:1", API: string(models.ProtocolOpenAIChatCompletions), APIKey: "secret"},
		},
		Models: map[string]config.Model{
			"fast-model": {Providers: []string{"local"}, ID: "wire-model", Context: 4096},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleFast: {Model: "fast-model", Provider: "local"},
		},
	}
	ag, model, providerName, err := NewConfiguredForRole(cfg, models.Profiles{}, config.RoleFast, "system", false)
	if err != nil {
		t.Fatal(err)
	}
	if ag.Role != config.RoleFast || model != "fast-model" || providerName != "local" {
		t.Fatalf("route = %q @ %q, role %q", model, providerName, ag.Role)
	}
	if ag.Model != "wire-model" || ag.ContextLimit != 4096 {
		t.Fatalf("agent route = %q, context %d", ag.Model, ag.ContextLimit)
	}
	if _, ok := ag.Backend.(*models.Client); !ok {
		t.Fatalf("backend = %T, want OpenAI", ag.Backend)
	}
}

func TestBuildAgentUsesModelRoutesAndModelAPIOverride(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	profiles := routeTestProfiles(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"route-test": {
				Profile: "route-test",
				APIKey:  "test-key",
			},
		},
		Models: map[string]config.Model{
			"anthropic-model": {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
			"responses-model": {Providers: []string{"route-test"}, ID: "grok-4.6", Context: 1000},
			"override-model":  {Providers: []string{"route-test"}, ID: "minimax-m3", API: string(models.ProtocolOpenAIChatCompletions), Context: 1000},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleDefault: {Model: "override-model", Provider: "route-test"},
			config.RoleSmart:   {Model: "anthropic-model", Provider: "route-test"},
			config.RoleFast:    {Model: "override-model", Provider: "route-test"},
			config.RoleTiny:    {Model: "anthropic-model", Provider: "route-test"},
		},
	}

	ag, _, _, err := NewConfigured(BuildOptions{
		Config: cfg, Profiles: profiles, Model: "anthropic-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.Backend.(*models.AnthropicClient); !ok {
		t.Fatalf("route-matched model backend = %T, want *models.AnthropicClient", ag.Backend)
	}

	ag, _, _, err = NewConfigured(BuildOptions{
		Config: cfg, Profiles: profiles, Model: "override-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.Backend.(*models.Client); !ok {
		t.Fatalf("model API override backend = %T, want *models.Client", ag.Backend)
	}

	ag, _, _, err = NewConfigured(BuildOptions{
		Config: cfg, Profiles: profiles, Model: "responses-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatalf("Responses route should build: %v", err)
	}
	if _, ok := ag.Backend.(*models.OpenAIResponsesClient); !ok {
		t.Fatalf("Responses route backend = %T, want *models.OpenAIResponsesClient", ag.Backend)
	}
}

func TestBuildAgentForRoleUsesRoleRoute(t *testing.T) {
	profiles := routeTestProfiles(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"route-test": {Profile: "route-test", APIKey: "test-key"},
		},
		Models: map[string]config.Model{
			"default-model": {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
			"smart-model":   {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
			"fast-model":    {Providers: []string{"route-test"}, ID: "qwen3-coder", Context: 1000},
			"tiny-model":    {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleDefault: {Model: "default-model", Provider: "route-test"},
			config.RoleSmart:   {Model: "smart-model", Provider: "route-test"},
			config.RoleFast:    {Model: "fast-model", Provider: "route-test"},
			config.RoleTiny:    {Model: "tiny-model", Provider: "route-test"},
		},
	}

	for _, tc := range []struct {
		role, model string
		wantType    any
	}{
		{config.RoleSmart, "smart-model", (*models.AnthropicClient)(nil)},
		{config.RoleFast, "fast-model", (*models.Client)(nil)},
		{config.RoleTiny, "tiny-model", (*models.AnthropicClient)(nil)},
	} {
		ag, model, prov, err := NewConfiguredForRole(cfg, profiles, tc.role, "system", false)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		if model != tc.model || prov != "route-test" || ag.Role != tc.role {
			t.Fatalf("%s route = %q @ %q, agent role %q", tc.role, model, prov, ag.Role)
		}
		switch tc.wantType.(type) {
		case *models.AnthropicClient:
			if _, ok := ag.Backend.(*models.AnthropicClient); !ok {
				t.Fatalf("%s backend = %T, want Anthropic", tc.role, ag.Backend)
			}
		case *models.Client:
			if _, ok := ag.Backend.(*models.Client); !ok {
				t.Fatalf("%s backend = %T, want OpenAI", tc.role, ag.Backend)
			}
		}
	}
}

func TestBuildAgentUsesModelsDevContextFallback(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveModelsDev(config.ModelsDevCache{
		Providers: map[string]map[string]int{"route-test": {"minimax-m3": 131072}},
	}); err != nil {
		t.Fatal(err)
	}
	profiles := routeTestProfiles(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"route-test": {Profile: "route-test", APIKey: "test-key"},
		},
		Models: map[string]config.Model{
			"metadata-model": {Providers: []string{"route-test"}, ID: "minimax-m3"},
		},
	}

	ag, _, _, err := NewConfigured(BuildOptions{
		Config: cfg, Profiles: profiles, Model: "metadata-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag.ContextLimit != 131072 {
		t.Fatalf("models.dev context = %d, want 131072", ag.ContextLimit)
	}
}

func routeTestProfiles(t *testing.T) models.Profiles {
	t.Helper()
	dir := t.TempDir()
	data := `schema: 1
id: route-test
display_name: Route Test
protocol: openai-chat-completions
base_url: http://127.0.0.1:9999/v1
auth:
  kind: bearer
  header: Authorization
  env_var: ROUTE_TEST_KEY
routes:
  - models: ["minimax-*"]
    protocol: anthropic-messages
    auth:
      kind: header
      header: x-api-key
    default_headers:
      anthropic-version: "2023-06-01"
  - models: ["grok-*"]
    protocol: openai-responses
catalog:
  kind: none
`
	if err := os.WriteFile(filepath.Join(dir, "route-test.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := models.Load(models.LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}
