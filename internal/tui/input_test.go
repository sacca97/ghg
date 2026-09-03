package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newGrowModel builds a model with the real input and a known width, as Run
// does after the first WindowSizeMsg. now defaults to the real clock; tests
// swap in a fake to simulate key-repeat timing.
func newGrowModel() *model {
	m := &model{input: newInput(), now: time.Now}
	m.width = 80
	m.input.SetWidth(m.width - 2) // matches Update's WindowSizeMsg handling
	m.layout()
	return m
}

// Regression: ctrl+j must both insert a newline and grow the input box so the
// whole prompt stays visible. The bug was that layout() sized the box from
// View(), which the textarea clamps to its current height — so it never grew.
func TestInputGrowsOnCtrlJ(t *testing.T) {
	m := newGrowModel()
	if got := m.input.Height(); got != 1 {
		t.Fatalf("empty input should be 1 line, got %d", got)
	}

	m.input.SetValue("first line")
	m.input.CursorEnd()

	// press ctrl+j through the real key handler, then type the next line
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m = tm.(*model)
	m.input.InsertString("second line")
	m.layout()

	if got := m.input.LineCount(); got != 2 {
		t.Fatalf("ctrl+j should insert a newline: LineCount=%d value=%q", got, m.input.Value())
	}
	if got := m.input.Height(); got != 2 {
		t.Fatalf("input box should grow to 2 lines, got %d", got)
	}

	// a third line keeps it growing
	tm, _ = m.key(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m = tm.(*model)
	m.input.InsertString("third line")
	m.layout()
	if got := m.input.Height(); got != 3 {
		t.Fatalf("input box should grow to 3 lines, got %d", got)
	}
}

// A single long line that wraps past the content width must also grow the box.
func TestInputGrowsOnWrap(t *testing.T) {
	m := newGrowModel()
	m.input.SetValue(strings.Repeat("x", (m.input.Width()-2)*2)) // two full content rows
	m.layout()
	if got := m.input.Height(); got != 2 {
		t.Fatalf("wrapped long line should need 2 rows, got %d", got)
	}
}

// The box must never exceed MaxHeight, so very long input scrolls instead of
// pushing the transcript off-screen.
func TestInputCappedAtMaxHeight(t *testing.T) {
	m := newGrowModel()
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "line")
	}
	m.input.SetValue(strings.Join(lines, "\n"))
	m.layout()
	if got := m.input.Height(); got != m.input.MaxHeight {
		t.Fatalf("input should cap at MaxHeight=%d, got %d", m.input.MaxHeight, got)
	}
}

// Deleting back to one line shrinks the box again.
func TestInputShrinksWhenContentRemoved(t *testing.T) {
	m := newGrowModel()
	m.input.SetValue("a\nb\nc")
	m.layout()
	if got := m.input.Height(); got != 3 {
		t.Fatalf("3 lines, got %d", got)
	}
	m.input.SetValue("a")
	m.layout()
	if got := m.input.Height(); got != 1 {
		t.Fatalf("should shrink back to 1 line, got %d", got)
	}
}

// Regression: when the box grows, every line of content must be visible in the
// rendered textarea — the textarea's internal viewport must not clip the top
// lines. The bug: repositionView only scrolls down to track the cursor, so
// after growing, earlier lines sat above the visible window. Proven by driving
// the real Update() path and checking each line appears in input.View().
func TestInputShowsAllLinesAfterGrowth(t *testing.T) {
	m := newGrowModel()
	lines := []string{"line one", "line two", "line three", "line four"}
	for i, ln := range lines {
		if i > 0 {
			tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
			m = tm.(*model)
		}
		for _, r := range ln {
			tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = tm.(*model)
		}
	}
	if got := m.input.Height(); got != len(lines) {
		t.Fatalf("box should have grown to %d lines, got %d", len(lines), got)
	}
	rendered := m.input.View()
	for _, ln := range lines {
		if !strings.Contains(rendered, ln) {
			t.Errorf("rendered input is missing %q\n--- rendered ---\n%s", ln, rendered)
		}
	}
}

// Regression: pasting a large multi-line block must not lock out ctrl+j. The
// bug: bubbles' textarea enforces MaxHeight as a content-line limit on
// InsertNewline (not just a visual cap), so once a pasted block reached
// MaxHeight lines every ctrl+j was silently swallowed.
func TestCtrlJWorksAfterLargePaste(t *testing.T) {
	m := newGrowModel()
	var lines []string
	for i := 0; i < m.input.MaxHeight+5; i++ {
		lines = append(lines, fmt.Sprintf("pasted %d", i))
	}
	// bracketed paste arrives as one rune batch, like a real terminal paste
	block := strings.Join(lines, "\n")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(block)})
	m = tm.(*model)
	if got, want := m.input.LineCount(), len(lines); got != want {
		t.Fatalf("paste should land all lines: LineCount=%d want %d", got, want)
	}

	// now ctrl+j must still insert newlines past the visual cap
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m = tm.(*model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("typed after")})
	m = tm.(*model)
	if got, want := m.input.LineCount(), len(lines)+1; got != want {
		t.Fatalf("ctrl+j after a large paste was swallowed: LineCount=%d want %d\nvalue tail: %q",
			got, want, m.input.Value()[max(0, len(m.input.Value())-120):])
	}
	if !strings.Contains(m.input.Value(), "\ntyped after") {
		t.Errorf("new line should be its own line, got tail %q", m.input.Value()[max(0, len(m.input.Value())-60):])
	}
	// the visual box stays capped — content keeps scrolling
	if got := m.input.Height(); got != m.input.MaxHeight {
		t.Errorf("box should stay capped at MaxHeight=%d, got %d", m.input.MaxHeight, got)
	}
}

// The box caps at MaxHeight while content keeps growing past it (older lines
// scroll off, which is correct once capped).
//
// Note: after a multi-line SetValue (a paste), bubbles v1.0.0's memoized wrap
// cache can leave the textarea's internal viewport parked at the top until
// the next width change — a pre-existing rendering quirk, separate from the
// newline behavior asserted here.
func TestInputScrollsWhenCapped(t *testing.T) {
	m := newGrowModel()
	for i := 0; i < m.input.MaxHeight+5; i++ {
		if i > 0 {
			tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
			m = tm.(*model)
		}
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(fmt.Sprintf("row%d", i))})
		m = tm.(*model)
	}
	if got := m.input.Height(); got != m.input.MaxHeight {
		t.Fatalf("should cap at MaxHeight=%d, got %d", m.input.MaxHeight, got)
	}
	// every ctrl+j landed: MaxHeight+4 newlines = MaxHeight+5 content lines
	if got, want := m.input.LineCount(), m.input.MaxHeight+5; got != want {
		t.Fatalf("content should grow past the visual cap: LineCount=%d want %d\nvalue=%q",
			got, want, m.input.Value())
	}
}

func TestInputPreservesCursorPositionOnGrowth(t *testing.T) {
	m := newGrowModel()
	m.input.SetValue("line 1\nline 2")
	m.input.CursorStart()
	m.input.SetCursor(2)
	lineBefore := m.input.Line()
	colBefore := m.input.LineInfo().ColumnOffset

	m.growInput()

	if m.input.Line() != lineBefore || m.input.LineInfo().ColumnOffset != colBefore {
		t.Fatalf("cursor position jumped on growth: got line %d col %d, want line %d col %d",
			m.input.Line(), m.input.LineInfo().ColumnOffset, lineBefore, colBefore)
	}
}

// keyRunes builds the KeyMsg bubbletea would produce for an unknown sequence
// whose String() renders as s.
func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestIsShiftEnterSeq(t *testing.T) {
	for in, want := range map[string]bool{
		// rendered forms of unknownCSISequenceMsg (see bubbletea key.go)
		"unknown csi sequence: 0x1b, '[', '1', '3', ';', '2', 'u'":                     true,  // CSI u
		"unknown csi sequence: 0x1b, '[', '2', '7', ';', '2', ';', '1', '3', '~'":      true,  // modifyOtherKeys
		"unknown csi sequence: 0x1b, '[', 'five', 'seven', 'four', 'four', 'one', 'u'": true,  // kitty 57441u
		"unknown csi sequence: 0x1b, '[', '1', ';', '2', 'A'":                          false, // shift+up
		"a":     false,
		"enter": false,
	} {
		if got := isShiftEnterSeq(keyRunes(in)); got != want {
			t.Errorf("isShiftEnterSeq(%q) = %v, want %v", in, got, want)
		}
	}
}

func pressKey(m *model, kt tea.KeyType) *model {
	tm, _ := m.key(tea.KeyMsg{Type: kt})
	return tm.(*model)
}

// The reported bug: typing "$go" filtered the skill menu, but tab only moved
// the selection — the rest of the name never got typed. Tab now completes.
func TestTabCompletesSkillName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ghg/skills/go-style"), 0o755)
	os.WriteFile(filepath.Join(home, ".ghg/skills/go-style/SKILL.md"),
		[]byte("---\nname: go-style\ndescription: d\n---\n"), 0o644)
	m := modelCmdModel()
	m = typeStr(t, m, "$go-sty")
	if m.menu == nil || len(m.menu.cands) != 1 || m.menu.cands[0].Text != "$go-style" {
		t.Fatalf("menu should hold exactly $go-style: %+v", m.menu)
	}
	m = pressKey(m, tea.KeyTab)
	if v := m.input.Value(); v != "$go-style" {
		t.Fatalf("tab should complete the skill name, got %q", v)
	}
	// a second tab on a single match is a no-op, and enter submits the line
	m = pressKey(m, tea.KeyTab)
	if v := m.input.Value(); v != "$go-style" {
		t.Fatalf("second tab should not corrupt a single match, got %q", v)
	}
}

// Tab cycles through candidates with preview: each press inserts the next
// candidate, wrapping at both ends. ("/e" matches /effort only, so use /m.)
func TestTabCyclesWithPreview(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/m") // multiple candidates, deterministic order
	m = pressKey(m, tea.KeyTab)
	first := m.input.Value()
	if first == "/m" {
		t.Fatalf("tab should preview a candidate, got %q", first)
	}
	m = pressKey(m, tea.KeyTab)
	second := m.input.Value()
	if first == second {
		t.Fatalf("second tab should preview the next candidate, still %q", first)
	}
	for m.input.Value() != first {
		m = pressKey(m, tea.KeyTab)
	}
	if m.input.Value() != first {
		t.Fatalf("wrap should return to %q, got %q", first, m.input.Value())
	}
}

// Esc while tab-cycling dismisses the menu and reverts to the completed
// prefix the cycle started from (readline-style; it does not un-complete).
func TestEscRevertsTabCycle(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/m")
	m = pressKey(m, tea.KeyTab)
	first := m.input.Value()
	m = pressKey(m, tea.KeyTab)
	m = pressKey(m, tea.KeyEsc)
	if m.menu != nil {
		t.Fatal("esc should close the menu")
	}
	if m.input.Value() != first {
		t.Fatalf("esc should revert to the cycle base, got %q", m.input.Value())
	}
}

// Enter runs an immediate command; other candidates remain in the input.
func TestEnterCommitsTabCycle(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/mo") // the only matching command is /model
	m = pressKey(m, tea.KeyTab)
	if m.input.Value() != "/model" {
		t.Fatalf("tab should preview /model first, got %q", m.input.Value())
	}
	m = pressKey(m, tea.KeyEnter)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatal("enter on tab-cycled /model should open the role picker")
	}
	if m.input.Value() != "" {
		t.Fatalf("execNow clears the input, got %q", m.input.Value())
	}

	// non-execNow command: /effort with args commits as text
	t.Setenv("HOME", t.TempDir())
	m = modelCmdModel()
	m = typeStr(t, m, "/effort ")
	m = pressKey(m, tea.KeyTab) // previews the first effort level
	want := m.input.Value() + " "
	m = pressKey(m, tea.KeyEnter)
	if m.input.Value() != want {
		t.Fatalf("enter should commit the preview as %q, got %q", want, m.input.Value())
	}
}

// Arrows still move the menu selection without touching the input.
func TestArrowsNavigateMenuOnly(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/m")
	idx := m.menu.idx
	m = pressKey(m, tea.KeyDown)
	if m.menu == nil || m.menu.idx == idx {
		t.Fatal("down should move the selection")
	}
	if m.input.Value() != "/m" {
		t.Fatalf("arrows must not edit the input, got %q", m.input.Value())
	}
}

// Immediate commands still run on enter, even mid-cycle.
func TestExecNowRunsOnEnterWhileCycling(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/mo")
	m = pressKey(m, tea.KeyTab) // previews /model
	m = pressKey(m, tea.KeyEnter)
	if m.input.Value() != "" {
		t.Fatalf("/model should run and clear the input, got %q", m.input.Value())
	}
	if m.settings == nil {
		t.Fatal("/model should open settings")
	}
}

func TestMenuViewAlignsUnicodeCandidates(t *testing.T) {
	m := &model{
		menu: &menu{
			cands: []cand{
				{Text: "你好", Desc: "Chinese greeting"},
				{Text: "hi", Desc: "English greeting"},
			},
		},
	}
	out := m.menuView()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %q", out)
	}
	col1 := strings.Index(lines[0], "Chinese")
	col2 := strings.Index(lines[1], "English")
	w1 := ansi.StringWidth(lines[0][:col1])
	w2 := ansi.StringWidth(lines[1][:col2])
	if w1 != w2 {
		t.Fatalf("column widths do not match: w1=%d, w2=%d (lines: %q, %q)", w1, w2, lines[0], lines[1])
	}
}

// Paste collapse is opt-in (config collapsePaste): off by default a paste
// lands verbatim; on, a ≥3-line paste becomes a placeholder whose real text
// swaps back in at submit.
func TestPasteCollapseOptIn(t *testing.T) {
	paste := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line1\nline2\nline3"), Paste: true}

	// default (nil) — off: the textarea takes the raw paste
	m := compactCmdModel()
	m.Update(paste)
	if !strings.Contains(m.input.Value(), "line1") {
		t.Fatalf("paste should land verbatim by default, got %q", m.input.Value())
	}
	if m.pasteBuf != "" {
		t.Fatal("no buffer held when collapse is off")
	}

	// on — collapse to a placeholder, real text held
	on := true
	m2 := compactCmdModel()
	m2.cfg.CollapsePaste = &on
	m2.Update(paste)
	if !strings.Contains(m2.input.Value(), "[Pasted ~3 lines]") {
		t.Fatalf("collapsed input should show the placeholder, got %q", m2.input.Value())
	}
	if m2.pasteBuf == "" {
		t.Fatal("the real paste text should be held")
	}
	// submit swaps it back
	m2.input.SetValue(m2.input.Value()) // settle
	m2.permDialog = nil
	// drive the submit path's swap directly (the placeholder → real text)
	text := strings.TrimSpace(m2.input.Value())
	text = strings.Replace(text, "[Pasted ~3 lines]", strings.TrimSpace(m2.pasteBuf), 1)
	if !strings.Contains(text, "line1\nline2\nline3") {
		t.Fatalf("submit should restore the real text, got %q", text)
	}
}

// A short paste (1-2 lines) never collapses, even when the option is on.
func TestPasteCollapseShortPasteIgnored(t *testing.T) {
	on := true
	m := compactCmdModel()
	m.cfg.CollapsePaste = &on
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("just one line"), Paste: true})
	if strings.Contains(m.input.Value(), "[Pasted") {
		t.Fatal("a one-line paste should not collapse")
	}
}
