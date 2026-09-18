package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"os"
	"testing"
)

func TestCommandRuleArity(t *testing.T) {
	cases := map[string]string{
		"git checkout main":             "git checkout",
		"git commit -m 'x'":             "git commit",
		"npm run dev":                   "npm run dev",
		"npm install lodash":            "npm install",
		"ls -la /tmp":                   "ls",
		"rm -rf build":                  "rm",
		"FOO=bar go test ./...":         "go test",
		"docker compose up":             "docker compose up",
		"git checkout main && rm -rf /": "git checkout main && rm -rf /", // compound commands use an exact rule
		"somescript.sh --flag":          "somescript.sh",
	}
	for in, want := range cases {
		if got := tools.CommandRule(in); got != want {
			t.Errorf("CommandRule(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPermissionDialogStaysOpenWhenWorkerDisconnects(t *testing.T) {
	m := compactCmdModel()
	m.permDialog = &permDialog{
		req:      tools.GateRequest{Tool: "bash", Command: "git status", Rule: "git status"},
		workerID: "approval-1",
	}

	m.permKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.permDialog == nil {
		t.Fatal("approval dialog was dismissed without a worker connection")
	}
}

func TestStalePermissionDialogClearsAfterReattach(t *testing.T) {
	m := compactCmdModel()
	m.permDialog = &permDialog{workerID: "approval-1"}

	m.applyWorkerSnapshot(workerwire.Snapshot{State: workerwire.StateIdle})
	if m.permDialog != nil {
		t.Fatal("stale approval dialog survived a reattach without a pending request")
	}
}

// The startup gate: a trusted cwd passes without a prompt; an untrusted one
// declines when there's no terminal to ask on.
func TestTrustGate(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	wd, _ := os.Getwd()
	if err := config.Trust(wd); err != nil {
		t.Fatal(err)
	}
	ok, err := config.CheckTrust(wd)
	if err != nil || !ok {
		t.Fatalf("trusted cwd should pass: %v %v", ok, err)
	}
}
