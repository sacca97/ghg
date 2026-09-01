package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

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
	if err := st.Save(id1, 0, []llm.Message{{Role: "user", Content: "hello 1"}}, "model1", "prov1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	id2, err := st.Create(tempDir, "model2", "prov2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id2, 0, []llm.Message{{Role: "user", Content: "hello 2"}}, "model2", "prov2"); err != nil {
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
	_ = st.Save(id1, 0, []llm.Message{{Role: "user", Content: "1"}}, "m1", "p1")
	time.Sleep(10 * time.Millisecond)
	id2, _ := st.Create(tempDir, "m2", "p2")
	_ = st.Save(id2, 0, []llm.Message{{Role: "user", Content: "2"}}, "m2", "p2")

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
