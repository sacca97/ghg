package worker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRuntimeStateAndPromptRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Phase 3.7 first cut uses Unix workers")
	}
	baseDir, err := os.MkdirTemp("/tmp", "ghg-worker-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(baseDir)
	rt, err := NewRuntime(baseDir, "state-1")
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Truncate(time.Second)
	if err := rt.WriteState(StateRecord{State: StateIdle, Role: "fast", PID: 41, UpdatedAt: when}); err != nil {
		t.Fatal(err)
	}
	got, err := rt.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != rt.SessionID || got.State != StateIdle || got.Role != "fast" || got.PID != 41 || !got.UpdatedAt.Equal(when) {
		t.Fatalf("state = %+v", got)
	}
	if err := rt.WritePrompt("line 1\nline 2"); err != nil {
		t.Fatal(err)
	}
	prompt, err := rt.ReadPrompt()
	if err != nil || prompt != "line 1\nline 2" {
		t.Fatalf("prompt = %q, error = %v", prompt, err)
	}
	states, err := ListStates(baseDir)
	if err != nil || len(states) != 1 || states[0].SessionID != rt.SessionID {
		t.Fatalf("states = %+v, error = %v", states, err)
	}
}

func TestRuntimeStateRejectsForeignRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Phase 3.7 first cut uses Unix workers")
	}
	rt, err := NewRuntime(t.TempDir(), "state-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.WriteState(StateRecord{SessionID: "other", State: StateIdle}); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("foreign state error = %v, want ErrInvalidSession", err)
	}
}

func TestListStatesSkipsCorruptRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Phase 3.7 first cut uses Unix workers")
	}
	baseDir := t.TempDir()
	rt, err := NewRuntime(baseDir, "valid-1")
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Truncate(time.Second)
	if err := rt.WriteState(StateRecord{State: StateIdle, Role: "fast", PID: 42, UpdatedAt: when}); err != nil {
		t.Fatal(err)
	}

	corruptDir := filepath.Join(baseDir, "run", "corrupt-2")
	if err := os.MkdirAll(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "state.json"), []byte("{malformed json"), 0o600); err != nil {
		t.Fatal(err)
	}

	states, err := ListStates(baseDir)
	if err != nil {
		t.Fatalf("ListStates returned error on corrupt record: %v", err)
	}
	if len(states) != 1 || states[0].SessionID != "valid-1" {
		t.Fatalf("ListStates = %+v, want 1 valid record", states)
	}
}
