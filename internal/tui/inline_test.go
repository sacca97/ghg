package tui

import (
	"strings"
	"testing"
	"time"
)

// The view contains only application content; terminal control sequences are
// owned by Run, which uses Bubble Tea's alternate-screen lifecycle. Mouse
// capture remains ON by default for wheel scroll and clicks.
func TestViewRendersTranscript(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	m.appendAssistant("hello **world**")
	v := m.View()
	if strings.Contains(v, "\x1b[?1049h") || strings.Contains(v, "\x1b[?47h") {
		t.Fatal("view must not enter the alternate screen")
	}
	for _, want := range []string{"ghg", "hello", "world"} {
		if !strings.Contains(stripAll(v), want) {
			t.Errorf("inline view missing %q", want)
		}
	}
	// mouse capture on by default (wheel scroll and clicks)
	if !m.mouseOn {
		t.Fatal("mouse capture must default on for wheel scroll")
	}
}

func stripAll(s string) string {
	out := strings.Builder{}
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' && s[i] != 'h' && s[i] != 'l' {
				i++
			}
			i++
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func TestThinkingDisplayEphemeralAndCollapsedTranscript(t *testing.T) {
	m := compactCmdModel()
	m.showThinking = true
	m.Update(mkWinSize(80, 20))

	t0 := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	currTime := t0
	m.now = func() time.Time { return currTime }

	// Stream multiple lines of reasoning containing sensitive text
	um, _ := m.Update(thinkMsg("secret reasoning tokens\ninternal model deliberation"))
	m = um.(*model)

	// Advance clock by 3.2s
	currTime = t0.Add(3200 * time.Millisecond)

	// Live thinkView should only show the timer, never raw reasoning tokens
	tv := stripAll(m.thinkView())
	if tv != "◌ Thinking 3.2s" {
		t.Fatalf("live thinkView should be timer '◌ Thinking 3.2s', got %q", tv)
	}
	if strings.Contains(tv, "secret") || strings.Contains(tv, "deliberation") {
		t.Fatalf("live thinkView leaked reasoning tokens: %q", tv)
	}

	// Tool starts: finalized timer precedes tool-call row
	um, _ = m.Update(toolStartMsg{id: "c1", name: "grep", args: `{"pattern":"secretKey","path":"."}`})
	m = um.(*model)

	if len(m.blocks) < 2 {
		t.Fatalf("expected at least 2 blocks (timer + tool), got %d", len(m.blocks))
	}
	timerBlock := stripAll(m.blocks[len(m.blocks)-2].text)
	if timerBlock != "◌ Thinking 3.2s" {
		t.Fatalf("expected finalized timer block '◌ Thinking 3.2s', got %q", timerBlock)
	}
	toolBlock := stripAll(m.blocks[len(m.blocks)-1].text)
	if !strings.Contains(toolBlock, "⚒ grep") || !strings.Contains(toolBlock, `{"pattern":"secretKey","path":"."}`) {
		t.Fatalf("expected tool row with name and full args, got %q", toolBlock)
	}

	// Send tool result
	um, _ = m.Update(toolEndMsg{id: "c1", name: "grep", result: "secret_file.go: secretKey = 12345"})
	m = um.(*model)

	// Verify tool result output never appears in rendered TUI blocks
	for _, b := range m.blocks {
		bText := stripAll(b.render(m.width))
		if strings.Contains(bText, "secretKey = 12345") {
			t.Fatalf("tool result output leaked into transcript block: %q", bText)
		}
	}

	// Tool name and full args remain visible after completion
	completedRow := stripAll(m.blocks[len(m.blocks)-1].render(m.width))
	if !strings.Contains(completedRow, "⚒ grep") || !strings.Contains(completedRow, `{"pattern":"secretKey","path":"."}`) {
		t.Fatalf("tool name and arguments must remain visible after completion, got %q", completedRow)
	}

	// When thinking display is disabled, no timer line is appended
	m2 := compactCmdModel()
	m2.showThinking = false
	m2.Update(mkWinSize(80, 20))
	um2, _ := m2.Update(thinkMsg("invisible reasoning"))
	m2 = um2.(*model)
	um2, _ = m2.Update(textMsg("Answer with thinking off"))
	m2 = um2.(*model)
	for _, b := range m2.blocks {
		stripped := stripAll(b.text)
		if strings.Contains(stripped, "Thinking") || strings.Contains(stripped, "invisible reasoning") {
			t.Fatalf("disabled thinking should not add thought line to transcript: %q", stripped)
		}
	}
}
