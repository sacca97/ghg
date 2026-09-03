package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/config"
	"strings"
	"testing"
	"time"
)

func pickerCfg() *config.Config {
	return &config.Config{
		DefaultModel: "kimi-k3-fast",
		Providers:    map[string]config.Provider{"inference": {BaseURL: "https://catalog.example/v1", APIKey: "test-key"}},
		Models: map[string]config.Model{
			"kimi-k3-fast": {Providers: []string{"inference"}},
		},
	}
}

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
	wanted := m.cfg.CatalogWantedModels(map[string]config.Catalog{
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

	got, changed := config.EnrichCatalogMetadata(cat, metadata, []string{"opencode"})
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

// Catalog-advertised models without a config entry appear after the
// configured routes, marked fromCatalog; configured models never duplicate.
func TestBuildModelItemsAppendsCatalogRoutes(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	cfg := pickerCfg()
	if err := config.SaveCatalogs(map[string]config.Catalog{
		"inference": {
			FetchedAt: time.Now(),
			BaseURL:   "https://catalog.example/v1",
			Models: []config.ModelInfoLite{
				{ID: "kimi-k3-fast", ContextLength: 1048576}, // configured: skip
				{ID: "deepseek-v4-pro", ContextLength: 1048576},
				{ID: "glm-5.2", ContextLength: 1000000},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	items := buildModelItems(cfg)
	if len(items) != 3 {
		t.Fatalf("want 1 configured + 2 catalog routes, got %d: %+v", len(items), items)
	}
	if items[0].model != "kimi-k3-fast" || items[0].fromCatalog {
		t.Errorf("configured route first, unmarked: %+v", items[0])
	}
	if items[1].model != "deepseek-v4-pro" || !items[1].fromCatalog {
		t.Errorf("catalog routes sorted after configured: %+v", items[1])
	}
	if items[2].model != "glm-5.2" || !items[2].fromCatalog || items[2].provider != "inference" {
		t.Errorf("catalog route should carry its provider: %+v", items[2])
	}
}

// The picker view marks catalog routes (new) and shows the stale hint when
// the cache is past its TTL.
func TestModelPickerViewMarksCatalogAndStale(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	cfg := pickerCfg()
	if err := config.SaveCatalogs(map[string]config.Catalog{
		"inference": {
			FetchedAt: time.Now().Add(-48 * time.Hour), // past the 24h TTL
			BaseURL:   "https://catalog.example/v1",
			Models:    []config.ModelInfoLite{{ID: "deepseek-v4-pro"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	m := &model{cfg: cfg, modelName: "kimi-k3-fast", provName: "inference"}
	m.openModelPicker()
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatal("role picker should open")
	}
	// /model is role-first; open the currently highlighted role's model list.
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.settings.top() == nil || m.settings.top().kind != panelModel {
		t.Fatal("role picker should open a model panel")
	}
	view := m.paletteView()
	if !strings.Contains(view, "(new)") {
		t.Error("catalog routes should carry a (new) marker")
	}
	if !strings.Contains(view, "deepseek-v4-pro") {
		t.Error("catalog model should be listed")
	}
	if !strings.Contains(view, "stale") || !strings.Contains(view, "/model refresh") {
		t.Error("stale catalog should hint at /model refresh")
	}
}

// Selecting a catalog route runs the normal switchModel path (which persists
// the choice as the default on use).
func TestModelPickerSelectsCatalogRoute(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveCatalogs(map[string]config.Catalog{
		"inference": {
			FetchedAt: time.Now(),
			BaseURL:   "https://x",
			Models:    []config.ModelInfoLite{{ID: "deepseek-v4-pro", ContextLength: 1048576}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	m := compactCmdModel() // cfg providers carry an API key; switchModel needs it
	m.openModelPicker()
	roles := m.settings.top()
	if roles == nil || roles.kind != panelRole {
		t.Fatal("role picker should open")
	}
	for roles.list[roles.midx] != "default" {
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
		m = tm.(*model)
		roles = m.settings.top()
	}
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	p := m.settings.top()
	for p.idx < len(p.items)-1 && !p.items[p.idx].fromCatalog {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
		m = tm.(*model)
		p = m.settings.top()
	}
	if p == nil || p.kind != panelModel || !p.items[p.idx].fromCatalog {
		t.Fatal("no catalog route in picker")
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.modelName != "deepseek-v4-pro" || m.provName != "inference" {
		t.Fatalf("enter on a catalog route should switch models, got %s/%s", m.provName, m.modelName)
	}
	if got := m.cfg.Roles[config.RoleDefault]; got.Model != "deepseek-v4-pro" || got.Provider != "inference" {
		t.Errorf("the switch should persist as the default role, got %+v", got)
	}
	if _, ok := m.cfg.Models["deepseek-v4-pro"]; ok {
		t.Error("catalog routes must not be written into cfg.Models")
	}
}

// /model refresh forces a catalog refetch instead of switching models. The
// fixture's provider (BaseURL https://x, no DNS) fails its fetch, so the
// seeded catalog must survive untouched — failure keeps the stale cache.
func TestModelRefreshForcesRefetch(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveCatalogs(map[string]config.Catalog{
		"inference": {FetchedAt: time.Now(), BaseURL: "https://x", Models: []config.ModelInfoLite{{ID: "seed"}}},
	}); err != nil {
		t.Fatal(err)
	}
	m := modelCmdModel()
	m = typeStr(t, m, "/model refresh")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if len(m.blocks) == 0 || !strings.Contains(m.blocks[len(m.blocks)-1].text, "refreshing model catalogs") {
		t.Fatalf("refresh notice should be appended, got %+v", m.blocks)
	}
	got := config.LoadCatalogs()["inference"]
	if len(got.Models) != 1 || got.Models[0].ID != "seed" {
		t.Errorf("failed refetch must keep the existing cache, got %+v", got)
	}
}

// refresh is offered as a /model argument, alongside catalog-advertised models.
func TestModelRefreshCompletion(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveCatalogs(map[string]config.Catalog{
		"inference": {FetchedAt: time.Now(), BaseURL: "https://x",
			Models: []config.ModelInfoLite{{ID: "catalog-only-model"}}},
	}); err != nil {
		t.Fatal(err)
	}
	m := modelCmdModel()
	_, cs := completions("/model r", m.modelCands(), m.providerCands(), m.providerCands(), nil, nil)
	if len(cs) != 1 || cs[0].Text != "refresh" {
		t.Fatalf("refresh should complete under /model, got %+v", cs)
	}
	_, cs = completions("/model cat", m.modelCands(), m.providerCands(), m.providerCands(), nil, nil)
	if len(cs) != 1 || cs[0].Text != "catalog-only-model" {
		t.Fatalf("catalog-only models should complete under /model, got %+v", cs)
	}
	// no provider completion after refresh
	_, cs = completions("/model refresh inf", m.modelCands(), m.providerCands(), m.providerCands(), nil, nil)
	if len(cs) != 0 {
		t.Errorf("refresh takes no provider argument, got %+v", cs)
	}
}

// staleCatalogs names providers with a missing or expired catalog.
func TestStaleCatalogs(t *testing.T) {
	cfg := pickerCfg()
	if got := staleCatalogs(cfg, map[string]config.Catalog{}); len(got) != 1 || got[0] != "inference" {
		t.Errorf("missing catalog is stale: %v", got)
	}
	fresh := map[string]config.Catalog{"inference": {FetchedAt: time.Now()}}
	if got := staleCatalogs(cfg, fresh); len(got) != 0 {
		t.Errorf("fresh catalog is not stale: %v", got)
	}
}

func TestBuildModelItems(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"a": {BaseURL: "https://a"},
			"b": {BaseURL: "https://b"},
		},
		Models: map[string]config.Model{
			"zeta":  {Providers: []string{"b", "a"}}, // declared order kept
			"alpha": {Providers: []string{"a", "ghost"}},
		},
	}
	items := buildModelItems(cfg)
	if len(items) != 4 {
		t.Fatalf("items: %+v", items)
	}
	// models sorted alphabetically
	if items[0].model != "alpha" || items[2].model != "zeta" {
		t.Fatalf("model order: %+v", items)
	}
	// provider order per model preserved
	if items[2].provider != "b" || items[3].provider != "a" {
		t.Fatalf("provider order: %+v", items)
	}
	if items[0].url != "https://a" {
		t.Fatalf("url: %+v", items[0])
	}
	if items[1].provider != "ghost" || items[1].url != "" {
		t.Fatalf("unknown provider should keep empty url: %+v", items[1])
	}
	if got := buildModelItems(&config.Config{}); len(got) != 0 {
		t.Fatalf("empty config: %+v", got)
	}
}

func TestModelItemLabelUsesProviderFirstFormat(t *testing.T) {
	if got := modelItemLabel(modelItem{provider: "openai", model: "gpt-5"}); got != "openai/gpt-5" {
		t.Fatalf("model label = %q, want openai/gpt-5", got)
	}
	if got := modelItemLabel(modelItem{provider: "opencode", model: "grok-4", fromCatalog: true, unavailable: true, unavailableReason: "no adapter"}); got != "opencode/grok-4 (new) (unsupported: no adapter)" {
		t.Fatalf("annotated model label = %q", got)
	}
}

func TestResolveModelFuzzy(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{"a": {BaseURL: "https://a"}},
		Models: map[string]config.Model{
			"claude-sonnet-4": {Providers: []string{"a"}},
			"claude-opus-4":   {Providers: []string{"a"}},
		},
	}

	// exact name passes through
	if got, ok, _ := resolveModelFuzzy(cfg, "claude-opus-4"); !ok || got != "claude-opus-4" {
		t.Fatalf("exact passthrough: %q %v", got, ok)
	}

	// unique substring resolves
	if got, ok, _ := resolveModelFuzzy(cfg, "sonnet"); !ok || got != "claude-sonnet-4" {
		t.Fatalf("substring resolve: %q %v", got, ok)
	}

	// ambiguous prefix reports candidates
	got, ok, alts := resolveModelFuzzy(cfg, "claude")
	if ok || len(alts) != 2 {
		t.Fatalf("ambiguous resolve: %q %v %v", got, ok, alts)
	}

	// no match at all
	if _, ok, _ := resolveModelFuzzy(cfg, "zzz"); ok {
		t.Fatal("no-match should not resolve")
	}
}

func TestTranscriptSelectionUsesDisplayCells(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(8, 30))
	m.appendRaw(blockText, "a界🙂z tail")
	m.refreshVP()
	y := m.blocks[0].y0 + m.contentPad() - m.vp.YOffset + 2
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: y})
	m = tm.(*model)
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 2, Y: y + 1})
	m = tm.(*model)
	tm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 2, Y: y + 1})
	m = tm.(*model)
	if cmd == nil {
		t.Fatal("a non-empty selection should copy asynchronously")
	}
	if got := m.selectedText(); got != "a界🙂z\nta" {
		t.Fatalf("wrapped display-cell selection = %q, want %q", got, "a界🙂z\nta")
	}
}

// A drag over a tool row is a selection, not a click. Only a stationary
// release toggles the tool block.
func TestTranscriptDragDoesNotToggleTool(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	m.appendRaw(blockTool, "line1\nline2")
	y := m.blocks[0].y0 + m.contentPad() - m.vp.YOffset + 2
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: y})
	m = tm.(*model)
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 3, Y: y + 1})
	m = tm.(*model)
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 3, Y: y + 1})
	m = tm.(*model)
	if m.blocks[0].expanded {
		t.Fatal("dragging across a tool row must not toggle it")
	}

	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: y})
	m = tm.(*model)
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 0, Y: y})
	m = tm.(*model)
	if !m.blocks[0].expanded {
		t.Fatal("a stationary tool click should toggle it")
	}
}

// Mouse capture defaults ON (wheel scroll + clicks work). A config "mouse":
// false opts back into no capture for terminals that need native selection.
func TestMouseDefaultsOn(t *testing.T) {
	cfg := config.Default()
	if cfg.Mouse != nil {
		t.Fatalf("default config must not set mouse (nil = on), got %v", *cfg.Mouse)
	}
	b := false
	cfg2 := &config.Config{Mouse: &b}
	if cfg2.Mouse == nil || *cfg2.Mouse {
		t.Fatal("explicit false should stay off")
	}
}

// A wheel-up MouseMsg routed through Update must scroll the transcript viewport
// up (YOffset increases) and drop follow mode. This is the event tmux forwards
// to ghg now that mouse_any_flag=1 (the regression was: capture off → tmux
// swallowed the wheel into copy-mode, so YOffset never moved).
func TestWheelScrollsTranscript(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 20))
	// overflow the viewport so there's somewhere to scroll
	for i := 0; i < 40; i++ {
		m.appendAssistant("line of transcript content that is long enough to matter")
	}
	m.vp.GotoBottom()
	if !m.vp.AtBottom() {
		t.Fatal("setup: should start at bottom")
	}
	start := m.vp.YOffset

	up := tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 40, Y: 10})
	um, _ := m.Update(up)
	m = um.(*model)
	if m.vp.YOffset >= start {
		t.Fatalf("wheel-up must scroll up: YOffset %d -> %d", start, m.vp.YOffset)
	}
	if got := start - m.vp.YOffset; got != 1 {
		t.Fatalf("one wheel detent should move one row, moved %d", got)
	}
	if m.follow {
		t.Fatal("scrolling up off the bottom must drop follow mode")
	}

	down := tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 40, Y: 10})
	for i := 0; i < 20; i++ {
		um, _ := m.Update(down)
		m = um.(*model)
	}
	if !m.vp.AtBottom() {
		t.Fatalf("wheel-down must scroll back to bottom, YOffset=%d", m.vp.YOffset)
	}
	if !m.follow {
		t.Fatal("returning to the bottom must re-engage follow mode")
	}
}

func TestWheelScrollKeepsPromptAndStatusFixed(t *testing.T) {
	m := compactCmdModel()
	tm, _ := m.Update(mkWinSize(80, 24))
	m = tm.(*model)
	for i := range 30 {
		m.appendAssistant(fmt.Sprintf("reply %d", i))
	}
	m.layout()

	before := strings.Split(ansi.Strip(m.View()), "\n")
	inputRow := -1
	for i, line := range before {
		if strings.Contains(line, "Ask ghg anything") {
			inputRow = i
			break
		}
	}
	if inputRow < 0 {
		t.Fatalf("input row not found in initial view:\n%s", strings.Join(before, "\n"))
	}
	tail := strings.Join(before[inputRow:], "\n")

	for i := 0; i < 5; i++ {
		tm, _ = m.Update(tea.MouseMsg(tea.MouseEvent{
			Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 40, Y: 10,
		}))
		m = tm.(*model)
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		gotInputRow := -1
		for j, line := range lines {
			if strings.Contains(line, "Ask ghg anything") {
				gotInputRow = j
				break
			}
		}
		if gotInputRow != inputRow {
			t.Fatalf("wheel step %d moved the input row: %d -> %d", i+1, inputRow, gotInputRow)
		}
		if got := strings.Join(lines[inputRow:], "\n"); got != tail {
			t.Fatalf("wheel step %d changed the prompt/status tail:\nwant:\n%s\ngot:\n%s", i+1, tail, got)
		}
	}
}

func TestScrolledUpViewportDoesNotSnapToBottomOnAssistantOutput(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 20))
	for i := 0; i < 40; i++ {
		m.appendAssistant(fmt.Sprintf("line %d of initial transcript content", i))
	}
	m.vp.GotoBottom()
	start := m.vp.YOffset

	// Scroll up 5 lines
	for i := 0; i < 5; i++ {
		um, _ := m.Update(tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 40, Y: 10}))
		m = um.(*model)
	}
	if m.follow {
		t.Fatal("expected follow == false after scrolling up")
	}
	scrolledOffset := m.vp.YOffset
	if scrolledOffset >= start {
		t.Fatalf("expected viewport to have scrolled up from %d, got %d", start, scrolledOffset)
	}

	// Incoming assistant text arrives
	m.appendAssistant("new assistant message arriving while scrolled up")

	if m.follow {
		t.Fatal("appendAssistant should not reset follow to true when user is scrolled up")
	}
	if m.vp.YOffset != scrolledOffset {
		t.Fatalf("viewport moved from %d to %d on assistant text while follow was false", scrolledOffset, m.vp.YOffset)
	}
}
