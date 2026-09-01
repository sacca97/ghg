package tui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/provider"
)

func compactCmdModel() *model {
	// NOTE: any test that drives setEffort/switchModel/compactCommand writes
	// through cfg.Save(); TestMain points GHG_HOME at a scratch dir so
	// those writes can never reach the real ~/.ghg/config.json.
	// serve the compaction summary so a bare /compact completes in-test
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"sim"}}]}`))
	}))
	m := &model{
		input:   newInput(),
		mouseOn: true, // matches the Run() default (wheel scroll + app selection)
		agent:   agent.New(testBackend(srv.URL, "k"), "kimi-k3-fast", 100, "sys"),
		cfg: &config.Config{
			DefaultModel: "kimi-k3-fast",
			Providers:    map[string]config.Provider{"inference": {BaseURL: "https://x", APIKey: "k"}},
			Models: map[string]config.Model{
				"kimi-k3-fast": {Providers: []string{"inference"}},
				"glm-5.2-fast": {Providers: []string{"inference"}},
				// the built-in compaction default, routable on inference
				config.DefaultCompactModel: {Providers: []string{"inference"}},
			},
		},
		modelName: "kimi-k3-fast",
		provName:  "inference",
		catalogs: map[string]config.Catalog{
			"inference": {Models: []config.ModelInfoLite{
				{ID: "kimi-k3-fast", ContextLength: 131072},
			}},
		},
	}
	m.width = 80
	m.input.SetWidth(78)
	return m
}

// Regression guard for the config corruption bug: running a persistence
// command from a test must write under the isolated GHG_HOME, never the
// user's real ~/.ghg.
func TestCompactCommandNeverTouchesRealHome(t *testing.T) {
	m := compactCmdModel()
	m.compactCommand([]string{"glm-5.2-fast"}) // triggers cfg.Save()
	dir := os.Getenv("GHG_HOME")
	if dir == "" || dir == filepath.Join(os.Getenv("HOME"), ".ghg") {
		t.Fatalf("tests must run with an isolated GHG_HOME, got %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("expected the save to land under GHG_HOME: %v", err)
	}
}

func TestCompactCommandSelectsModel(t *testing.T) {
	m := compactCmdModel()
	blocks := len(m.blocks)
	m.compactCommand([]string{"glm-5.2-fast"})
	if m.compactModel != "glm-5.2-fast" || m.compactProv != "" {
		t.Fatalf("compact model state: %q @ %q", m.compactModel, m.compactProv)
	}
	if m.agent.CompactModel != "glm-5.2-fast" || m.agent.CompactBackend == nil {
		t.Fatalf("agent should summarize with glm-5.2-fast on its own client")
	}
	if m.cfg.CompactModel != "glm-5.2-fast" {
		t.Fatalf("config should persist the pick, got %q", m.cfg.CompactModel)
	}
	m.compactCommand([]string{"off"})
	if m.compactModel != "" || m.agent.CompactModel != config.DefaultCompactModel || m.agent.CompactBackend == nil {
		t.Fatalf("off should restore the default compaction model: %q", m.compactModel)
	}
	if len(m.blocks) != blocks {
		t.Fatalf("successful compaction model changes should not append routine notes, got %v", m.blocks)
	}
}

// An empty compactModel resolves the built-in default from the config at
// apply time, so users who never picked one compact on deepseek-v4-flash.
func TestCompactModelEmptyResolvesDefault(t *testing.T) {
	m := compactCmdModel()
	m.applyCompactModel()
	if m.agent.CompactModel != config.DefaultCompactModel || m.agent.CompactBackend == nil {
		t.Fatalf("empty compactModel should resolve the default, got %q", m.agent.CompactModel)
	}
}

func TestCompactModelUsesTinyRole(t *testing.T) {
	m := compactCmdModel()
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleTiny: {Model: "glm-5.2-fast", Provider: "inference"},
	}
	m.applyCompactModel()
	if m.agent.CompactModel != "glm-5.2-fast" || m.agent.CompactBackend == nil {
		t.Fatalf("tiny role should provide the compaction route, got %q / %T", m.agent.CompactModel, m.agent.CompactBackend)
	}
}

// When the default model isn't in the user's config, the override clears and
// compaction falls back to the conversation's own model — no error note.
func TestCompactModelDefaultFallsBack(t *testing.T) {
	m := compactCmdModel()
	delete(m.cfg.Models, config.DefaultCompactModel)
	blocks := len(m.blocks)
	m.applyCompactModel()
	if m.agent.CompactBackend != nil || m.agent.CompactModel != "" {
		t.Fatal("unresolvable default should fall back to the current model")
	}
	if len(m.blocks) != blocks {
		t.Fatal("a missing default should not nag — only picked models earn an error note")
	}
}

func TestCompactCommandRejectsUnknownModel(t *testing.T) {
	m := compactCmdModel()
	m.compactCommand([]string{"nope"})
	if m.compactModel != "" || m.agent.CompactModel != "" {
		t.Fatal("unknown model must not become the compaction model")
	}
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "unknown model") {
		t.Fatalf("expected an error note, got %v", m.blocks)
	}
}

func TestContextLimitFromCatalog(t *testing.T) {
	m := compactCmdModel()
	if got := m.contextLimitFor("inference", "kimi-k3-fast"); got != 131072 {
		t.Fatalf("contextLimitFor: %d", got)
	}
	if got := m.contextLimitFor("inference", "unknown"); got != 0 {
		t.Fatalf("unknown model: %d", got)
	}
	// a fresh /models fetch re-resolves the agent's limit
	cats := map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{{ID: "kimi-k3-fast", ContextLength: 262144}}},
	}
	m.updateCatalogs(cats)
	if m.agent.ContextLimit != 262144 {
		t.Fatalf("agent limit should follow the catalog, got %d", m.agent.ContextLimit)
	}
}

func TestContextLimitFallsBackToModelsDev(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveModelsDev(config.ModelsDevCache{
		FetchedAt: time.Now(),
		Providers: map[string]map[string]int{"opencode": {"grok-4": 131072}},
	}); err != nil {
		t.Fatal(err)
	}
	profiles, err := provider.Load(provider.LoadOptions{UserDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := &model{
		cfg: &config.Config{
			Providers: map[string]config.Provider{"opencode": {Profile: "opencode"}},
		},
		profiles:  profiles,
		modelName: "grok-4",
		provName:  "opencode",
		catalogs: map[string]config.Catalog{
			"opencode": {Models: []config.ModelInfoLite{{ID: "grok-4"}}},
		},
	}
	if got := m.contextLimitFor("opencode", "grok-4"); got != 131072 {
		t.Fatalf("models.dev context = %d, want 131072", got)
	}

	// Provider metadata remains authoritative when it is present.
	m.catalogs["opencode"] = config.Catalog{Models: []config.ModelInfoLite{{ID: "grok-4", ContextLength: 262144}}}
	if got := m.contextLimitFor("opencode", "grok-4"); got != 262144 {
		t.Fatalf("provider context = %d, want 262144", got)
	}
}

// Bare /compact with no history reports there's nothing to fold rather than
// touching the compaction-model selection. (The busy path is exercised
// end-to-end in the running TUI; here m.prog is nil so we stay on the
// synchronous error branch.)
func TestCompactBareKeepsSelection(t *testing.T) {
	m := compactCmdModel()
	m.compactModel, m.compactProv = "glm-5.2-fast", ""
	m.applyCompactModel()
	m.busy = true // busy path: synchronous, never starts the goroutine
	m.command("/compact")
	if m.compactModel != "glm-5.2-fast" || m.agent.CompactModel != "glm-5.2-fast" {
		t.Fatal("bare /compact must not change the compaction-model selection")
	}
}

func TestCompactThresholdFor(t *testing.T) {
	cases := []struct {
		pct  int
		want float64
	}{
		{0, 0.4},   // unset → built-in default
		{70, 0.7},  // user preference
		{5, 0.1},   // clamped to the floor
		{99, 0.9},  // clamped to the ceiling
		{-30, 0.1}, // garbage clamps too
	}
	for _, tc := range cases {
		cfg := &config.Config{CompactPct: tc.pct}
		if got := compactThresholdFor(cfg); got != tc.want {
			t.Errorf("compactThresholdFor(%d) = %v, want %v", tc.pct, got, tc.want)
		}
	}
}

func TestSetCompactPct(t *testing.T) {
	m := compactCmdModel()
	m.agent.CompactThreshold = 0.5

	m.setCompactPct(60)
	if m.agent.CompactThreshold != 0.6 || m.cfg.CompactPct != 60 || m.compactPct() != 60 {
		t.Fatalf("setCompactPct(60): agent=%v cfg=%d", m.agent.CompactThreshold, m.cfg.CompactPct)
	}

	m.setCompactPct(120) // clamps to the 90 ceiling
	if m.agent.CompactThreshold != 0.9 || m.cfg.CompactPct != 90 {
		t.Fatalf("setCompactPct(120) should clamp to 90: agent=%v cfg=%d", m.agent.CompactThreshold, m.cfg.CompactPct)
	}
	m.setCompactPct(0) // clamps to the 10 floor
	if m.agent.CompactThreshold != 0.1 || m.cfg.CompactPct != 10 {
		t.Fatalf("setCompactPct(0) should clamp to 10: agent=%v cfg=%d", m.agent.CompactThreshold, m.cfg.CompactPct)
	}
}
