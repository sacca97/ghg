package main

import (
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

func TestConfigureWorkerCompactionFallsBackByRole(t *testing.T) {
	newConfig := func() *config.Config {
		return &config.Config{
			DefaultModel: "default-model",
			Providers: map[string]config.Provider{
				"test": {BaseURL: "https://provider.example/v1", API: string(models.ProtocolOpenAICompletions), APIKey: "key"},
			},
			Models: map[string]config.Model{
				"tiny-model":    {Providers: []string{"test"}, MaxOut: 1500},
				"fast-model":    {Providers: []string{"test"}, MaxOut: 1600},
				"default-model": {Providers: []string{"test"}, MaxOut: 1700},
				"smart-model":   {Providers: []string{"test"}, MaxOut: 1800},
			},
			Roles: map[string]config.RoleConfig{
				config.RoleTiny:    {Model: "tiny-model", Provider: "test"},
				config.RoleFast:    {Model: "fast-model", Provider: "test"},
				config.RoleDefault: {Model: "default-model", Provider: "test"},
				config.RoleSmart:   {Model: "smart-model", Provider: "test"},
			},
		}
	}
	tests := []struct {
		name      string
		configure func(*config.Config)
		want      string
		wantMax   int
	}{
		{name: "tiny", want: "tiny-model", wantMax: 1500},
		{name: "fast when tiny is absent", configure: func(cfg *config.Config) { delete(cfg.Roles, config.RoleTiny) }, want: "fast-model", wantMax: 1600},
		{name: "fast when tiny is invalid", configure: func(cfg *config.Config) {
			cfg.Roles[config.RoleTiny] = config.RoleConfig{Model: "missing-model", Provider: "test"}
		}, want: "fast-model", wantMax: 1600},
		{name: "default after fast", configure: func(cfg *config.Config) {
			delete(cfg.Roles, config.RoleTiny)
			delete(cfg.Roles, config.RoleFast)
			delete(cfg.Roles, config.RoleSmart)
		}, want: "default-model", wantMax: 1700},
		{name: "smart last", configure: func(cfg *config.Config) {
			cfg.DefaultModel = ""
			delete(cfg.Roles, config.RoleTiny)
			delete(cfg.Roles, config.RoleFast)
			delete(cfg.Roles, config.RoleDefault)
		}, want: "smart-model", wantMax: 1800},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			if tc.configure != nil {
				tc.configure(cfg)
			}
			ag := agent.New(nil, "active-model", 100, "system")
			configureWorkerCompaction(ag, cfg, models.Profiles{}, "system")
			if len(ag.CompactCandidates) == 0 || ag.CompactCandidates[0].Model != tc.want {
				t.Fatalf("compaction candidates = %+v, want first %q", ag.CompactCandidates, tc.want)
			}
			if ag.CompactCandidates[0].MaxTokens != tc.wantMax {
				t.Fatalf("compaction output cap = %d, want %d", ag.CompactCandidates[0].MaxTokens, tc.wantMax)
			}
		})
	}
}

func TestWorkerEffortForModelUsesAdvertisedEffort(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveCatalog("test", "", []models.ModelInfo{{ID: "model", ReasoningEfforts: []string{"low", "high"}}}); err != nil {
		t.Fatal(err)
	}
	if got := workerEffortForModel("test", "model", "", false); got != "high" {
		t.Fatalf("effort = %q, want high", got)
	}
}
