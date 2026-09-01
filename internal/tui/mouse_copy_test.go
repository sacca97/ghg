package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

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

// Changing the theme updates rendering and config without adding a routine
// confirmation block to the transcript.
func TestThemeChangeDoesNotAppendTranscriptNote(t *testing.T) {
	m := compactCmdModel()
	m.setTheme("light")
	m.command("/theme auto")
	if len(m.blocks) != 0 {
		t.Fatalf("theme changes should not append routine notes, got %v", m.blocks)
	}
}
