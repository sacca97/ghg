package main

import (
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
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

func TestWorkerEffortForModelDoesNotEscalateBaseline(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveCatalog("test", "", []models.ModelInfo{{ID: "model", ReasoningEfforts: []string{"low", "high"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		current, want string
	}{
		{current: "", want: ""},
		{current: "medium", want: "low"},
		{current: "high", want: "high"},
	} {
		if got := workerEffortForModel("test", "model", tc.current, false); got != tc.want {
			t.Errorf("current=%q: effort = %q, want %q", tc.current, got, tc.want)
		}
	}
}

func TestWorkerApprovalModeChangesLive(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	cfg := config.Default()
	runtime, err := tools.NewToolRuntime(nil, tools.ApprovalAsk, false)
	if err != nil {
		t.Fatal(err)
	}
	w := &workerProcessState{cfg: cfg, runtime: runtime}
	if err := w.configure(workerConfigureRequest{Approval: "auto-review"}); err != nil {
		t.Fatal(err)
	}
	if got := runtime.CurrentApprovalMode(); got != tools.ApprovalAutoReview {
		t.Fatalf("live approval mode = %q, want %q", got, tools.ApprovalAutoReview)
	}
	if cfg.Execution == nil || cfg.Execution.Approval != string(tools.ApprovalAutoReview) {
		t.Fatalf("saved approval mode = %+v", cfg.Execution)
	}
	if err := w.configure(workerConfigureRequest{Approval: "invalid"}); err == nil {
		t.Fatal("invalid approval mode should fail")
	}
}
