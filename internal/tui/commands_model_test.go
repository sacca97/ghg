package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

func TestCompactCommandRejectsModelSelection(t *testing.T) {
	m := compactCmdModel()
	m.command("/compact meta/muse-spark-1.3-contributor")
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "does not accept a model") {
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
	// a fresh /models fetch re-resolves the active limit
	cats := map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{{ID: "kimi-k3-fast", ContextLength: 262144}}},
	}
	m.updateCatalogs(cats)
	if m.contextLimit != 262144 {
		t.Fatalf("active limit should follow the catalog, got %d", m.contextLimit)
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
	profiles, err := models.Load(models.LoadOptions{UserDir: t.TempDir()})
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
		if got := config.CompactThreshold(cfg); got != tc.want {
			t.Errorf("CompactThreshold(%d) = %v, want %v", tc.pct, got, tc.want)
		}
	}
}

func TestSetCompactPct(t *testing.T) {
	m := compactCmdModel()
	m.setCompactPct(60)
	if m.cfg.CompactPct != 60 || m.compactPct() != 60 {
		t.Fatalf("setCompactPct(60): cfg=%d", m.cfg.CompactPct)
	}

	m.setCompactPct(120) // clamps to the 90 ceiling
	if m.cfg.CompactPct != 90 {
		t.Fatalf("setCompactPct(120) should clamp to 90: cfg=%d", m.cfg.CompactPct)
	}
	m.setCompactPct(0) // clamps to the 10 floor
	if m.cfg.CompactPct != 10 {
		t.Fatalf("setCompactPct(0) should clamp to 10: cfg=%d", m.cfg.CompactPct)
	}
}

func TestEffortCycleAndParse(t *testing.T) {
	got := ""
	for _, want := range []string{"low", "medium", "high", "", "low"} {
		got = nextEffort(defaultEfforts, got)
		if got != want {
			t.Fatalf("cycle: got %q want %q", got, want)
		}
	}
	if nextEffort(defaultEfforts, "bogus") != "" {
		t.Fatal("unknown level should reset to off")
	}
	if effortLabel("") != "off" || effortLabel("high") != "high" {
		t.Fatal("labels")
	}
	for in, want := range map[string]string{"off": "", "low": "low", "high": "high"} {
		if lv, ok := parseEffort(defaultEfforts, in); !ok || lv != want {
			t.Fatalf("parse %q: %q %v", in, lv, ok)
		}
	}
	if _, ok := parseEffort(defaultEfforts, "ultra"); ok {
		t.Fatal("invalid level accepted")
	}
}

func TestEffortCompletion(t *testing.T) {
	_, cs := completions("/effort h", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "high" {
		t.Fatalf("effort completion: %v", texts(cs))
	}
}

func TestEffortsForAdvertisedLevels(t *testing.T) {
	m := &model{
		provName:  "inference",
		modelName: "deepseek-v4-flash",
		catalogs: map[string]config.Catalog{
			"inference": {Models: []config.ModelInfoLite{
				{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}, ReasoningToggle: true},
				{ID: "claude-opus-5", ReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}},
				{ID: "gemini-3.5-flash"}, // no reasoning_efforts
				{ID: "toggle-only", ReasoningKnown: true, ReasoningToggle: true},
				{ID: "no-controls", ReasoningKnown: true},
			}},
		},
	}
	if got := m.effortsFor(); len(got) != 4 || got[0] != "" || got[3] != "max" {
		t.Fatalf("advertised levels: %v", got)
	}
	if next := nextEffort(m.effortsFor(), "high"); next != "max" {
		t.Fatalf("cycle should reach max: %q", next)
	}
	for _, e := range m.effortsFor() {
		if e == "on" {
			t.Fatal("a model with graded efforts should not add a duplicate on state")
		}
	}
	if _, ok := parseEffort(m.effortsFor(), "medium"); ok {
		t.Fatal("medium should be rejected for deepseek")
	}

	// "none" collapses into off ("")
	m.modelName = "claude-opus-5"
	got := m.effortsFor()
	if got[0] != "" || len(got) != 7 {
		t.Fatalf("claude levels: %v", got)
	}
	for _, e := range got {
		if e == "none" {
			t.Fatalf("none should map to off: %v", got)
		}
	}

	// no advertised levels → defaults
	m.modelName = "gemini-3.5-flash"
	if got := m.effortsFor(); len(got) != len(defaultEfforts) {
		t.Fatalf("gemini should fall back to defaults: %v", got)
	}

	m.modelName = "toggle-only"
	if got := m.effortsFor(); len(got) != 2 || got[0] != "" || got[1] != "on" {
		t.Fatalf("toggle-only model should expose off/on: %v", got)
	}
	m.modelName = "no-controls"
	if got := m.effortsFor(); len(got) != 1 || got[0] != "" {
		t.Fatalf("known model without controls should expose off only: %v", got)
	}

	// unknown provider → defaults
	m.provName = "elsewhere"
	if got := m.effortsFor(); len(got) != len(defaultEfforts) {
		t.Fatalf("missing catalog should fall back to defaults: %v", got)
	}
}

// bare /effort opens the level selector (settings panel) so the user can
// scroll ↑/↓ and pick — cycling blindly hides the choices.
func TestEffortBareOpensSelector(t *testing.T) {
	m := compactCmdModel()
	m.command("/effort")
	if m.settings == nil {
		t.Fatal("bare /effort should open the settings")
	}
	pp := m.settings.top()
	if pp == nil || pp.kind != panelEffort {
		t.Fatalf("expected the effort panel, got %+v", pp)
	}
	if len(pp.rows) != len(defaultEfforts) || pp.rows[pp.selected].value != m.effort {
		t.Fatalf("effort panel should list the model's levels on the current one: %v @%d", pp.rows, pp.selected)
	}
	// scroll down to low and apply with enter
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.effort != "low" {
		t.Fatalf("selecting low in the selector should apply it, got %q", m.effort)
	}
	// the selector came from /effort, not ctrl+p: commit-and-close, don't
	// strand the user on a settings root they never opened
	if m.settings != nil {
		t.Fatal("enter in a directly-opened selector should close the settings")
	}
}

// A user-picked effort updates the global default; the worker persists the
// live session value when it receives the route change.
func TestSetEffortPersistsGlobal(t *testing.T) {
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.effort = "medium"

	m.setEffort("low") // the user picks a level
	if m.effort != "low" {
		t.Fatalf("effort: %q", m.effort)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DefaultEffort != "low" {
		t.Fatalf("global default should follow the pick, got %q", reloaded.DefaultEffort)
	}
	// A reconciliation must not rewrite the user's global default.
	m.resetEffort("")
	reloaded, _ = config.Load()
	if reloaded.DefaultEffort != "low" {
		t.Fatalf("reset must not touch the global default, got %q", reloaded.DefaultEffort)
	}
}

func TestUpdateCatalogsResetsUnsupportedEffort(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep setEffort's cfg.Save() away from the real config
	m := &model{
		cfg:       &config.Config{},
		provName:  "inference",
		modelName: "deepseek-v4-flash",
		effort:    "medium",
	}
	m.updateCatalogs(map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
		}},
	})
	if m.effort != "" {
		t.Fatalf("unsupported effort should reset to off, got %q", m.effort)
	}

	// a supported effort survives the refresh
	m.effort = "high"
	m.updateCatalogs(map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
		}},
	})
	if m.effort != "high" {
		t.Fatalf("supported effort should survive, got %q", m.effort)
	}
}

func TestModelSwitchUsesConfiguredEffort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.catalogs = map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
			{ID: "toggle-only", ReasoningKnown: true, ReasoningToggle: true},
			{ID: "no-controls", ReasoningKnown: true},
		}},
	}
	m.cfg.Models["deepseek-v4-flash"] = config.Model{Providers: []string{"inference"}}
	m.cfg.Models["toggle-only"] = config.Model{Providers: []string{"inference"}}
	m.cfg.Models["no-controls"] = config.Model{Providers: []string{"inference"}}

	m.switchModel("deepseek-v4-flash", "inference")
	if m.effort != "medium" {
		t.Fatalf("expected configured effort for deepseek-v4-flash, got %q", m.effort)
	}

	m.switchModel("toggle-only", "inference")
	if m.effort != "medium" {
		t.Fatalf("expected configured effort for toggle-only, got %q", m.effort)
	}

	m.switchModel("no-controls", "inference")
	if m.effort != "medium" {
		t.Fatalf("expected configured effort for no-controls, got %q", m.effort)
	}
}

func TestBottomStatusControlsCycleModelAndMode(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleDefault: {Model: "default-model", Provider: "inference"},
		config.RoleSmart:   {Model: "smart-model", Provider: "inference"},
		config.RoleFast:    {Model: "fast-model", Provider: "inference"},
		config.RoleTiny:    {Model: "tiny-model", Provider: "inference"},
	}
	for _, name := range []string{"default-model", "smart-model", "fast-model", "tiny-model"} {
		m.cfg.Models[name] = config.Model{Providers: []string{"inference"}}
	}
	m.modelName = "tiny-model"
	m.role = config.RoleTiny

	clickModel := func() {
		t, _ := m.Update(tea.MouseMsg{
			Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			X: m.statusModelX + m.statusModelW/2, Y: statusInfoRow(m.height),
		})
		m = t.(*model)
		_ = m.View()
	}
	_ = m.View()
	clickModel()
	if m.settings != nil || m.uiMode() != uiModeExecute || m.role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should select smart without changing execute mode, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
	clickModel()
	if m.role != config.RoleDefault || m.modelName != "default-model" || m.uiMode() != uiModeExecute {
		t.Fatalf("model click should select default without changing execute mode, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
	clickModel()
	if m.role != config.RoleFast || m.modelName != "fast-model" {
		t.Fatalf("model click should select fast, got %q/%q", m.role, m.modelName)
	}
	clickModel()
	if m.role != config.RoleTiny || m.modelName != "tiny-model" {
		t.Fatalf("model click should select tiny, got %q/%q", m.role, m.modelName)
	}

	clickMode := func() {
		t, _ := m.Update(tea.MouseMsg{
			Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			X: m.statusModeX + m.statusModeW/2, Y: statusInfoRow(m.height),
		})
		m = t.(*model)
		_ = m.View()
	}
	clickMode()
	if m.settings != nil || m.uiMode() != uiModePlan || m.role != config.RoleTiny {
		t.Fatalf("mode click should cycle execute → plan without changing role, got %q/%q", m.uiMode(), m.role)
	}
	clickModel()
	if m.uiMode() != uiModePlan || m.role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should work in plan mode, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
	clickMode()
	if m.uiMode() != uiModeExecute || m.role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("second mode click should wrap plan → execute without changing role, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
}

func TestModeSelectionPreservesModel(t *testing.T) {
	m := compactCmdModel()
	origRole := m.role
	origModel := m.modelName
	if err := m.setMode(uiModePlan); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModePlan || m.role != origRole || m.modelName != origModel {
		t.Fatalf("plan mode should preserve model and role, got mode %q role %q", m.uiMode(), m.role)
	}
	if err := m.setMode(uiModeExecute); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModeExecute || m.role != origRole || m.modelName != origModel {
		t.Fatalf("execute mode should preserve model and role, got mode %q role %q", m.uiMode(), m.role)
	}
}

func TestBottomModeClickCyclesWithoutOpeningPalette(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	_ = m.View()
	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusModeX + m.statusModeW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.settings != nil {
		t.Fatal("clicking the bottom mode should not open a selector")
	}
	if m.uiMode() != uiModePlan {
		t.Fatalf("mode click did not activate plan: %q", m.uiMode())
	}
	if got := m.statusView(); !strings.Contains(got, " plan ") {
		t.Fatalf("status should expose the selected mode: %q", got)
	}
}

func TestBottomStatusEffortIsSeparateAndClickable(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.effort = "high"
	_ = m.View()
	if got := m.statusView(); !strings.Contains(got, "│ kimi-k3-fast │ high (dynamic) │ execute │") {
		t.Fatalf("status should render effort as a separate segment: %q", got)
	}

	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.settings != nil || m.effort != "" || m.uiMode() != uiModeExecute {
		t.Fatalf("effort click should cycle high → off without opening a selector or changing mode: %q/%q", m.effort, m.uiMode())
	}

	_ = m.View()
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.effort != "low" {
		t.Fatalf("effort click should cycle off → low, got %q", m.effort)
	}
}

func TestDynamicReasoningCommandTogglesAndUpdatesStatus(t *testing.T) {
	m := compactCmdModel()
	m.effort = "high"
	if !m.dynamicReasoningEnabled() {
		t.Fatal("dynamic reasoning should default to enabled")
	}

	m.command("/dynamic-reasoning")
	if m.dynamicReasoningEnabled() || strings.Contains(ansi.Strip(m.statusView()), "high (dynamic)") {
		t.Fatalf("toggle off did not update state/status: enabled=%v view=%q", m.dynamicReasoningEnabled(), m.statusView())
	}

	m.command("/dynamic-reasoning")
	if !m.dynamicReasoningEnabled() || !strings.Contains(ansi.Strip(m.statusView()), "high (dynamic)") {
		t.Fatalf("toggle on did not update state/status: enabled=%v view=%q", m.dynamicReasoningEnabled(), m.statusView())
	}
}

func TestAvailableModelItemsRequireConfiguredProvider(t *testing.T) {
	m := &model{cfg: &config.Config{
		Providers: map[string]config.Provider{
			"ready":   {BaseURL: "https://ready.example", APIKey: "key"},
			"missing": {BaseURL: "https://missing.example"},
		},
		Models: map[string]config.Model{
			"shared":       {Providers: []string{"missing", "ready"}},
			"only-missing": {Providers: []string{"missing"}},
		},
	}}
	items := m.availableModelItems()
	if len(items) != 1 || items[0].model != "shared" || items[0].provider != "ready" {
		t.Fatalf("available routes should exclude providers without keys: %+v", items)
	}
}

func TestPaletteMouseClicksActivateRootAndPanelRows(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.openPalette()

	rows, positions := m.paletteRootRows()
	modelIndex := -1
	for i, it := range m.settings.items {
		if it.title == "Model" {
			modelIndex = i
			break
		}
	}
	if modelIndex < 0 {
		t.Fatal("settings should contain Model")
	}
	if positions[modelIndex] >= len(rows) {
		t.Fatalf("Model row position %d is outside %d rendered rows", positions[modelIndex], len(rows))
	}

	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 5, Y: 1 + m.paletteRootListStart() + positions[modelIndex],
	})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatal("clicking the Model row should open the role panel")
	}

	// Click the plan role. The panel's title and separator occupy the first
	// two settings rows; the role rows follow them.
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 5, Y: 1 + 2 + 1,
	})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelModel || m.settings.top().role != config.RoleSmart {
		t.Fatal("clicking the smart role should open its model panel")
	}

	// The first model row is directly below the model-panel title/separator.
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 5, Y: 1 + 2,
	})
	m = tm.(*model)
	if m.cfg.Roles[config.RoleSmart].Model == "" {
		t.Fatal("clicking a role model should persist the selected route")
	}
}

func TestModelBareEnterOpensPicker(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/model")
	if m.menu == nil {
		t.Fatal("typing /model should focus the completion menu")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatalf("/model + enter should open the role picker; input=%q LineCount=%d", m.input.Value(), m.input.LineCount())
	}
	if m.input.Value() != "" || m.input.LineCount() != 1 {
		t.Errorf("enter must not leave a newline in the input: value=%q LineCount=%d", m.input.Value(), m.input.LineCount())
	}
}

// The ctrl+p settings's first suggestion is Model; enter drills into its
// interactive panel without leaving the settings.
func TestModelPaletteEnterOpensPicker(t *testing.T) {
	m := modelCmdModel()
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = tm.(*model)
	if m.settings == nil {
		t.Fatal("ctrl+p should open the command settings")
	}
	if len(m.settings.items) == 0 || m.settings.items[0].title != "Model" {
		t.Fatalf("first suggestion should be Model, got %+v", m.settings.items)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	pp := m.settings.top()
	if pp == nil || pp.kind != panelRole {
		t.Fatalf("settings Model + enter should push the model-role panel; input=%q", m.input.Value())
	}
	if len(pp.rows) != 4 || pp.rows[1].value != "smart" {
		t.Fatalf("model-role panel should list default, smart, fast, tiny: %+v", pp.rows)
	}
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleDefault: {Model: "kimi-k3-fast", Provider: "inference"},
		config.RoleSmart:   {Model: "glm-5.2-fast", Provider: "inference"},
		config.RoleFast:    {Model: "kimi-k3-fast", Provider: "inference"},
		config.RoleTiny:    {Model: "glm-5.2-fast", Provider: "inference"},
	}
	rows, _, _ := m.panelContent(pp)
	view := ansi.Strip(strings.Join(rows, "\n"))
	for _, want := range []string{"default  — kimi-k3-fast", "smart  — glm-5.2-fast", "fast  — kimi-k3-fast", "tiny  — glm-5.2-fast"} {
		if !strings.Contains(view, want) {
			t.Fatalf("model-role panel should show configured role models, missing %q in %q", want, view)
		}
	}
	if strings.Contains(view, "(current)") {
		t.Fatalf("model-role panel should not add a redundant current marker: %q", view)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if pp := m.settings.top(); pp == nil || pp.kind != panelModel {
		t.Fatal("selecting a role should open its model panel")
	}
	if len(m.settings.top().rows) == 0 {
		t.Fatal("model panel should list the configured routes")
	}
}

// Selecting a model name completes it on the first enter; the second enter
// submits. Neither may insert a newline into the input.
func TestModelArgEnterNeverNewlines(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/model glm")
	if m.menu == nil {
		t.Fatal("expected model-name completion menu")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // complete the name
	m = tm.(*model)
	if m.input.LineCount() != 1 {
		t.Fatalf("completing a model name must not newline: value=%q", m.input.Value())
	}
	if m.input.Value() == "/model glm" {
		t.Fatalf("enter should have accepted the completion, still %q", m.input.Value())
	}
}
