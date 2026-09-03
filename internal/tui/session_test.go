package tui

import (
	"context"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The rendered screen must be artifact-free: no bare/empty SGR escapes
// (lipgloss' Width() padding artifact), no styled-blank rows, and no line
// wider than the terminal.
func TestNoArtifacts(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(70, 30))
	m.appendAssistant("Found the bug. Fixes:\n\n1. isolate HOME\n\n```go\nx := 1\n```")
	m.append("some status line")
	v := m.View()
	for i, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "\x1b[m") {
			t.Errorf("row %d still has bare SGR: %q", i, l)
		}
		if strings.TrimSpace(ansi.Strip(l)) == "" && strings.Contains(l, "\x1b[") {
			t.Errorf("row %d is a styled blank: %q", i, l)
		}
		if w := ansi.StringWidth(l); w > 70 {
			t.Errorf("row %d overflows width 70 (%d)", i, w)
		}
	}
}

func TestExportResultCommand(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sessions.db")
	st, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	sessionID, err := st.Create(tempDir, "model-test", "prov-test")
	if err != nil {
		t.Fatal(err)
	}

	m := compactCmdModel()
	m.store = st
	m.sessionID = sessionID

	// 1. When no results exist, it should report a friendly message
	m.exportResultCommand("/export-result")
	var foundNoResults bool
	for _, b := range m.blocks {
		if strings.Contains(b.text, "no completed workflow result") {
			foundNoResults = true
			break
		}
	}
	if !foundNoResults {
		t.Fatalf("expected message about no completed results, blocks: %+v", m.blocks)
	}

	// 2. Add plan and review to session
	planRes := session.WorkflowResultRecord{
		ResultID:  "res-plan-1",
		SessionID: sessionID,
		Kind:      "plan",
		Version:   1,
		Payload:   `{"goal":"build export","steps":["s1","s2"],"acceptance_checks":["c1"]}`,
		Role:      "smart",
		CreatedAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := st.SaveWorkflowResult(ctx, planRes); err != nil {
		t.Fatal(err)
	}

	reviewRes := session.WorkflowResultRecord{
		ResultID:  "res-review-1",
		SessionID: sessionID,
		Kind:      "review",
		Version:   1,
		Payload:   `{"summary":"review summary","verdict":"approve","findings":[]}`,
		Role:      "smart",
		CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveWorkflowResult(ctx, reviewRes); err != nil {
		t.Fatal(err)
	}

	// 3. Export latest review to a specified file
	outFile := filepath.Join(tempDir, "my-review.md")
	m.exportResultCommand("/export-result review " + outFile)

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("failed to read exported review: %v", err)
	}
	if !strings.Contains(string(data), "# Review: APPROVE") {
		t.Fatalf("unexpected content in exported file: %s", string(data))
	}

	// 4. Overwrite without force must show already exists error
	m.blocks = nil
	m.exportResultCommand("/export-result review " + outFile)
	var foundExistsErr bool
	for _, b := range m.blocks {
		if strings.Contains(b.text, "already exists") {
			foundExistsErr = true
			break
		}
	}
	if !foundExistsErr {
		t.Fatalf("expected already exists error block, got: %+v", m.blocks)
	}

	// 5. Overwrite with --force must succeed
	m.blocks = nil
	m.exportResultCommand("/export-result review " + outFile + " --force")
	var foundSuccess bool
	for _, b := range m.blocks {
		if strings.Contains(b.text, "exported review") {
			foundSuccess = true
			break
		}
	}
	if !foundSuccess {
		t.Fatalf("expected export success message, got: %+v", m.blocks)
	}

	// 6. Export last message
	m.appendAssistant("This is the last assistant response summarizing the work.")
	lastMsgOut := filepath.Join(tempDir, "last-message.md")
	m.exportResultCommand("/export-result last " + lastMsgOut)

	msgData, err := os.ReadFile(lastMsgOut)
	if err != nil {
		t.Fatalf("failed to read exported last message: %v", err)
	}
	if !strings.Contains(string(msgData), "This is the last assistant response summarizing the work.") {
		t.Fatalf("unexpected content in exported message: %s", string(msgData))
	}

	// 7. Export chat log
	chatOut := filepath.Join(tempDir, "chat-log.md")
	m.exportResultCommand("/export-result chat " + chatOut)
	chatData, err := os.ReadFile(chatOut)
	if err != nil {
		t.Fatalf("failed to read exported chat log: %v", err)
	}
	if !strings.Contains(string(chatData), "# Conversation") || !strings.Contains(string(chatData), "This is the last assistant response") {
		t.Fatalf("unexpected content in exported chat log: %s", string(chatData))
	}
}

func TestExportProposedPlan(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sessions.db")
	st, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	m := compactCmdModel()
	m.store = st
	m.modelName = "fast-model"
	m.provName = "fast-prov"

	// Propose a plan before any session exists
	m.proposedPlanMD = "# Plan: Migrate database\n\n1. step 1: backup\n2. step 2: apply migrations\n"

	outFile := filepath.Join(tempDir, "plan-export.md")
	m.exportResultCommand("/export-result plan " + outFile)

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("failed to read exported plan: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# Plan: Migrate database") || !strings.Contains(content, "step 1: backup") {
		t.Fatalf("unexpected plan export content:\n%s", content)
	}
}

func forkModel(t *testing.T) *model {
	t.Helper()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &model{
		input:    newInput(),
		agent:    &agent.Agent{},
		store:    st,
		queueSel: -1,
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1", Authored: true},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2", Authored: true},
		{Role: "assistant", Content: "a2"},
	}
	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	m.sessionID = id
	m.saved = len(m.agent.Messages)
	if err := st.Save(id, 1, m.agent.Messages, "m", "p"); err != nil {
		t.Fatal(err)
	}
	m.rebuildTranscript()
	return m
}

func tailBlock(m *model) string { return m.blocks[len(m.blocks)-1].text }

func TestForkWithArg(t *testing.T) {
	m := forkModel(t)
	m.command("/fork experiment")

	if m.sessionID == "" || strings.Contains(tailBlock(m), "fork failed") {
		t.Fatalf("fork failed: %q", tailBlock(m))
	}
	meta, msgs, err := m.store.Load(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "experiment" || len(msgs) != 4 { // the full conversation
		t.Fatalf("fork: %+v (%d msgs)", meta, len(msgs))
	}
	if !strings.Contains(tailBlock(m), "⑂ forked") {
		t.Fatalf("confirmation: %q", tailBlock(m))
	}
	// the original session survives untouched under /resume
	recent, err := m.store.Recent(10)
	if err != nil || len(recent) != 2 {
		t.Fatalf("recent: %v %+v", err, recent)
	}
}

func TestForkBareOpensNamePrompt(t *testing.T) {
	m := forkModel(t)
	m.command("/fork")

	if m.namePrompt == nil || m.input.Value() != "q1 (fork #1)" {
		t.Fatalf("prompt: %+v input=%q", m.namePrompt, m.input.Value())
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // accept the suggestion
	meta, _, err := m.store.Load(m.sessionID)
	if err != nil || meta.Title != "q1 (fork #1)" {
		t.Fatalf("fork title: %+v %v", meta, err)
	}
	// a second bare fork unwraps the suffix and suggests #2 off the base
	m.command("/fork")
	if m.input.Value() != "q1 (fork #2)" {
		t.Fatalf("suggestion: %q", m.input.Value())
	}
	// esc cancels without forking
	press(t, m, esc(m))
	if m.namePrompt != nil {
		t.Fatal("esc should cancel the prompt")
	}
	if recent, _ := m.store.Recent(10); len(recent) != 2 {
		t.Fatalf("cancelled fork created a session: %+v", recent)
	}
}

func TestForkFromRewindPicker(t *testing.T) {
	m := forkModel(t)
	press(t, m, esc(m))
	press(t, m, esc(m)) // open rewind picker, selection on "q2"
	press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})

	if m.rew != nil || m.namePrompt == nil {
		t.Fatalf("f should swap picker for name prompt: rew=%v np=%v", m.rew, m.namePrompt)
	}
	m.input.SetValue("at-q2")
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	meta, msgs, err := m.store.Load(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "at-q2" {
		t.Fatalf("title: %+v", meta)
	}
	// forked at q2: the copy keeps the selected message (q1, a1, q2);
	// rewinding instead would drop q2
	if len(msgs) != 3 || msgs[2].Content != "q2" {
		t.Fatalf("prefix: %+v", msgs)
	}
	if len(m.agent.Messages) != 4 {
		t.Fatalf("live messages: %+v", m.agent.Messages)
	}
}

func TestForkWhileRewoundIntoFuture(t *testing.T) {
	// rewind to before q1, then fork from the picker at the future q2 entry:
	// the clipped tail up to the cut comes along
	m := forkModel(t)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // select q1
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m.input.Reset()
	if len(m.agent.Messages) != 1 || len(m.future) != 4 {
		t.Fatalf("rewound: msgs=%d future=%d", len(m.agent.Messages), len(m.future))
	}

	press(t, m, esc(m))
	press(t, m, esc(m)) // reopen: both entries are future
	if len(m.rew.entries) != 2 {
		t.Fatalf("entries: %+v", m.rew.entries)
	}
	// sel 0 = newest = q2 (cut = 1+2 = 3); fork through it
	press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m.input.SetValue("redo-fork")
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.agent.Messages) != 4 || m.agent.Messages[3].Content != "q2" {
		t.Fatalf("fork through future: %+v", m.agent.Messages)
	}
	_, msgs, err := m.store.Load(m.sessionID)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("stored: %v %+v", err, msgs)
	}
	// fork consumes the redo stack — the new session starts at the picked
	// point with no forward tail
	if len(m.future) != 0 {
		t.Fatalf("remaining future: %+v", m.future)
	}
}

func TestRename(t *testing.T) {
	m := forkModel(t)
	m.command("/rename better-name")
	meta, _, err := m.store.Load(m.sessionID)
	if err != nil || meta.Title != "better-name" {
		t.Fatalf("rename: %+v %v", meta, err)
	}

	// bare rename prompts prefilled with the current title
	m.command("/rename")
	if m.namePrompt == nil || m.input.Value() != "better-name" {
		t.Fatalf("prompt: %q", m.input.Value())
	}
	m.input.SetValue("final-name")
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	meta, _, _ = m.store.Load(m.sessionID)
	if meta.Title != "final-name" {
		t.Fatalf("prompt rename: %+v", meta)
	}
}

// Resuming an interrupted session tells the user exactly what the model was
// told: each dangling tool call renders as an inline "interrupted" row under
// its call, plus one summary note at the resume boundary.
func TestResumeShowsInterruptedToolCalls(t *testing.T) {
	m := compactCmdModel()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st

	call := func(id, name string) models.ToolCall {
		var tc models.ToolCall
		tc.ID, tc.Function.Name = id, name
		tc.Function.Arguments = `{"path":"x.go"}`
		return tc
	}
	id, _ := st.Create("/tmp", m.modelName, m.provName)
	msgs := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "fix the tests", Authored: true},
		{Role: "assistant", ToolCalls: []models.ToolCall{call("c1", "read"), call("c2", "bash")}},
		{Role: "tool", Content: "file body", ToolCallID: "c1", Name: "read"},
		// c2 never answered: the crash landed here
	}
	if err := st.Save(id, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}

	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}

	var rows []string
	for _, b := range m.blocks {
		rows = append(rows, ansi.Strip(b.render(m.width)))
	}
	var sawCallRow, sawNote bool
	for _, r := range rows {
		if strings.Contains(r, "⚒ bash") && strings.Contains(r, "interrupted") {
			sawCallRow = true
		}
		if strings.Contains(r, "1 tool call(s) were interrupted") && strings.Contains(r, "can retry") {
			sawNote = true
		}
	}
	if !sawCallRow {
		t.Fatalf("transcript missing the inline interrupted row for bash:\n%s", strings.Join(rows, "\n"))
	}
	if !sawNote {
		t.Fatalf("transcript missing the resume summary note:\n%s", strings.Join(rows, "\n"))
	}
	// the real read result must not be mislabeled as interrupted
	for _, r := range rows {
		if strings.Contains(r, "⚒ read") && strings.Contains(r, "interrupted") {
			t.Fatalf("answered tool mislabeled as interrupted: %q", r)
		}
	}
}

func TestWorkerConnectionErrorRecoversTUI(t *testing.T) {
	m := compactCmdModel()
	serverConn1, clientConn1 := net.Pipe()
	defer serverConn1.Close()
	defer clientConn1.Close()

	serverConn2, clientConn2 := net.Pipe()
	defer serverConn2.Close()
	defer clientConn2.Close()

	client1 := workerwire.NewClient(clientConn1, "s1")
	client2 := workerwire.NewClient(clientConn2, "s2")

	cancelled := false
	m.workerClient = client1
	m.busy = true
	m.cancel = func() { cancelled = true }
	m.interrupt1 = true
	m.thinkStart = time.Now()

	// 1. Deliver connection error from stale client2
	m.Update(workerErrorMsg{err: errors.New("connection reset"), client: client2})

	if !m.busy {
		t.Fatal("stale client error should not clear busy")
	}
	if m.workerClient != client1 {
		t.Fatal("stale client error should not replace or clear current client")
	}
	if m.cancel == nil || cancelled {
		t.Fatal("stale client error should not clear or invoke cancel")
	}
	if !m.interrupt1 {
		t.Fatal("stale client error should not clear interrupt1")
	}

	// 2. Deliver connection error from current client1
	m.Update(workerErrorMsg{err: errors.New("connection closed"), client: client1})

	if m.busy {
		t.Fatal("current client error should clear busy")
	}
	if m.workerClient != nil {
		t.Fatal("current client error should clear workerClient")
	}
	if m.cancel != nil {
		t.Fatal("current client error should clear cancel")
	}
	if m.interrupt1 {
		t.Fatal("current client error should clear interrupt1")
	}
	if m.workerState != workerwire.StateInterrupted {
		t.Fatalf("workerState = %v, want StateInterrupted", m.workerState)
	}
	if !m.thinkStart.IsZero() {
		t.Fatal("current client error should stop thinking timer")
	}
}

func TestPickerDeleteWithDD(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sessions.db")
	st, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	id1, err := st.Create(tempDir, "model1", "prov1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id1, 0, []models.Message{{Role: "user", Content: "hello 1"}}, "model1", "prov1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	id2, err := st.Create(tempDir, "model2", "prov2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id2, 0, []models.Message{{Role: "user", Content: "hello 2"}}, "model2", "prov2"); err != nil {
		t.Fatal(err)
	}

	m := compactCmdModel()
	m.store = st
	m.openPicker()

	if m.picker == nil || len(m.picker.metas) != 2 {
		t.Fatalf("expected picker with 2 sessions, got %+v", m.picker)
	}

	// First press of 'd' arms deletion
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if !m.picker.pendingD {
		t.Fatal("expected pendingD to be true after first 'd'")
	}
	view := m.pickerView()
	if !strings.Contains(view, "press d again to delete session") {
		t.Fatalf("expected pending delete prompt in view, got: %s", view)
	}

	// Pressing another key cancels pending delete
	m.pickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.picker.pendingD {
		t.Fatal("expected pendingD to be reset after arrow key")
	}

	// Pressing 'd' twice deletes the selected session
	deletedID := m.picker.metas[m.picker.idx].ID
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})

	if len(m.picker.metas) != 1 {
		t.Fatalf("expected 1 session remaining in picker, got %d", len(m.picker.metas))
	}
	if m.picker.metas[0].ID == deletedID {
		t.Fatalf("expected deleted session %s to not be in picker", deletedID)
	}

	// Verify session was deleted from database
	recent, err := st.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Fatalf("expected 1 session in database, got %d", len(recent))
	}
	for _, rec := range recent {
		if rec.ID == deletedID {
			t.Fatalf("expected deleted session %s to not be in db", deletedID)
		}
	}
	// Delete the last remaining session — closes the picker
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if m.picker != nil {
		t.Fatalf("expected picker to close after deleting all sessions, got: %+v", m.picker)
	}
}

func TestPickerVimNavigation(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sessions.db")
	st, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	id1, _ := st.Create(tempDir, "m1", "p1")
	_ = st.Save(id1, 0, []models.Message{{Role: "user", Content: "1"}}, "m1", "p1")
	time.Sleep(10 * time.Millisecond)
	id2, _ := st.Create(tempDir, "m2", "p2")
	_ = st.Save(id2, 0, []models.Message{{Role: "user", Content: "2"}}, "m2", "p2")

	m := compactCmdModel()
	m.store = st
	m.openPicker()

	if m.picker == nil || m.picker.idx != 0 {
		t.Fatalf("expected initial index 0, got %+v", m.picker)
	}

	// 'k' navigates up (older)
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.picker.idx != 1 {
		t.Fatalf("expected index 1 after 'k', got %d", m.picker.idx)
	}

	// 'j' navigates down (newer)
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.picker.idx != 0 {
		t.Fatalf("expected index 0 after 'j', got %d", m.picker.idx)
	}
}

func TestPickerDeleteCurrentSessionResetsState(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sessions.db")
	st, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	id, err := st.Create(tempDir, "m1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Save(id, 0, []models.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}, "m1", "p1")

	m := compactCmdModel()
	m.store = st
	m.sessionID = id
	m.saved = 2
	m.agent.Messages = append(m.agent.Messages, models.Message{Role: "user", Content: "hi"})

	m.openPicker()
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})

	if m.sessionID != "" {
		t.Fatalf("sessionID should be empty, got %q", m.sessionID)
	}
	if m.saved != 1 {
		t.Fatalf("saved should be 1 (skip system prompt), got %d", m.saved)
	}
	if len(m.agent.Messages) != 1 {
		t.Fatalf("agent messages should only retain system prompt, got %d", len(m.agent.Messages))
	}
}

// rewindModel builds an idle model with an authored conversation and a real
// (temp-dir) session store. msgs excludes the system prompt; the session is
// created and fully persisted.
func rewindModel(t *testing.T, msgs ...models.Message) *model {
	t.Helper()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &model{
		input:    newInput(),
		agent:    &agent.Agent{},
		store:    st,
		queueSel: -1,
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	m.agent.Messages = append([]models.Message{{Role: "system", Content: "sys"}}, msgs...)
	m.vp.SetContent("x")
	// persisted as if turns had run
	if err := st.Save(m.sessionIDC(t), 1, m.agent.Messages, "m", "p"); err != nil {
		t.Fatal(err)
	}
	m.saved = len(m.agent.Messages)
	m.rebuildTranscript()
	return m
}

func (m *model) sessionIDC(t *testing.T) string {
	t.Helper()
	id, err := m.store.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	m.sessionID = id
	return id
}

func esc(m *model) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEsc} }

func TestDoubleEscOpensRewind(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
	)
	press(t, m, esc(m)) // first: arms
	if m.rew != nil {
		t.Fatal("single esc must not open the picker")
	}
	if !m.esc1 {
		t.Fatal("first idle esc should arm")
	}
	press(t, m, esc(m)) // second: opens
	if m.rew == nil {
		t.Fatal("double esc should open the rewind picker")
	}
	if len(m.rew.entries) != 1 || m.rew.entries[0].text != "q1" {
		t.Fatalf("entries: %+v", m.rew.entries)
	}
}

func TestBusyEscStillInterrupts(t *testing.T) {
	m := rewindModel(t, models.Message{Role: "user", Content: "q1", Authored: true})
	m.busy = true
	called := false
	m.cancel = func() { called = true }
	press(t, m, esc(m))
	if !called || m.rew != nil {
		t.Fatal("busy esc must interrupt, never open rewind")
	}
}

// Regression: esc with a draft typed while the agent is running must clear
// the DRAFT (double-esc, into input history), not interrupt the agent.
func TestBusyEscWithDraftClearsInputNotAgent(t *testing.T) {
	m := rewindModel(t, models.Message{Role: "user", Content: "q1", Authored: true})
	m.busy = true
	called := false
	m.cancel = func() { called = true }
	m.input.SetValue("half-written follow-up")

	press(t, m, esc(m)) // first: arms the clear, warning shows
	if called {
		t.Fatal("esc with a draft must not interrupt the agent")
	}
	if !m.escClr {
		t.Fatal("first esc with a draft should arm the clear")
	}
	if m.input.Value() == "" {
		t.Fatal("first esc must not clear the draft yet")
	}

	press(t, m, esc(m)) // second: clears the draft into input history
	if called {
		t.Fatal("double-esc with a draft must never interrupt the agent")
	}
	if m.input.Value() != "" {
		t.Fatalf("double-esc should clear the draft, got %q", m.input.Value())
	}
	if got := m.hist[len(m.hist)-1]; got != "half-written follow-up" {
		t.Fatalf("cleared draft should land in input history, got %q", got)
	}
	if m.histIdx != len(m.hist) {
		t.Fatalf("histIdx should sit at the newest edge, got %d of %d", m.histIdx, len(m.hist))
	}
	if m.rew != nil {
		t.Fatal("clearing a draft must not open the rewind picker")
	}
	// chat history untouched: agent messages are exactly what we seeded
	if len(m.agent.Messages) != 2 { // system + q1
		t.Fatalf("chat history must be untouched, got %d messages", len(m.agent.Messages))
	}
}

// The cleared draft is recallable with ↑, in case the clear was an accident.
func TestClearedDraftRecallsWithUp(t *testing.T) {
	m := rewindModel(t, models.Message{Role: "user", Content: "q1", Authored: true})
	m.input.SetValue("oops i cleared it")
	press(t, m, esc(m))
	press(t, m, esc(m))
	if m.input.Value() != "" {
		t.Fatal("double-esc should clear the draft")
	}
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.input.Value() != "oops i cleared it" {
		t.Fatalf("↑ should recall the cleared draft, got %q", m.input.Value())
	}
}

// A single esc with a draft, then a pause past the arming window, leaves the
// draft intact and requires a fresh double-esc.
func TestSingleEscKeepsDraft(t *testing.T) {
	m := rewindModel(t, models.Message{Role: "user", Content: "q1", Authored: true})
	m.input.SetValue("still thinking")
	press(t, m, esc(m))
	if !m.escClr {
		t.Fatal("first esc should arm the clear")
	}
	// the arming window closes on escArmMsg (the tea.Tick firing)
	tm, _ := m.Update(escArmMsg{})
	m = tm.(*model)
	if m.escClr {
		t.Fatal("arming window should have closed")
	}
	if m.input.Value() != "still thinking" {
		t.Fatalf("draft must survive a lone esc, got %q", m.input.Value())
	}
}

// Idle with NO draft: double-esc still opens the rewind picker (unchanged).
func TestDoubleEscWithoutDraftStillRewinds(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
	)
	press(t, m, esc(m))
	if m.escClr {
		t.Fatal("no draft: the draft-clear arm must stay off")
	}
	if !m.esc1 {
		t.Fatal("first idle esc should arm the rewind")
	}
	press(t, m, esc(m))
	if m.rew == nil {
		t.Fatal("double esc with no draft should open the rewind picker")
	}
}

// Dismissing UI (e.g. an open completion menu) consumes the esc and must not
// arm either the clear or the rewind.
func TestEscDismissalDoesNotArm(t *testing.T) {
	m := rewindModel(t, models.Message{Role: "user", Content: "q1", Authored: true})
	m.input.SetValue("/mo")
	m.menu = &menu{head: "/", cands: []cand{{Text: "/model"}}}
	press(t, m, esc(m))
	if m.menu != nil {
		t.Fatal("esc should dismiss the menu")
	}
	if m.escClr || m.esc1 {
		t.Fatal("a dismissal must not arm clear or rewind")
	}
	if m.input.Value() != "/mo" {
		t.Fatalf("dismissing the menu keeps the draft, got %q", m.input.Value())
	}
}

// Regression: the picker lists oldest at the TOP and the latest message at
// the BOTTOM, with the selection starting on the latest. ↑ moves up toward
// older, ↓ down toward newer. The bug was newest-first rendering plus a
// "distance from newest" selection index, which read reversed.
func TestRewindPickerOrderAndArrows(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true},
		models.Message{Role: "assistant", Content: "a2"},
		models.Message{Role: "user", Content: "q3", Authored: true},
	)
	press(t, m, esc(m))
	press(t, m, esc(m))

	// entries are chronological; the selection starts on the LATEST (bottom)
	if got := len(m.rew.entries); got != 3 {
		t.Fatalf("entries: %d", got)
	}
	if m.rew.sel != 2 || m.rew.entries[m.rew.sel].text != "q3" {
		t.Fatalf("selection should start on the latest q3: sel=%d", m.rew.sel)
	}

	// rendered top-to-bottom: q1, q2, q3 — q3 (the latest) last
	view := m.rewindView()
	i1, i2, i3 := strings.Index(view, "q1"), strings.Index(view, "q2"), strings.Index(view, "q3")
	if i1 < 0 || i1 >= i2 || i2 >= i3 {
		t.Fatalf("list should read oldest→latest top→bottom (q1 q2 q3)\n%s", view)
	}
	// exactly one row carries the cursor marker, and it is q3's
	if strings.Count(view, "❯") != 1 {
		t.Fatalf("exactly one selected row should be marked\n%s", view)
	}
	// ↑ walks toward older (up the list): q3 → q2 → q1, then clamps
	for _, want := range []string{"q2", "q1", "q1"} {
		press(t, m, tea.KeyMsg{Type: tea.KeyUp})
		if got := m.rew.entries[m.rew.sel].text; got != want {
			t.Fatalf("↑ should move to %s, on %s", want, got)
		}
	}
	// ↓ walks back toward newer: q1 → q2 → q3, then clamps at the latest
	for _, want := range []string{"q2", "q3", "q3"} {
		press(t, m, tea.KeyMsg{Type: tea.KeyDown})
		if got := m.rew.entries[m.rew.sel].text; got != want {
			t.Fatalf("↓ should move to %s, on %s", want, got)
		}
	}
}

// Each entry renders its submission timestamp dimmed on the line below the
// preview. Messages predating SentAt show an em dash, never a wrong time.
func TestRewindPickerShowsTimestamps(t *testing.T) {
	t1 := time.Date(2025, 6, 1, 14, 30, 0, 0, time.Local)
	t2 := time.Date(2025, 6, 1, 15, 45, 0, 0, time.Local)
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true, SentAt: &t1},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true, SentAt: &t2},
		models.Message{Role: "assistant", Content: "a2"},
		models.Message{Role: "user", Content: "q3-old", Authored: true}, // no SentAt: legacy row
	)
	press(t, m, esc(m))
	press(t, m, esc(m))

	view := m.rewindView()
	for _, ts := range []string{"2025-06-01 14:30", "2025-06-01 15:45"} {
		if !strings.Contains(view, ts) {
			t.Errorf("picker should show timestamp %q\n%s", ts, view)
		}
	}
	// the legacy row (no SentAt) renders a dash, and each timestamp sits on
	// the line below its own preview
	lines := strings.Split(view, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "q1") && !strings.Contains(lines[i+1], "14:30") {
			t.Errorf("q1's timestamp should sit directly below it\n%s", view)
		}
		if strings.Contains(ln, "q3-old") && !strings.Contains(lines[i+1], "—") {
			t.Errorf("q3-old (no SentAt) should show a dash below it\n%s", view)
		}
	}
}

func TestRewindTruncatesAndRestoresInput(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true},
		models.Message{Role: "assistant", Content: "a2"},
	)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // sel starts on the latest (q2); ↑ moves to q1
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// rewound to just before q1: only the system prompt survives
	if len(m.agent.Messages) != 1 {
		t.Fatalf("messages after rewind: %+v", m.agent.Messages)
	}
	if m.input.Value() != "q1" {
		t.Fatalf("input should restore the rewound message, got %q", m.input.Value())
	}
	if len(m.future) != 4 { // q1..a2 clipped
		t.Fatalf("redo stack: %+v", m.future)
	}
	if m.saved != 1 {
		t.Fatalf("saved=%d", m.saved)
	}
	// DB rows at/after the cut are gone
	_, stored, err := m.store.Load(m.sessionID)
	if err != nil || len(stored) != 0 {
		t.Fatalf("stored after rewind: %v %+v", err, stored)
	}
	// transcript rebuilt: no message blocks remain
	var texts []string
	for _, b := range m.blocks {
		texts = append(texts, b.text)
	}
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, "q1") || strings.Contains(joined, "q2") {
		t.Fatalf("blocks: %q", joined)
	}
}

// After a rewind, resubmitting records the replaced message's text as
// RewoundFrom — rewind provenance survives on the new message (and in the
// store) even though the redo stack is discarded.
func TestResubmitAfterRewindStampsRewoundFrom(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2 original", Authored: true},
		models.Message{Role: "assistant", Content: "a2"},
	)
	// rewind to before q2: q2/a2 become the redo stack
	m.applyRewind(3)
	if len(m.future) != 2 || len(m.agent.Messages) != 3 {
		t.Fatalf("after rewind: msgs=%d future=%d", len(m.agent.Messages), len(m.future))
	}

	// the submitTurn logic: capture the replaced text, then discard the future
	rewoundFrom := ""
	if len(m.future) > 0 {
		for _, fm := range m.future {
			if fm.Role == "user" && fm.Authored {
				rewoundFrom = oneLine(fm.Content)
				break
			}
		}
	}
	if rewoundFrom != "q2 original" {
		t.Fatalf("rewoundFrom should capture the replaced message, got %q", rewoundFrom)
	}
	m.discardFuture()
	// the resubmitted message is stamped (what submitTurn does post-turn)
	m.agent.Messages = append(m.agent.Messages, models.Message{
		Role: "user", Content: "q2 edited", Authored: true, RewoundFrom: rewoundFrom,
	})
	got := m.agent.Messages[len(m.agent.Messages)-1]
	if got.RewoundFrom != "q2 original" {
		t.Fatalf("resubmitted message should carry RewoundFrom, got %q", got.RewoundFrom)
	}
	if len(m.future) != 0 {
		t.Fatalf("redo stack should be discarded, got %d", len(m.future))
	}
}

func TestRewindForwardTravel(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true},
		models.Message{Role: "assistant", Content: "a2"},
	)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // q2 → q1, rewind to just before it
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m.input.Reset()
	if len(m.agent.Messages) != 1 || len(m.future) != 4 {
		t.Fatalf("after rewind: msgs=%d future=%d", len(m.agent.Messages), len(m.future))
	}

	// reopen: both clipped user messages appear as dimmed future entries
	press(t, m, esc(m))
	press(t, m, esc(m))
	if len(m.rew.entries) != 2 || !m.rew.entries[0].future || !m.rew.entries[1].future {
		t.Fatalf("entries: %+v", m.rew.entries)
	}
	// sel 0 is q2 (the newest future entry): enter goes forward to just before it
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// forward to just before q2: q1/a1 restored, q2/a2 still clipped
	if len(m.agent.Messages) != 3 || len(m.future) != 2 || m.future[0].Content != "q2" {
		t.Fatalf("forward: msgs=%d future=%+v", len(m.agent.Messages), m.future)
	}
	// the restored rows are persisted again
	if _, stored, _ := m.store.Load(m.sessionID); len(stored) != 2 {
		t.Fatalf("stored after forward: %+v", stored)
	}
	// forward travel does not clobber the input
	if m.input.Value() != "" {
		t.Fatalf("input: %q", m.input.Value())
	}
}

func TestRewindNeverCutsToolCallPairs(t *testing.T) {
	// entries sit at user messages; tool results always travel with their
	// assistant message, so no cut can orphan a tool result from its call
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{{ID: "c1"}}},
		models.Message{Role: "tool", ToolCallID: "c1", Content: "out"},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true},
	)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // q1
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.agent.Messages) != 1 { // only the system prompt survives
		t.Fatalf("messages: %+v", m.agent.Messages)
	}
	if _, stored, _ := m.store.Load(m.sessionID); len(stored) != 0 {
		t.Fatalf("stored: %+v", stored)
	}
}

func TestRewindCancelLeavesConversation(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
	)
	before := len(m.agent.Messages)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	press(t, m, esc(m)) // cancel
	if m.rew != nil || len(m.agent.Messages) != before || len(m.future) != 0 {
		t.Fatal("cancel must not touch the conversation")
	}
}

func TestPartialRewindKeepsPrefixInDB(t *testing.T) {
	// rewind to a middle cut: the retained prefix must survive in the DB
	// exactly (seq == conversation index; a cut at 3 keeps seq 1 and 2)
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true},
		models.Message{Role: "assistant", Content: "a2"},
		models.Message{Role: "user", Content: "q3", Authored: true},
	)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // sel starts on q3 (latest); ↑ moves to q2
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.agent.Messages) != 3 { // sys, q1, a1
		t.Fatalf("messages: %+v", m.agent.Messages)
	}
	_, stored, err := m.store.Load(m.sessionID)
	if err != nil || len(stored) != 2 || stored[0].Content != "q1" || stored[1].Content != "a1" {
		t.Fatalf("stored prefix: %v %+v", err, stored)
	}
}

func TestEscArmDoesNotLeakAcrossModalDismiss(t *testing.T) {
	m := forkModel(t)
	press(t, m, esc(m))  // arm
	m.command("/rename") // opens the name prompt
	press(t, m, esc(m))  // dismisses the prompt — must not count toward rewind
	if m.esc1 {
		t.Fatal("modal dismissal must clear the esc arm")
	}
	press(t, m, esc(m)) // one more: arms again, picker must NOT open
	if m.rew != nil {
		t.Fatal("picker opened from a stale arm")
	}
}

func TestNamePromptPreservesDraft(t *testing.T) {
	m := forkModel(t)
	m.input.SetValue("my half-typed thought")
	m.command("/rename") // prompt takes over the input box
	if m.input.Value() == "my half-typed thought" {
		t.Fatal("prompt should replace the input")
	}
	press(t, m, esc(m)) // cancel: the draft comes back
	if m.input.Value() != "my half-typed thought" {
		t.Fatalf("draft lost: %q", m.input.Value())
	}
}

func TestResumeAfterRewindMatches(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2", Authored: true},
	)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // rewind to before q1

	// a fresh load of the session sees exactly the rewound history (nothing)
	_, stored, err := m.store.Load(m.sessionID)
	if err != nil || len(stored) != 0 {
		t.Fatalf("resumed history: %v %+v", err, stored)
	}
}

// Workspace rewind: a turn's file changes are captured as a git snapshot, and
// rewinding the conversation past that turn restores the files too — while
// untracked files the user made are left alone and the rollback is recorded.
func TestWorkspaceRewind(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")
	t.Chdir(repo) // cwd() is process-global; snapshot/restore run here

	m := compactCmdModel()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st
	m.sessionID, _ = st.Create(repo, m.modelName, m.provName)

	// a turn starts: snapshot the pre-turn workspace, then the agent edits a
	// tracked file mid-turn
	m.snapshots = map[int]string{}
	snap := session.SnapshotWorkspace(repo)
	if snap == "" {
		t.Fatal("a clean tree still snapshots (as HEAD) — the point is pre-turn state")
	}
	if !session.WorkspaceClean(repo) {
		t.Fatal("tree should be clean before the turn")
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("agent edit\n"), 0o644)
	if session.WorkspaceClean(repo) {
		t.Fatal("tree should be dirty after the agent's edit")
	}
	// turn ends dirty → the snapshot is kept, keyed by the turn's start index
	m.snapshots[3] = snap
	if err := st.SetSnapshot(m.sessionID, 3, snap); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "mine.txt"), []byte("keep me\n"), 0o644)

	// conversation rewind past the turn: messages 0..2 survive, cut at 3
	m.agent.Messages = append(m.agent.Messages,
		models.Message{Role: "system"},
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "do the edit", Authored: true},
		models.Message{Role: "assistant", Content: "done"},
	)
	m.rebuildTranscript()
	m.applyRewind(3)

	body, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(body) != "base\n" {
		t.Fatalf("tracked file not restored: %q", body)
	}
	if _, err := os.Stat(filepath.Join(repo, "mine.txt")); err != nil {
		t.Fatal("untracked user file must survive a workspace rewind")
	}
	if got := st.Snapshots(m.sessionID); len(got) != 0 {
		t.Fatalf("consumed snapshot rows should be trimmed, got %v", got)
	}

	// the transcript shows the rollback
	var sawNote bool
	for _, b := range m.blocks {
		if strings.Contains(ansi.Strip(b.render(m.width)), "workspace rewound") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Fatal("transcript should record the workspace rewind")
	}
}

// A turn that changed nothing leaves no snapshot, and a rewind without any
// snapshot restores nothing and notes nothing.
func TestWorkspaceRewindNoSnapshot(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")
	t.Chdir(repo)

	m := compactCmdModel()
	m.snapshots = map[int]string{}
	m.agent.Messages = append(m.agent.Messages,
		models.Message{Role: "system"},
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
	)
	m.rebuildTranscript()
	blocksBefore := len(m.blocks)
	m.applyRewind(1)
	for _, b := range m.blocks {
		if strings.Contains(ansi.Strip(b.render(m.width)), "workspace rewound") {
			t.Fatal("no snapshot, no rewind note")
		}
	}
	if len(m.blocks) >= blocksBefore {
		t.Fatal("rewind should still clip the transcript")
	}
	body, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(body) != "base\n" {
		t.Fatalf("file should be untouched: %q", body)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
