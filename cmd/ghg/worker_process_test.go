package main

import (
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
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

// The worker owns configuration writes: a controller asks for a role model or
// the dynamic-reasoning switch and the worker persists it, so both clients get
// one validation and state-transition path.
func TestWorkerConfigurePersistsRoleModelAndDynamicReasoning(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	cfg := &config.Config{
		DefaultModel: "fast-model",
		Providers: map[string]config.Provider{
			"test": {BaseURL: "https://provider.example/v1", API: string(models.ProtocolOpenAICompletions), APIKey: "key"},
		},
		Models: map[string]config.Model{
			"fast-model": {Providers: []string{"test"}, MaxOut: 1600},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleFast: {Model: "fast-model", Provider: "test"},
		},
	}
	w := &workerProcessState{cfg: cfg, profiles: models.Profiles{}, ag: agent.New(nil, "fast-model", 100, "system")}
	dynamicReasoning := false
	if err := w.configure(workerConfigureRequest{
		Role: config.RoleFast, Model: "fast-model", Provider: "test",
		DynamicReasoning: &dynamicReasoning, PersistDynamicReasoning: true,
		PersistRoleModel: true, Mode: "execute",
	}); err != nil {
		t.Fatal(err)
	}

	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Roles[config.RoleFast]; got.Model != "fast-model" || got.Provider != "test" {
		t.Fatalf("persisted role route = %+v, want fast-model/test", got)
	}
	if saved.DynamicReasoning == nil || *saved.DynamicReasoning {
		t.Fatalf("persisted dynamic reasoning = %v, want false", saved.DynamicReasoning)
	}

	// An unknown role must be refused before anything is written.
	marker := config.RoleConfig{Model: "fast-model", Provider: "test"}
	if err := w.configure(workerConfigureRequest{Model: "fast-model", Provider: "test", PersistRoleModel: true}); err == nil {
		t.Fatal("persisting without a role should fail")
	}
	after, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Roles[config.RoleFast]; got != marker {
		t.Fatalf("failed persist changed the role route: %+v", got)
	}
}

func TestWorkerConfigureBusyDoesNotPersistOrApplyRoute(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"test": {BaseURL: "https://provider.example/v1", API: string(models.ProtocolOpenAICompletions), APIKey: "key"},
		},
		Models: map[string]config.Model{
			"old-model": {Providers: []string{"test"}},
			"new-model": {Providers: []string{"test"}},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleFast: {Model: "old-model", Provider: "test"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	w := &workerProcessState{
		cfg:          cfg,
		ag:           agent.New(nil, "old-model", 100, "system"),
		activeCancel: func() {},
		state:        workerwire.StateRunning,
		modelName:    "old-model",
		provider:     "test",
		role:         config.RoleFast,
	}
	err := w.configure(workerConfigureRequest{
		Role: config.RoleFast, Model: "new-model", Provider: "test", PersistRoleModel: true,
	})
	if err == nil || err.Error() != "worker is busy or stopping" {
		t.Fatalf("configure error = %v, want busy error", err)
	}
	if got := w.ag.Model; got != "old-model" {
		t.Fatalf("live model = %q, want old-model", got)
	}
	if got := cfg.Roles[config.RoleFast].Model; got != "old-model" {
		t.Fatalf("in-memory role model = %q, want old-model", got)
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Roles[config.RoleFast].Model; got != "old-model" {
		t.Fatalf("saved role model = %q, want old-model", got)
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
	if err := w.configure(workerConfigureRequest{Approval: "auto"}); err != nil {
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
