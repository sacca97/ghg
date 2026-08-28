package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/sacca97/ghg/internal/llm"
)

// frameModel is a model with a transcript long enough to fill the fixed-height
// viewport, making any undercounted `chrome` visible in the rendered frame.
func frameModel() *model {
	m := compactCmdModel()
	m.queueSel = -1
	m.width, m.height = 100, 30
	for i := range 80 {
		m.agent.Messages = append(m.agent.Messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("answer %d padded out a bit", i)})
	}
	m.seedTranscript(m.agent.Messages, 0)
	return m
}

// The rendered frame must never be taller than the terminal. A frame one row
// too tall makes the terminal scroll on every repaint. The visible symptom is
// not "the layout is off" — it is "the mouse wheel is broken", because the
// wheel scrolls the viewport while the repaint shoves the whole frame the
// other way.
//
// layout() computes the viewport height as m.height - chrome, so this is
// really a test that chrome counts every row View() spends outside the
// viewport. It caught chrome missing the trailing blank + three-row status box (+4 in
// every state) and the armed-hint row (+1 more).
func TestFrameNeverExceedsTerminalHeight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *model)
	}{
		{"idle", func(m *model) {}},
		{"busy", func(m *model) { m.busy = true }},
		{"queued messages", func(m *model) { m.queue = []string{"one", "two"} }},
		{"busy with queue", func(m *model) { m.busy = true; m.queue = []string{"one"} }},
		{"multiline input", func(m *model) { m.input.SetValue("a\nb\nc") }},
		{"quit armed", func(m *model) { m.quit1 = true }},
		{"esc armed", func(m *model) { m.esc1 = true }},
		{"clear armed", func(m *model) { m.escClr = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := frameModel()
			tc.setup(m)
			m.layout()
			if h := lipgloss.Height(m.View()); h > m.height {
				t.Errorf("frame is %d rows in a %d-row terminal (%+d): layout()'s chrome "+
					"undercounts View()'s non-viewport rows, so the terminal scrolls on "+
					"every repaint and the wheel fights it", h, m.height, h-m.height)
			}
		})
	}
}

// A narrow or not-yet-sized terminal must not produce a taller frame either:
// width 0 happens before the first WindowSizeMsg.
//
// The floor is the fixed chrome itself (header, tips, divider, input, status —
// 9 rows with a one-line input): below that nothing can fit and the frame
// necessarily overflows, so those sizes are out of scope rather than a bug.
func TestFrameFitsAtDegenerateSizes(t *testing.T) {
	for _, size := range [][2]int{{0, 0}, {10, 10}, {20, 12}, {400, 60}} {
		m := frameModel()
		m.width, m.height = size[0], size[1]
		m.layout()
		if h := lipgloss.Height(m.View()); m.height > 0 && h > m.height {
			t.Errorf("%dx%d: frame %d rows exceeds height %d", size[0], size[1], h, m.height)
		}
	}
}

// The divider sits directly above the input box so the thing you type into is
// visually separate from the thing you read, and it replaces a blank line
// rather than adding a row (see layout()'s chrome).
func TestInputRuleSeparatesInputFromTranscript(t *testing.T) {
	m := frameModel()
	m.input.SetValue("typing")
	m.layout()
	lines := strings.Split(ansi.Strip(m.View()), "\n")

	rule := -1
	for i, ln := range lines {
		if s := strings.TrimSpace(ln); s != "" && strings.Trim(s, "─") == "" {
			rule = i
		}
	}
	if rule < 0 {
		t.Fatalf("no divider row in the frame:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[rule+1], "typing") {
		t.Errorf("divider should sit directly above the input, got %q then %q",
			lines[rule], lines[rule+1])
	}
}

// An interactive bash command hides the input box; a divider with nothing
// under it would read as a stray line, so that case keeps the blank row.
func TestInputRuleHiddenWhileInteractive(t *testing.T) {
	m := frameModel()
	if m.inputRule() == "" {
		t.Fatal("expected a divider while the input box is shown")
	}
	m.iactive = &interactive{}
	if got := m.inputRule(); got != "" {
		t.Errorf("interactive bash hides the input box, so the divider should be blank, got %q", got)
	}
}
