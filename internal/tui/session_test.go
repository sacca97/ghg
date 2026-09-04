package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
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
		store:    st,
		queueSel: -1,
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	m.messages = []models.Message{
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
	if err := st.Save(id, 1, m.messages, "m", "p"); err != nil {
		t.Fatal(err)
	}
	m.rebuildTranscript()
	return m
}

func tailBlock(m *model) string { return m.blocks[len(m.blocks)-1].text }

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
	m.messages = append(m.messages, models.Message{Role: "user", Content: "hi"})

	m.openPicker()
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m.pickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})

	if m.sessionID != "" {
		t.Fatalf("sessionID should be empty, got %q", m.sessionID)
	}
	if len(m.messages) != 1 {
		t.Fatalf("agent messages should only retain system prompt, got %d", len(m.messages))
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
		store:    st,
		queueSel: -1,
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	m.messages = append([]models.Message{{Role: "system", Content: "sys"}}, msgs...)
	m.vp.SetContent("x")
	// persisted as if turns had run
	if err := st.Save(m.sessionIDC(t), 1, m.messages, "m", "p"); err != nil {
		t.Fatal(err)
	}
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
	if len(m.messages) != 2 { // system + q1
		t.Fatalf("chat history must be untouched, got %d messages", len(m.messages))
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

func TestResubmitAfterRewindStampsRewoundFrom(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
		models.Message{Role: "user", Content: "q2 original", Authored: true},
		models.Message{Role: "assistant", Content: "a2"},
	)
	// simulate rewind to before q2: q2/a2 become the redo stack
	m.future = append(slices.Clone(m.messages[3:]), m.future...)
	m.messages = m.messages[:3]
	if len(m.future) != 2 || len(m.messages) != 3 {
		t.Fatalf("after rewind: msgs=%d future=%d", len(m.messages), len(m.future))
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
	m.messages = append(m.messages, models.Message{
		Role: "user", Content: "q2 edited", Authored: true, RewoundFrom: rewoundFrom,
	})
	got := m.messages[len(m.messages)-1]
	if got.RewoundFrom != "q2 original" {
		t.Fatalf("resubmitted message should carry RewoundFrom, got %q", got.RewoundFrom)
	}
	if len(m.future) != 0 {
		t.Fatalf("redo stack should be discarded, got %d", len(m.future))
	}
}

func TestRewindCancelLeavesConversation(t *testing.T) {
	m := rewindModel(t,
		models.Message{Role: "user", Content: "q1", Authored: true},
		models.Message{Role: "assistant", Content: "a1"},
	)
	before := len(m.messages)
	press(t, m, esc(m))
	press(t, m, esc(m))
	press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	press(t, m, esc(m)) // cancel
	if m.rew != nil || len(m.messages) != before || len(m.future) != 0 {
		t.Fatal("cancel must not touch the conversation")
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

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestWorkerForkAndRenameAcks(t *testing.T) {
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	origID, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	forkID, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}

	m := compactCmdModel()
	m.store = st
	m.sessionID = origID
	// 1. Deliver rename ack
	renamePayload, _ := json.Marshal(workerwire.RenameResult{
		SessionID: origID,
		Title:     "fresh title",
	})
	m.handleWorkerFrame(workerwire.Frame{
		Type:      workerwire.TypeAck,
		RequestID: "rename-123",
		Payload:   renamePayload,
	})
	if !strings.Contains(tailBlock(m), "fresh title") {
		t.Fatalf("confirmation: %q", tailBlock(m))
	}

	// 2. Deliver fork ack
	forkPayload, _ := json.Marshal(workerwire.ForkResult{
		NewSessionID: forkID,
		OldSessionID: origID,
		Title:        "fork title",
		OldTitle:     "orig title",
	})
	m.handleWorkerFrame(workerwire.Frame{
		Type:      workerwire.TypeAck,
		RequestID: "fork-123",
		Payload:   forkPayload,
	})
	if m.sessionID != forkID {
		t.Fatalf("sessionID = %q, want %q", m.sessionID, forkID)
	}
	if !strings.Contains(tailBlock(m), "⑂ forked") {
		t.Fatalf("confirmation: %q", tailBlock(m))
	}
}

func TestResumeRestoresProposedPlan(t *testing.T) {
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	planJSON, _ := json.Marshal(map[string]string{"markdown": "# My Saved Plan\n\n1. Do this"})
	err = st.SaveWorkflowResult(context.Background(), session.WorkflowResultRecord{
		ResultID:   "plan-1",
		SessionID:  id,
		Kind:       "plan",
		Version:    2,
		Payload:    string(planJSON),
		MessageSeq: 1,
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	m := compactCmdModel()
	m.store = st
	if err := m.resumeDisplay(id); err != nil {
		t.Fatal(err)
	}
	if m.proposedPlanMD != "# My Saved Plan\n\n1. Do this" {
		t.Fatalf("proposedPlanMD = %q, want plan markdown", m.proposedPlanMD)
	}
}

func TestWorkerTurnSubmissionAndDoneVerticalSlice(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	m := compactCmdModel()
	m.sessionID = "test-session-slice"
	client := workerwire.NewClient(clientConn, "test-session-slice")
	m.workerClient = client

	readFrame := make(chan workerwire.Frame, 1)
	readErr := make(chan error, 1)
	go func() {
		decoder := workerwire.NewDecoder(serverConn)
		f, err := decoder.Read()
		if err != nil {
			readErr <- err
			return
		}
		readFrame <- f
	}()

	// 1. Submit turn via TUI
	m.submitTurn("explain the algorithm", true)

	if !m.busy {
		t.Fatal("expected model to be busy during turn")
	}

	// 2. Read command frame on serverConn
	var frame workerwire.Frame
	select {
	case frame = <-readFrame:
	case err := <-readErr:
		t.Fatalf("failed to read turn command: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn command frame")
	}
	if frame.Type != workerwire.TypeCommand {
		t.Fatalf("frame type = %q, want %q", frame.Type, workerwire.TypeCommand)
	}
	var cmdReq workerwire.CommandRequest
	if err := json.Unmarshal(frame.Payload, &cmdReq); err != nil {
		t.Fatal(err)
	}
	if cmdReq.Name != workerwire.CommandInput {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandInput)
	}
	var input workerwire.Input
	if err := json.Unmarshal(cmdReq.Payload, &input); err != nil {
		t.Fatal(err)
	}
	if input.Input != "explain the algorithm" || !input.Authored {
		t.Fatalf("input = %+v", input)
	}

	// 3. Worker emits turn_done event
	turnResultData, _ := json.Marshal(workerwire.TurnResult{
		Final:         "Here is the explanation.",
		Usage:         models.Usage{PromptTokens: 10, CompletionTokens: 5},
		ContextTokens: 15,
		At:            input.At,
		Clean:         true,
	})
	cmd := m.workerEvent(workerEvent{Kind: "turn_done", Data: turnResultData})
	if cmd == nil {
		t.Fatal("expected command from turn_done")
	}
	doneMsg := cmd()
	m.handleTurnDone(doneMsg.(turnDoneMsg))

	if m.busy {
		t.Fatal("model should not be busy after turn done")
	}
	if m.usage.PromptTokens != 10 || m.usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v, want prompt=10 completion=5", m.usage)
	}
	if m.workerContextTokens != 15 {
		t.Fatalf("contextTokens = %d, want 15", m.workerContextTokens)
	}

	// 4. Test plan delivery in turn_done
	turnWithPlan, _ := json.Marshal(workerwire.TurnResult{
		Final: "Here is the plan.",
		Plan:  "# Step 1\n\nRun the test",
	})
	cmdPlan := m.workerEvent(workerEvent{Kind: "turn_done", Data: turnWithPlan})
	donePlanMsg := cmdPlan()
	m.mode = uiModePlan
	m.handleTurnDone(donePlanMsg.(turnDoneMsg))
	if m.proposedPlanMD != "# Step 1\n\nRun the test" {
		t.Fatalf("proposedPlanMD = %q, want plan", m.proposedPlanMD)
	}
}
