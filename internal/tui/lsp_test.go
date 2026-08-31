package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/lsp"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// Headless /lsp command coverage: status rows for configured servers.

func TestLSPCommandNoManager(t *testing.T) {
	m := tasksModel("http://unused")
	m.lspMgr = nil
	m.command("/lsp")
	last := m.blocks[len(m.blocks)-1].text
	if !strings.Contains(last, "no LSP servers configured") {
		t.Errorf("got %q", last)
	}
}

func TestLSPCommandStatusRows(t *testing.T) {
	m := tasksModel("http://unused")
	m.lspMgr = lsp.NewManager(map[string]lsp.ServerSpec{
		"gopls":  {Command: []string{"gopls"}, Extensions: []string{".go"}},
		"zz-lsp": {Command: []string{"zz"}, Extensions: []string{".zz"}},
	})
	m.command("/lsp")
	last := m.blocks[len(m.blocks)-1].text
	if !strings.Contains(last, "LSP servers:") {
		t.Fatalf("got %q", last)
	}
	for _, want := range []string{"gopls", "zz-lsp", "idle — starts on first matching file"} {
		if !strings.Contains(last, want) {
			t.Errorf("status view missing %q: %q", want, last)
		}
	}
}

func TestWorkerLSPStatusAckRendersAllStates(t *testing.T) {
	m := tasksModel("http://unused")
	payload, err := json.Marshal([]workerwire.LSPStatus{
		{Name: "gopls", Root: "/workspace", State: "connected"},
		{Name: "typescript", State: "not started"},
		{Name: "rust-analyzer", State: "failed", Error: "rust-analyzer not on PATH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.handleWorkerFrame(workerwire.Frame{Type: workerwire.TypeAck, RequestID: "lsp-1", Payload: payload})
	last := m.blocks[len(m.blocks)-1].text
	for _, want := range []string{
		"● gopls",
		"connected (root: /workspace)",
		"○ typescript",
		"idle — starts on first matching file",
		"✗ rust-analyzer",
		"rust-analyzer not on PATH",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("worker status view missing %q: %q", want, last)
		}
	}
}
