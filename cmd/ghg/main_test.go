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
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
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
	for _, fragment := range []string{"use grep for literal or regex text", "glob for exact path patterns", "find_files for fuzzy paths", "read for exact bounded source ranges", "When multiple independent repository queries are already known"} {
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
	if err := st.Save(id, 1, []models.Message{{Role: "system"}, {Role: "user", Content: "continue me"}}, "model", "provider"); err != nil {
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

type workerTestBackend struct {
	planText string
}

func (b *workerTestBackend) Stream(_ context.Context, _ models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
	if sink.OnText != nil {
		sink.OnText(b.planText)
	}
	return models.Message{Role: "assistant", Content: b.planText}, models.Usage{PromptTokens: 10, CompletionTokens: 20}, nil
}

func (b *workerTestBackend) Complete(_ context.Context, _ models.Request) (models.Message, models.Usage, error) {
	return models.Message{Role: "assistant", Content: b.planText}, models.Usage{}, nil
}

func TestWorkerPlanTurnAndSnapshot(t *testing.T) {
	baseDir := t.TempDir()
	runtimeFile, err := workerwire.NewRuntime(baseDir, "s-plan-1")
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.Open(filepath.Join(baseDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	planText := "I will create a plan.\n<proposed_plan>\n# Step 1\nDo the work\n</proposed_plan>"
	backend := &workerTestBackend{planText: planText}
	ag := agent.New(backend, "mock-model", 1000, "system prompt")

	w := &workerProcessState{
		runtimeFile: runtimeFile,
		sessionID:   "s-plan-1",
		state:       workerwire.StateIdle,
		ag:          ag,
		store:       store,
		mode:        "plan",
		pending:     make(map[string]*workerApprovalFlight),
		done:        make(chan struct{}),
	}
	ag.PlanMode = true

	snapAny, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap, ok := snapAny.(workerSnapshot)
	if !ok {
		t.Fatalf("expected workerSnapshot, got %T", snapAny)
	}
	if snap.Mode != "plan" {
		t.Fatalf("expected snapshot mode 'plan', got %q", snap.Mode)
	}

	w.startTurn(workerInput{Input: "make a plan", PlanMode: true})
	w.turns.Wait()

	// Verify plan was persisted to workflow results
	results, err := store.ListWorkflowResults(context.Background(), "s-plan-1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 workflow result, got %d", len(results))
	}
	if results[0].Kind != "plan" || results[0].Version != 2 {
		t.Fatalf("expected kind 'plan' version 2, got kind %q version %d", results[0].Kind, results[0].Version)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(results[0].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload["markdown"], "# Step 1") {
		t.Fatalf("expected markdown to contain '# Step 1', got %q", payload["markdown"])
	}
}

func TestWorkerForkAndRenameCommands(t *testing.T) {
	dir := t.TempDir()
	st, err := session.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sessionID, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	b := &workerTestBackend{}
	ag := agent.New(b, "m", 100, "sys")
	ag.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1", Authored: true},
		{Role: "assistant", Content: "a1"},
	}

	w := &workerProcessState{
		sessionID: sessionID,
		store:     st,
		ag:        ag,
		state:     workerwire.StateIdle,
	}

	// 1. Test Rename
	renamePayload, _ := json.Marshal(workerwire.RenameRequest{Title: "renamed session"})
	renameRes, err := w.Command(context.Background(), workerwire.Command{
		Name:    workerwire.CommandRename,
		Payload: renamePayload,
	})
	if err != nil {
		t.Fatalf("rename command failed: %v", err)
	}
	var renameResult workerwire.RenameResult
	if err := json.Unmarshal(renameRes.Payload, &renameResult); err != nil {
		t.Fatal(err)
	}
	if renameResult.Title != "renamed session" {
		t.Fatalf("rename result title = %q, want 'renamed session'", renameResult.Title)
	}
	meta, _, err := st.Load(sessionID)
	if err != nil || meta.Title != "renamed session" {
		t.Fatalf("stored title = %q, want 'renamed session'", meta.Title)
	}

	// 2. Test Fork
	forkPayload, _ := json.Marshal(workerwire.ForkRequest{Cut: 2, Title: "forked session"})
	forkRes, err := w.Command(context.Background(), workerwire.Command{
		Name:    workerwire.CommandFork,
		Payload: forkPayload,
	})
	if err != nil {
		t.Fatalf("fork command failed: %v", err)
	}
	var forkResult workerwire.ForkResult
	if err := json.Unmarshal(forkRes.Payload, &forkResult); err != nil {
		t.Fatal(err)
	}
	if forkResult.Title != "forked session" || forkResult.NewSessionID == "" || forkResult.OldSessionID != sessionID {
		t.Fatalf("fork result = %+v", forkResult)
	}
	forkedMeta, forkedMsgs, err := st.Load(forkResult.NewSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if forkedMeta.Title != "forked session" || len(forkedMsgs) != 3 {
		t.Fatalf("forked session meta = %+v, msgs = %d", forkedMeta, len(forkedMsgs))
	}
}

func TestWorkerHumanGateAndPermRules(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	perms := tools.LoadPermRules()
	w := &workerProcessState{
		perms:   perms,
		pending: make(map[string]*workerApprovalFlight),
	}

	req := tools.GateRequest{Tool: "bash", Command: "git status", Rule: "git status"}
	if perms.CoveredBy(req) {
		t.Fatal("initially should not be covered")
	}

	// 1. Uncovered command: humanGate starts flight in background
	answered := make(chan struct{})
	go func() {
		dec, _ := w.humanGate(req)
		if dec != tools.GateAllowAlways {
			t.Errorf("expected GateAllowAlways, got %v", dec)
		}
		close(answered)
	}()

	// Wait for pending flight
	var id string
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		w.mu.Lock()
		for pendingID := range w.pending {
			id = pendingID
			break
		}
		w.mu.Unlock()
		if id != "" {
			break
		}
	}
	if id == "" {
		t.Fatal("expected pending approval flight")
	}

	// 2. Answer allow_always
	ok := w.answerApproval(workerApprovalAnswer{ID: id, Decision: "allow_always"})
	if !ok {
		t.Fatal("answerApproval returned false")
	}
	<-answered

	// 3. PermRules now covers git status
	if !perms.CoveredBy(req) {
		t.Fatal("expected perms to cover git status now")
	}

	// 4. Calling humanGate again should return immediately without pending flight
	dec, redirect := w.humanGate(req)
	if dec != tools.GateAllowOnce || redirect != "" {
		t.Fatalf("expected GateAllowOnce, got %v redirect=%q", dec, redirect)
	}
	w.mu.Lock()
	pendingLen := len(w.pending)
	w.mu.Unlock()
	if pendingLen != 0 {
		t.Fatalf("expected 0 pending flights, got %d", pendingLen)
	}
}
