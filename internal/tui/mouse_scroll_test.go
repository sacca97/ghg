package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

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
