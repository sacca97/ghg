package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// shift+mouse must pass through unconsumed so the terminal's native
// selection (drag-to-copy) works while capture is on.
func TestShiftMousePassesThrough(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	m.appendRaw(blockTool, "line1\nline2")
	m.refreshVP()
	before := m.blocks[0].expanded
	// shift+click on the tool block must NOT expand it (native selection owns it)
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Shift: true, X: 5, Y: m.blocks[0].y0 + m.contentPad() - m.vp.YOffset + 2})
	m = tm.(*model)
	if m.blocks[0].expanded != before {
		t.Fatal("shift+click must not toggle the block — it belongs to native selection")
	}
	// plain click still toggles
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 5, Y: m.blocks[0].y0 + m.contentPad() - m.vp.YOffset + 2})
	m = tm.(*model)
	if m.blocks[0].expanded == before {
		t.Fatal("plain click should toggle the block")
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
