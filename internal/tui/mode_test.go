package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/config"
)

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
	m.agent.Role = config.RoleTiny

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
	if m.settings != nil || m.uiMode() != uiModeExecute || m.agent.Role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should select smart without changing execute mode, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
	clickModel()
	if m.agent.Role != config.RoleDefault || m.modelName != "default-model" || m.uiMode() != uiModeExecute {
		t.Fatalf("model click should select default without changing execute mode, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
	clickModel()
	if m.agent.Role != config.RoleFast || m.modelName != "fast-model" {
		t.Fatalf("model click should select fast, got %q/%q", m.agent.Role, m.modelName)
	}
	clickModel()
	if m.agent.Role != config.RoleTiny || m.modelName != "tiny-model" {
		t.Fatalf("model click should select tiny, got %q/%q", m.agent.Role, m.modelName)
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
	if m.settings != nil || m.uiMode() != uiModePlan || m.agent.Role != config.RoleSmart {
		t.Fatalf("mode click should cycle execute → plan/smart, got %q/%q", m.uiMode(), m.agent.Role)
	}
	clickModel()
	if m.uiMode() != uiModePlan || m.agent.Role != config.RoleDefault || m.modelName != "default-model" {
		t.Fatalf("model click should work in plan mode without changing it, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
	clickMode()
	if m.uiMode() != uiModeExecute || m.agent.Role != config.RoleFast || m.modelName != "fast-model" {
		t.Fatalf("second mode click should wrap plan → execute/fast, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
}

func TestModeSelectionEnforcesSmartAndFast(t *testing.T) {
	m := compactCmdModel()
	if err := m.setMode(uiModePlan); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModePlan || m.agent.Role != config.RoleSmart {
		t.Fatalf("plan mode should use smart, got mode %q role %q", m.uiMode(), m.agent.Role)
	}
	if err := m.setMode(uiModeExecute); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModeExecute || m.agent.Role != config.RoleFast {
		t.Fatalf("execute mode should default to fast, got mode %q role %q", m.uiMode(), m.agent.Role)
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
	if m.uiMode() != uiModePlan || m.agent.Role != config.RoleSmart {
		t.Fatalf("mode click did not activate plan/smart: %q/%q", m.uiMode(), m.agent.Role)
	}
	if got := m.statusView(); !strings.Contains(got, " plan ") {
		t.Fatalf("status should expose the selected mode: %q", got)
	}
}

func TestBottomStatusEffortIsSeparateAndClickable(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.agent.Effort = "high"
	_ = m.View()
	if got := m.statusView(); !strings.Contains(got, "│ kimi-k3-fast │ (high) │ execute │") {
		t.Fatalf("status should render effort as a separate segment: %q", got)
	}

	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.settings != nil || m.agent.Effort != "" || m.uiMode() != uiModeExecute {
		t.Fatalf("effort click should cycle high → off without opening a selector or changing mode: %q/%q", m.agent.Effort, m.uiMode())
	}

	_ = m.View()
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.agent.Effort != "low" {
		t.Fatalf("effort click should cycle off → low, got %q", m.agent.Effort)
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
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelModel || m.settings.top().role != uiModePlan {
		t.Fatal("clicking the plan role should open its model panel")
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
