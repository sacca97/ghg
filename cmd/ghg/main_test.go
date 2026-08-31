package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// The system prompt always carries the built-in operating rules (the safety
// rails); ~/.ghg/me.md appends the user's standing instructions after them.
func TestSystemPromptAppendsUserMe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)

	p := systemPrompt()
	if !strings.Contains(p, "never force-push") {
		t.Fatal("built-in operating rules must always be present")
	}
	if !strings.Contains(p, "verify it from the relevant source instead of guessing") {
		t.Fatal("embedded prompt must include the verification rule")
	}
	if strings.Contains(p, "Standing instructions") {
		t.Fatal("a fresh install (all-comments me.md) appends nothing")
	}

	os.WriteFile(filepath.Join(home, "me.md"), []byte("- Always pnpm, never npm.\n"), 0o644)
	p = systemPrompt()
	if !strings.Contains(p, "never force-push") {
		t.Fatal("built-in rules survive a user me.md")
	}
	if !strings.Contains(p, "Standing instructions from the user") || !strings.Contains(p, "Always pnpm") {
		t.Fatalf("user instructions should append:\n%s", p)
	}
}

func TestSystemPromptAppendsTrustedProjectInstructions(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("GHG_HOME", home)
	t.Chdir(root)
	if err := config.Trust(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "me.md"), []byte("prefer task test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("run task check\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := systemPrompt()
	if !strings.Contains(p, "<project_instructions>") || !strings.Contains(p, "run task check") {
		t.Fatalf("trusted AGENTS.md should be in the system prompt:\n%s", p)
	}
	base := strings.Index(p, "You are an expert coding assistant")
	cwd := strings.Index(p, "Current working directory: "+root)
	me := strings.Index(p, "Standing instructions from the user")
	project := strings.Index(p, "<project_instructions>")
	if base < 0 || cwd < base || me < cwd || project < me {
		t.Fatalf("prompt blocks out of order: base=%d cwd=%d me=%d project=%d", base, cwd, me, project)
	}

	if got := systemPromptForProject(false); strings.Contains(got, "run task check") {
		t.Fatal("untrusted project instructions must not be added")
	}
}

func TestSystemPromptPrefersBoundedExplorationTools(t *testing.T) {
	prompt := systemPrompt()
	for _, fragment := range []string{"Prefer grep for text", "glob for exact paths", "find_files for fuzzy paths", "read with offset/limit", "Reserve bash for builds, tests, git"} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("system prompt lacks %q", fragment)
		}
	}
}

func TestContinueSessionIDUsesCurrentDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("GHG_HOME", home)
	t.Chdir(root)
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Create(root, "model", "provider")
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Save(id, 1, []llm.Message{{Role: "system"}, {Role: "user", Content: "continue me"}}, "model", "provider"); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := continueSessionID()
	if err != nil || got != id {
		t.Fatalf("continue session: %q, %v", got, err)
	}
}

func TestWorkerEventOrderTerminalBeforeIdle(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "ghg-w-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(baseDir)

	runtimeFile, err := workerwire.NewRuntime(baseDir, "s1")
	if err != nil {
		t.Fatal(err)
	}

	var events []string
	var eventsMu sync.Mutex

	w := &workerProcessState{
		runtimeFile: runtimeFile,
		sessionID:   "s1",
		state:       workerwire.StateIdle,
		pending:     make(map[string]*workerApprovalFlight),
		done:        make(chan struct{}),
	}
	w.ag = agent.New(nil, "m", 100, "sys")

	server, err := workerwire.NewServer(runtimeFile, w)
	if err != nil {
		t.Fatal(err)
	}
	w.server = server
	defer server.Close()

	w.startOperation("turn", func(ctx context.Context) {
		w.publish("turn_done", workerTurnResult{}, true)
		eventsMu.Lock()
		events = append(events, "turn_done")
		eventsMu.Unlock()
	})

	w.turns.Wait()
	eventsMu.Lock()
	events = append(events, "idle")
	eventsMu.Unlock()

	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) < 2 || events[0] != "turn_done" || events[1] != "idle" {
		t.Fatalf("expected [turn_done, idle], got %v", events)
	}
}

func TestWorkerLSPStatusCommandReturnsManagerStatuses(t *testing.T) {
	w := &workerProcessState{lsp: lsp.NewManager(map[string]lsp.ServerSpec{
		"gopls": {Command: []string{"gopls"}, Extensions: []string{".go"}},
		"z-lsp": {Command: []string{"z-lsp"}, Extensions: []string{".z"}},
	})}
	defer w.lsp.Close()

	result, err := w.Command(context.Background(), workerwire.Command{Name: workerwire.CommandLSPStatus})
	if err != nil {
		t.Fatal(err)
	}
	var statuses []workerwire.LSPStatus
	if err := json.Unmarshal(result.Payload, &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Name != "gopls" || statuses[0].State != "not started" || statuses[1].Name != "z-lsp" || statuses[1].State != "not started" {
		t.Fatalf("worker LSP statuses = %+v", statuses)
	}
}

func TestWorkerConcurrentStartStopNeverPublishesStaleRunning(t *testing.T) {
	dir := t.TempDir()
	runtimeFile, err := workerwire.NewRuntime(dir, "test-session")
	if err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 50; iter++ {
		w := &workerProcessState{
			runtimeFile: runtimeFile,
			sessionID:   "test-session",
			state:       workerwire.StateIdle,
			pending:     make(map[string]*workerApprovalFlight),
			done:        make(chan struct{}),
		}
		w.ag = agent.New(nil, "m", 100, "sys")

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				w.startOperation("turn", func(ctx context.Context) {
					time.Sleep(time.Microsecond * 10)
				})
			}
		}()

		go func() {
			defer wg.Done()
			time.Sleep(time.Microsecond * 20)
			w.requestStop(false, "stopping")
		}()

		wg.Wait()
		w.turns.Wait()
		<-w.done

		rec, err := runtimeFile.ReadState()
		if err == nil {
			if rec.State == workerwire.StateRunning {
				t.Fatalf("state record is running after stop: %+v", rec)
			}
		}
		w.mu.Lock()
		st := w.state
		w.mu.Unlock()
		if st == workerwire.StateRunning {
			t.Fatalf("worker state is running after stop: %v", st)
		}
	}
}
