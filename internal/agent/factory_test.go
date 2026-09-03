package agent

import (
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
