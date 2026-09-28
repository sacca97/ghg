package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func TestOutputsGarbageCollectKeepsReferencedPayloads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)

	payloads, err := session.NewOutputStore(filepath.Join(home, "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	keep, err := payloads.Put(context.Background(), []byte("keep"), 0, true, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payloads.Put(context.Background(), []byte("drop"), 0, true, "text/plain"); err != nil {
		t.Fatal(err)
	}

	st, err := session.Open(filepath.Join(home, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Outputs = payloads
	id, err := st.Create(home, "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id, 0, []models.Message{
		{Role: "user", Content: "keep"},
		{Role: "tool", Content: "preview", Name: "read", ToolCallID: "call-1", Output: &keep},
	}, "m", "p"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := outputsCLI([]string{"gc", "--max-bytes", "1"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "removed 1") {
		t.Fatalf("cleanup output = %q", out)
	}
	if _, err := payloads.Read(context.Background(), keep, 0, 10); err != nil {
		t.Fatalf("referenced payload was removed: %v", err)
	}
}
