package tui

import (
	"strings"
	"testing"
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
