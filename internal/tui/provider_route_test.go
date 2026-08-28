package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/provider"
)

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
			"override-model":  {Providers: []string{"route-test"}, ID: "minimax-m3", API: provider.ProtocolOpenAIChatCompletions, Context: 1000},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleDefault: {Model: "override-model", Provider: "route-test"},
			config.RoleSmart:   {Model: "anthropic-model", Provider: "route-test"},
			config.RoleFast:    {Model: "override-model", Provider: "route-test"},
			config.RoleTiny:    {Model: "anthropic-model", Provider: "route-test"},
		},
	}

	agent, _, _, err := buildAgentWithProfiles(cfg, "anthropic-model", "", "system", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.Backend.(*llm.AnthropicBackend); !ok {
		t.Fatalf("route-matched model backend = %T, want *llm.AnthropicBackend", agent.Backend)
	}

	agent, _, _, err = buildAgentWithProfiles(cfg, "override-model", "", "system", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.Backend.(*llm.OpenAIBackend); !ok {
		t.Fatalf("model API override backend = %T, want *llm.OpenAIBackend", agent.Backend)
	}

	agent, _, _, err = buildAgentWithProfiles(cfg, "responses-model", "", "system", profiles)
	if err != nil {
		t.Fatalf("Responses route should build: %v", err)
	}
	if _, ok := agent.Backend.(*llm.OpenAIResponsesBackend); !ok {
		t.Fatalf("Responses route backend = %T, want *llm.OpenAIResponsesBackend", agent.Backend)
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
		{config.RoleSmart, "smart-model", (*llm.AnthropicBackend)(nil)},
		{config.RoleFast, "fast-model", (*llm.OpenAIBackend)(nil)},
		{config.RoleTiny, "tiny-model", (*llm.AnthropicBackend)(nil)},
	} {
		ag, model, prov, err := buildAgentForRoleWithProfiles(cfg, tc.role, "system", profiles)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		if model != tc.model || prov != "route-test" || ag.Role != tc.role {
			t.Fatalf("%s route = %q @ %q, agent role %q", tc.role, model, prov, ag.Role)
		}
		switch tc.wantType.(type) {
		case *llm.AnthropicBackend:
			if _, ok := ag.Backend.(*llm.AnthropicBackend); !ok {
				t.Fatalf("%s backend = %T, want Anthropic", tc.role, ag.Backend)
			}
		case *llm.OpenAIBackend:
			if _, ok := ag.Backend.(*llm.OpenAIBackend); !ok {
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

	ag, _, _, err := buildAgentWithProfiles(cfg, "metadata-model", "", "system", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if ag.ContextLimit != 131072 {
		t.Fatalf("models.dev context = %d, want 131072", ag.ContextLimit)
	}
}

func routeTestProfiles(t *testing.T) provider.Profiles {
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
	profiles, err := provider.Load(provider.LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}
