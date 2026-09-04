package tui

import (
	"net"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/config"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func TestPaletteOpensAndClosesOnEsc(t *testing.T) {
	m := compactCmdModel()
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = tm.(*model)
	if m.settings == nil {
		t.Fatal("ctrl+p should open the settings")
	}
	// esc pops the dialog (opencode: esc pops one level)
	tm, _ = m.key(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*model)
	if m.settings != nil {
		t.Fatal("esc should close the settings")
	}
}

func TestPaletteSuggestedGroupOnTop(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	if m.settings.items[0].category != "Suggested" {
		t.Fatalf("empty filter should pin a Suggested group, got %q", m.settings.items[0].category)
	}
	titles := map[string]bool{}
	for _, it := range m.settings.items {
		titles[it.title] = true
	}
	for _, want := range []string{"Model", "Resume session", "Compact session", "Goal", "Help", "Quit"} {
		if !titles[want] {
			t.Errorf("settings missing %q", want)
		}
	}
}

func TestPaletteFilter(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	for _, r := range "new sess" {
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	if len(m.settings.items) != 1 || m.settings.items[0].title != "New session" {
		t.Fatalf("filter 'new sess': %+v", m.settings.items)
	}
	if m.settings.items[0].category != "Session" {
		t.Fatalf("filtering drops the Suggested group, got %q", m.settings.items[0].category)
	}
	// backspace restores the full list
	for i := 0; i < 8; i++ {
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyBackspace})
		m = tm.(*model)
	}
	if len(m.settings.items) < 10 {
		t.Fatalf("backspace should restore all items, got %d", len(m.settings.items))
	}
}

func TestPaletteNavigationWraps(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	n := len(m.settings.items)
	// up from the top wraps to the bottom
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.settings.idx != n-1 {
		t.Fatalf("up from 0 should wrap to %d, got %d", n-1, m.settings.idx)
	}
	// down from the bottom wraps to the top
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	if m.settings.idx != 0 {
		t.Fatalf("down should wrap to 0, got %d", m.settings.idx)
	}
}

func TestPaletteEnterRunsCommand(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	for _, r := range "quit" {
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	_, cmd := m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Quit should return tea.Quit")
	}
	if msg := cmd(); msg != tea.Quit() {
		t.Fatalf("expected tea.QuitMsg, got %v", msg)
	}
}

func TestPaletteViewRendersCategories(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	m.width = 100
	v := m.paletteView()
	for _, want := range []string{"Commands", "Suggested", "Agent", "Session", "Display", "App", "esc close"} {
		if !strings.Contains(v, want) {
			t.Errorf("settings view missing %q:\n%s", want, v)
		}
	}
}

func TestPaletteFitsSmallTerminalAndScrolls(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 80, 10
	m.openPalette()
	if lines := strings.Count(m.paletteView(), "\n") + 1; lines > m.height-1 {
		t.Fatalf("settings should fit below the header: %d lines in %d rows", lines, m.height)
	}
	for i := 0; i < len(m.settings.items)-1; i++ {
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
		m = tm.(*model)
	}
	if m.settings.offset == 0 {
		t.Fatal("moving past the short settings viewport should scroll the list")
	}
	before := m.settings.offset
	for i := 0; i < 2; i++ {
		tm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
		m = tm.(*model)
	}
	if m.settings.offset >= before {
		t.Fatalf("wheel-up should scroll the settings upward: %d -> %d", before, m.settings.offset)
	}
	if lines := strings.Count(m.paletteView(), "\n") + 1; lines > m.height-1 {
		t.Fatalf("scrolled settings should still fit: %d lines in %d rows", lines, m.height)
	}
}

// The settings must not swallow the agent's interrupt keys while a turn runs:
// it routes through key() only as a modal, and ctrl+c closes it like esc.
func TestPaletteCtrlCClosesNotQuits(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(*model)
	if m.settings != nil {
		t.Fatal("ctrl+c should close the settings, not quit the app")
	}
}

// Reversible rows change the setting in place with ←/→ while the settings
// stays open — the core of the interactive settings.
func TestPaletteArrowsStepEffortInPlace(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	var tm tea.Model
	for _, r := range "effort" {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	if m.settings.items[m.settings.idx].title != "Reasoning effort" {
		t.Fatalf("filter 'effort' should select Reasoning effort")
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRight})
	m = tm.(*model)
	if m.settings == nil {
		t.Fatal("→ must keep the settings open")
	}
	if m.effort != "low" {
		t.Fatalf("→ should step off → low, got %q", m.effort)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyLeft})
	m = tm.(*model)
	if m.effort != "" {
		t.Fatalf("← should step back to off, got %q", m.effort)
	}
}

// Toggles apply in place too: enter flips thinking tokens, settings open.
func TestPaletteToggleThinkingInPlace(t *testing.T) {
	m := compactCmdModel()
	m.showThinking = true // matches the Run() default
	m.openPalette()
	var tm tea.Model
	for _, r := range "thinking" {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.settings == nil {
		t.Fatal("enter on a toggle must keep the settings open")
	}
	if m.showThinking {
		t.Fatal("enter should have toggled thinking tokens off")
	}
	// the toggle persists to the global config (reload proves the round-trip)
	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Thinking == nil || *reloaded.Thinking {
		t.Fatalf("expected thinking: false saved to config, got %v", reloaded.Thinking)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if !m.showThinking {
		t.Fatal("a second enter should toggle thinking tokens back on")
	}
	reloaded, err = config.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Thinking == nil || !*reloaded.Thinking {
		t.Fatalf("expected thinking: true saved to config, got %v", reloaded.Thinking)
	}
}

// Sub-panels drill in and esc pops back one level to the root list.
func TestPalettePanelPushPop(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	var tm tea.Model
	for _, r := range "effort" {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	pp := m.settings.top()
	if pp == nil || pp.kind != panelEffort {
		t.Fatal("enter should push the effort panel")
	}
	if pp.levels[pp.lidx] != m.effort {
		t.Fatalf("panel should start on the current level, got %q", pp.levels[pp.lidx])
	}
	// filter input is paused inside a panel: typing runes does nothing
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = tm.(*model)
	if m.settings.filter != "effort" {
		t.Fatalf("panel should not edit the root filter, got %q", m.settings.filter)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() != nil {
		t.Fatal("esc should pop back to the root list, not close")
	}
}

// The effort panel lists the model's levels and applies the highlighted one.
func TestPaletteEffortPanelApplies(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	var tm tea.Model
	for _, r := range "effort" {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter}) // push panel
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyDown}) // off → low
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyDown}) // low → medium
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.effort != "medium" {
		t.Fatalf("enter should apply the highlighted level, got %q", m.effort)
	}
	if m.settings.top() != nil {
		t.Fatal("enter should pop the panel after applying")
	}
}

// Model settings are two-level: choose the role first, then commit a single
// model/provider route. Browsing a role's routes does not switch the live
// session until enter.
func TestPaletteModelRolePanelSelectsRoute(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	var tm tea.Model
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter}) // choose Model
	m = tm.(*model)
	pp := m.settings.top()
	if pp == nil || pp.kind != panelRole {
		t.Fatal("enter should push the model-role panel")
	}
	if len(pp.list) != 4 {
		t.Fatalf("expected four role choices, got %d", len(pp.list))
	}
	// The test agent has no role, so it selects default by default.
	if m.settings.top().list[m.settings.top().midx] != "default" {
		t.Fatalf("role selector should reach default, got %q", m.settings.top().list[m.settings.top().midx])
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter}) // open default's models
	m = tm.(*model)
	pp = m.settings.top()
	if pp == nil || pp.kind != panelModel || len(pp.items) != 3 {
		t.Fatalf("default model panel should list three keyed routes: %+v", pp)
	}
	modelBefore := m.modelName
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyUp}) // choose glm without previewing
	m = tm.(*model)
	if m.modelName != modelBefore {
		t.Fatal("browsing a role model must not switch the live route")
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if got := m.cfg.Roles[config.RoleDefault]; got.Model != "glm-5.2-fast" || got.Provider != "inference" {
		t.Fatalf("selected route should be saved to default role, got %+v", got)
	}
	if m.modelName != "glm-5.2-fast" {
		t.Fatalf("execute mode should activate the selected route, got %q", m.modelName)
	}
	if m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatal("after selecting a route, non-direct settings should return to roles")
	}
}

// The goal panel edits the goal inline; enter applies and starts working.
func TestPaletteGoalPanelSetsGoal(t *testing.T) {
	m := compactCmdModel()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	go func() {
		dec := workerwire.NewDecoder(serverConn)
		for {
			if _, err := dec.Read(); err != nil {
				return
			}
		}
	}()
	m.workerClient = workerwire.NewClient(clientConn, "test")
	// commitGoal submits the first turn through the worker projection.
	m.prog = tea.NewProgram(m, tea.WithoutRenderer())
	defer m.prog.Kill()
	m.openPalette()
	var tm tea.Model
	for _, r := range "goal" {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter}) // push goal panel
	m = tm.(*model)
	for _, r := range "ship it" {
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.currentGoal() != "ship it" {
		t.Fatalf("enter should set the goal, got %q", m.currentGoal())
	}
	if !m.busy {
		t.Fatal("setting a goal should start the first turn")
	}
	if m.settings.top() != nil {
		t.Fatal("enter should pop the goal panel")
	}
}

// The compaction-model panel applies on ←/→ without closing.
func TestPaletteCompactPanelAppliesInPlace(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	var tm tea.Model
	for m.settings.items[m.settings.idx].title != "Compaction model" {
		tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
		m = tm.(*model)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter}) // push compact panel
	m = tm.(*model)
	pp := m.settings.top()
	if pp == nil || pp.kind != panelCompact {
		t.Fatal("enter should push the compaction panel")
	}
	if pp.midx != 0 { // no override configured → the default row selected
		t.Fatalf("should start on the default row, got %d", pp.midx)
	}
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyRight}) // apply in place
	m = tm.(*model)
	if m.compactModel == "" {
		t.Fatal("→ should apply the highlighted model")
	}
	if m.settings.top() == nil {
		t.Fatal("→ must keep the panel open")
	}
}

// The panel's first row restores the built-in default (""), not "current
// model": picking a model then selecting the default row resets the override.
func TestPaletteCompactPanelDefaultRowRestores(t *testing.T) {
	m := compactCmdModel()
	m.compactCommand([]string{"glm-5.2-fast"}) // pick an override first
	m.openPaletteOn("Compaction model")
	pp := m.settings.top()
	if pp == nil || pp.kind != panelCompact {
		t.Fatal("openPaletteOn should land in the compaction panel")
	}
	if !strings.Contains(pp.list[0], "default (") {
		t.Fatalf("first row should read default (…), got %q", pp.list[0])
	}
	for pp.midx != 0 { // navigate to the default row
		tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyUp})
		m = tm.(*model)
	}
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.compactModel != "" {
		t.Fatalf("the default row should restore the built-in default: %q", m.compactModel)
	}
	// enter popped the panel — and since it was opened directly (not drilled
	// into from the root list), the whole settings closed with it
	if m.settings != nil && m.settings.top() != nil {
		t.Fatal("enter should pop the panel")
	}
}

// The Compaction level row steps the threshold ±10% in place and shows it.
func TestPaletteCompactionLevelSteps(t *testing.T) {
	m := compactCmdModel()
	m.cfg.CompactPct = 40 // default 40%
	m.openPalette()
	var it *paletteItem
	for i := range m.settings.items {
		if m.settings.items[i].title == "Compaction level" {
			it = &m.settings.items[i]
			break
		}
	}
	if it == nil {
		t.Fatal("settings should have a Compaction level row")
	}
	if it.stepFwd == nil || it.stepBack == nil {
		t.Fatal("Compaction level should be ←/→ steppable")
	}
	it.stepFwd(m)
	if m.compactPct() != 50 {
		t.Fatalf("→ should step to 50%%, got %v", m.compactPct())
	}
	it.stepBack(m)
	it.stepBack(m)
	if m.compactPct() != 30 {
		t.Fatalf("← ← should step to 30%%, got %v", m.compactPct())
	}
	if state := paletteState(m, *it); !strings.Contains(state, "30%") {
		t.Fatalf("the row badge should show the live level, got %q", state)
	}
}
