package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/session"
)

func TestTraceCLIJSONLReadsTelemetry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	store, err := session.Open(filepath.Join(home, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(home, "model", "provider")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.AppendTelemetry(context.Background(), id, "tool_end", map[string]any{"tool": "read"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	traceErr := traceCLI([]string{id[:8], "--jsonl"})
	_ = w.Close()
	os.Stdout = oldStdout
	if traceErr != nil {
		t.Fatal(traceErr)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"session_id":"`+id+`"`) || !strings.Contains(string(data), `"kind":"tool_end"`) {
		t.Fatalf("trace output = %s", data)
	}
}
