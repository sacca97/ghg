package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
)

func modelCmdModel() *model {
	m := &model{
		input: newInput(),
		agent: &agent.Agent{},
		cfg: &config.Config{
			DefaultModel: "kimi-k3-fast",
			Providers:    map[string]config.Provider{"inference": {BaseURL: "https://x", APIKey: "k"}},
			Models: map[string]config.Model{
				"kimi-k3-fast": {Providers: []string{"inference"}},
				"glm-5.2-fast": {Providers: []string{"inference"}},
			},
		},
		modelName: "kimi-k3-fast",
		provName:  "inference",
	}
	m.width = 80
	m.input.SetWidth(78)
	return m
}

func typeStr(t *testing.T, m *model, s string) *model {
	t.Helper()
	for _, r := range s {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	return m
}

// Regression: typing /model and pressing enter must open the interactive
// picker, NOT insert a newline. (The newline bug was KeyCtrlM == KeyEnter
// being forwarded to the textarea; this guards against its return.)
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
	if len(pp.list) != 4 || pp.list[1] != "plan" {
		t.Fatalf("model-role panel should list default, plan, fast, tiny: %+v", pp.list)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if pp := m.settings.top(); pp == nil || pp.kind != panelModel {
		t.Fatal("selecting a role should open its model panel")
	}
	if len(m.settings.top().items) == 0 {
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
