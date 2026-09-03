package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func TestSessionsCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GHG_HOME", dir)

	st, _ := session.Open(filepath.Join(dir, "sessions.db"))
	id, _ := st.Create("/tmp", "kimi-k3-fast", "inference")
	st.Save(id, 0, []models.Message{
		{Role: "user", Content: "how do I unstage a file", Authored: true},
		{Role: "assistant", Content: "git restore --staged"},
	}, "kimi-k3-fast", "inference")
	st.Close()

	out := captureStdout(t, func() {
		if err := sessionsCLI(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "how do I unstage a file") || !strings.Contains(out, "kimi-k3-fast") {
		t.Fatalf("sessions should list id/title/model, got:\n%s", out)
	}
	if !strings.Contains(out, "just now") && !strings.Contains(out, time.Now().Format("2006-01-02")) {
		t.Fatalf("age column should render, got:\n%s", out)
	}
}

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
		if err := artifactsCLI([]string{"gc", "--max-bytes", "1"}); err != nil {
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
