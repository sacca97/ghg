package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func TestModelsCLIJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	configJSON := `{"defaultModel":"fallback","defaultProvider":"provider","providers":{"provider":{"baseUrl":"https://example.test"}},"models":{"fallback":{"providers":["provider"]},"smart-model":{"providers":["provider"]}},"roles":{"smart":{"model":"smart-model","provider":"provider"}}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCatalog("provider", "https://example.test", []models.ModelInfo{
		{ID: "smart-model", ReasoningEfforts: []string{"low", "max"}},
	}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := modelsCLI([]string{"--format", "json"}); err != nil {
			t.Fatal(err)
		}
	})
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["smart"] != "smart-model" || got["fast"] != "fallback" {
		t.Fatalf("role models = %#v", got)
	}
	out = captureStdout(t, func() {
		if err := modelsCLI([]string{"--all", "--format", "json"}); err != nil {
			t.Fatal(err)
		}
	})
	var choices []catalogModelChoice
	if err := json.Unmarshal([]byte(out), &choices); err != nil {
		t.Fatal(err)
	}
	if len(choices) != 2 || choices[0].Model != "fallback" || choices[1].Model != "smart-model" {
		t.Fatalf("catalog models = %#v", choices)
	}
	if got := choices[1].ReasoningEfforts; len(got) != 3 || got[0] != "" || got[1] != "low" || got[2] != "max" {
		t.Fatalf("smart model reasoning efforts = %#v", got)
	}
}

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
		if err := sessionsCLI(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "how do I unstage a file") || !strings.Contains(out, "kimi-k3-fast") {
		t.Fatalf("sessions should list id/title/model, got:\n%s", out)
	}
	if !strings.Contains(out, "just now") && !strings.Contains(out, time.Now().Format("2006-01-02")) {
		t.Fatalf("age column should render, got:\n%s", out)
	}
	out = captureStdout(t, func() {
		if err := sessionsCLI([]string{"--format", "json"}); err != nil {
			t.Fatal(err)
		}
	})
	var listed []sessionListItem
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != id || listed[0].Title != "how do I unstage a file" {
		t.Fatalf("sessions JSON = %#v", listed)
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
