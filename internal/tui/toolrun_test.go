package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A tool row renders the tool name and full arguments. On completion, it retains
// the name and arguments without leaking stdout/result into the viewport.
func TestToolRowDetailsOnly(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))

	m.Update(toolStartMsg{id: "c1", name: "read", args: `{"path":"internal/session/session.go","offset":700,"limit":100}`})
	if len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].kind != blockToolRun {
		t.Fatal("toolStart should append a running row")
	}
	row := m.blocks[len(m.blocks)-1]
	if !row.toolRunning {
		t.Fatal("row should be running")
	}
	got := ansi.Strip(row.render(m.width))
	if !strings.Contains(got, "⚒ read") || !strings.Contains(got, `{"path":"internal/session/session.go","offset":700,"limit":100}`) {
		t.Fatalf("running row should show tool name and full args, got %q", got)
	}

	m.Update(toolEndMsg{id: "c1", name: "read", result: "file body sensitive content\nline2\nline3"})
	row = m.blocks[len(m.blocks)-1]
	if row.toolRunning {
		t.Fatal("completion should stop the run state")
	}
	got = ansi.Strip(row.render(m.width))
	if !strings.Contains(got, "⚒ read") || !strings.Contains(got, `{"path":"internal/session/session.go","offset":700,"limit":100}`) {
		t.Fatalf("completed row should retain tool name and arguments, got %q", got)
	}
	if strings.Contains(got, "file body") || strings.Contains(got, "sensitive") {
		t.Fatalf("completed row must not leak tool output into viewport, got %q", got)
	}
}

// A failed tool row appends "— failed" without showing error bodies or stderr.
func TestToolRowFailureHidesErrorBody(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))
	m.Update(toolStartMsg{id: "c1", name: "bash", args: `{"command":"go test ./..."}`})
	m.Update(toolEndMsg{id: "c1", name: "bash", result: "Error: exit status 1\nsensitive stderr"})

	var run *block
	for i := range m.blocks {
		if m.blocks[i].kind == blockToolRun {
			run = &m.blocks[i]
		}
	}
	if run == nil || !run.toolFailed {
		t.Fatal("a failed tool should mark the row failed")
	}
	got := ansi.Strip(run.render(m.width))
	if !strings.Contains(got, "— failed") {
		t.Fatalf("failed row should contain '— failed', got %q", got)
	}
	if !strings.Contains(got, `{"command":"go test ./..."}`) {
		t.Fatalf("failed row should retain command arguments, got %q", got)
	}
	if strings.Contains(got, "exit status 1") || strings.Contains(got, "sensitive stderr") {
		t.Fatalf("failed row must not display error body or stderr, got %q", got)
	}
}
