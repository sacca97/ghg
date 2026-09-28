package tui

import (
	"encoding/json"
	"strings"
	"testing"

	workerwire "github.com/sacca97/ghg/internal/worker"
)

func TestAskCommandSendsReadOnlyWorkerTurn(t *testing.T) {
	m := compactCmdModel()
	frames := workerTestFrames(t, m, "test-ask")

	m.command("/ask what is the answer?")
	frame := nextWorkerFrame(t, frames)
	var command workerwire.CommandRequest
	if err := json.Unmarshal(frame.Payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Name != workerwire.CommandInput {
		t.Fatalf("command = %q, want input", command.Name)
	}
	var input workerwire.Input
	if err := json.Unmarshal(command.Payload, &input); err != nil {
		t.Fatal(err)
	}
	if input.Input != "what is the answer?" {
		t.Fatalf("input = %q", input.Input)
	}
	if !input.AskMode || input.PlanMode || input.ReviewMode {
		t.Fatalf("unexpected mode flags: %+v", input)
	}
}

func TestContinueCommandSendsOptionalInstruction(t *testing.T) {
	m := compactCmdModel()
	frames := workerTestFrames(t, m, "test-continue")

	m.command("/continue run the tests")
	frame := nextWorkerFrame(t, frames)
	var command workerwire.CommandRequest
	if err := json.Unmarshal(frame.Payload, &command); err != nil {
		t.Fatal(err)
	}
	var input workerwire.Input
	if err := json.Unmarshal(command.Payload, &input); err != nil {
		t.Fatal(err)
	}
	if command.Name != workerwire.CommandInput || input.Input != "run the tests" || !input.Continue {
		t.Fatalf("unexpected continue input: command=%q input=%+v", command.Name, input)
	}
}

func TestDetachCommandStopsWorkerBeforeExit(t *testing.T) {
	m := compactCmdModel()
	frames := workerTestFrames(t, m, "test-stop")
	m.workerState = workerwire.StateRunning
	m.workerLiveWork = true

	m.command("/detach")
	frame := nextWorkerFrame(t, frames)
	var command workerwire.CommandRequest
	if err := json.Unmarshal(frame.Payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Name != workerwire.CommandStop {
		t.Fatalf("detach command = %q, want %q", command.Name, workerwire.CommandStop)
	}
	if m.workerStopRequestID == "" {
		t.Fatal("stop request should remain pending until the worker acknowledges it")
	}
}

func TestWorkerLSPStatusAckRendersAllStates(t *testing.T) {
	m := compactCmdModel()
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

func TestWorkerLSPAndMCPCommands(t *testing.T) {
	m := compactCmdModel()
	m.sessionID = "test-worker-integrations"
	frames := workerTestFrames(t, m, "test-worker-integrations")

	// LSP status
	m.lspCommand(nil)
	frame := nextWorkerFrame(t, frames)
	if frame.Type != workerwire.TypeCommand {
		t.Fatalf("frame type = %q, want %q", frame.Type, workerwire.TypeCommand)
	}
	var cmdReq workerwire.CommandRequest
	_ = json.Unmarshal(frame.Payload, &cmdReq)
	if cmdReq.Name != workerwire.CommandLSPStatus {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandLSPStatus)
	}

	// MCP status
	m.mcpCommand([]string{"/mcp"})
	frame = nextWorkerFrame(t, frames)
	_ = json.Unmarshal(frame.Payload, &cmdReq)
	if cmdReq.Name != workerwire.CommandMCPStatus {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandMCPStatus)
	}

	// MCP reconnect
	m.mcpCommand([]string{"/mcp", "myserver", "reconnect"})
	frame = nextWorkerFrame(t, frames)
	_ = json.Unmarshal(frame.Payload, &cmdReq)
	if cmdReq.Name != workerwire.CommandMCPReconnect {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandMCPReconnect)
	}
}

// Worker-owned TUI commands share the canonical worker adapter. Without a
// worker, the adapter reports one actionable note instead of sending.
func TestWorkerOwnedCommandUsesCanonicalAdapter(t *testing.T) {
	m := compactCmdModel()
	m.sessionID = "test-adapter"
	frames := workerTestFrames(t, m, "test-adapter")
	m.command("/notify on")
	frame := nextWorkerFrame(t, frames)
	if !strings.HasPrefix(frame.RequestID, "notify-") {
		t.Fatalf("request id = %q, want a notify- prefix", frame.RequestID)
	}
	var req workerwire.CommandRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != workerwire.CommandNotify {
		t.Fatalf("command = %q, want %q", req.Name, workerwire.CommandNotify)
	}
	var payload workerwire.NotifyRequest
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Action != "on" {
		t.Fatalf("notify action = %q, want %q", payload.Action, "on")
	}

	cold := compactCmdModel()
	before := len(cold.blocks)
	cold.command("/lsp")
	last := cold.blocks[len(cold.blocks)-1].text
	if len(cold.blocks) != before+1 || !strings.Contains(last, "worker unavailable") {
		t.Fatalf("expected one worker-unavailable note, got %q", last)
	}
}
