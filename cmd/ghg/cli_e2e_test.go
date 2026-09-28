//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

func TestCLIRunPersistsAndResumes(t *testing.T) {
	requests := make(chan []byte, 4)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- body

		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"read-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`+"\n\n")
			_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		case 2:
			writeSSEReply(w, "first reply")
		case 3:
			writeSSEReply(w, "resumed reply")
		case 4:
			writeSSEReply(w, "continued reply")
		default:
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
		}
	}))
	defer provider.Close()

	home := t.TempDir()
	privateHome := filepath.Join(home, ".ghg")
	if err := os.Mkdir(privateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("needle from workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".golangci.yml"), []byte("version: \"2\"\nrun:\n  timeout: 5m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectProviders := filepath.Join(workspace, ".ghg", "providers")
	if err := os.MkdirAll(projectProviders, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := "schema: 1\nid: local\ndisplay_name: local\nprotocol: openai-chat-completions\nbase_url: https://example.test/v1\nauth:\n  kind: none\ncatalog:\n  kind: none\n"
	if err := os.WriteFile(filepath.Join(projectProviders, "local.yaml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	configJSON := fmt.Sprintf(`{
		"version":2,
		"providers":{"test-provider":{"baseUrl":%q,"api":"openai-chat-completions","apiKey":"test-key"}},
		"models":{"test":{"providers":["test-provider"],"maxOut":100},"smart-model":{"providers":["test-provider"],"maxOut":100}},
		"roles":{"default":{"model":"test","provider":"test-provider"},"smart":{"model":"smart-model","provider":"test-provider"}}
	}`, provider.URL)
	if err := os.WriteFile(filepath.Join(privateHome, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHG_HOME", privateHome)
	if err := config.Trust(workspace); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCatalog("test-provider", provider.URL, []models.ModelInfo{
		{ID: "smart-model", ReasoningEfforts: []string{"low", "max"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "ghg")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ghg: %v\n%s", err, output)
	}

	runEnv := append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+cache)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = workspace
		cmd.Env = runEnv
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ghg %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}

	parseEvents := func(output string) (sessionID, reply, streamedText string) {
		t.Helper()
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			var event struct {
				Type      string `json:"type"`
				SessionID string `json:"session_id"`
				Text      string `json:"text"`
				Delta     string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("invalid CLI event %q: %v", line, err)
			}
			if event.Type == "session" {
				sessionID = event.SessionID
			}
			if event.Type == "done" {
				reply = event.Text
			}
			if event.Type == "text" && event.Delta != "" {
				streamedText += event.Delta
			}
		}
		return sessionID, reply, streamedText
	}

	first := run("run", "--format", "json", "--quiet", "read note.txt and report it")
	sessionID, firstReply, streamedText := parseEvents(first)
	if sessionID == "" || firstReply != "first reply" || streamedText != "first reply" {
		t.Fatalf("first run session=%q reply=%q streamed=%q:\n%s", sessionID, firstReply, streamedText, first)
	}

	second := run("run", "--quiet", "--resume", sessionID, "follow-up")
	if strings.TrimSpace(second) != "resumed reply" {
		t.Fatalf("text-mode resumed reply = %q, want %q", strings.TrimSpace(second), "resumed reply")
	}
	continued := run("--continue", "run", "--quiet", "continue from current directory")
	if strings.TrimSpace(continued) != "continued reply" {
		t.Fatalf("text-mode continued reply = %q, want %q", strings.TrimSpace(continued), "continued reply")
	}

	if listed := run("sessions"); !strings.Contains(listed, "read note.txt and report it") || !strings.Contains(listed, "test") {
		t.Fatalf("session list omitted title or model: %s", listed)
	}
	var roleModels map[string]string
	if err := json.Unmarshal([]byte(run("models", "--format", "json")), &roleModels); err != nil || roleModels["smart"] != "smart-model" || roleModels["fast"] != "test" {
		t.Fatalf("models JSON = %#v, err=%v", roleModels, err)
	}
	var choices []struct {
		Model            string   `json:"model"`
		Provider         string   `json:"provider"`
		ReasoningEfforts []string `json:"reasoningEfforts"`
	}
	if err := json.Unmarshal([]byte(run("models", "--all", "--format", "json")), &choices); err != nil {
		t.Fatal(err)
	}
	if len(choices) != 2 || choices[0].Model != "smart-model" || choices[1].Model != "test" || choices[0].Provider != "test-provider" || len(choices[0].ReasoningEfforts) != 3 || choices[0].ReasoningEfforts[0] != "" || choices[0].ReasoningEfforts[1] != "low" || choices[0].ReasoningEfforts[2] != "max" {
		t.Fatalf("catalog model choices = %+v", choices)
	}
	var sessions []sessionListItem
	if err := json.Unmarshal([]byte(run("sessions", "--format", "json")), &sessions); err != nil || len(sessions) != 1 || sessions[0].ID != sessionID {
		t.Fatalf("sessions JSON = %+v, err=%v", sessions, err)
	}
	trace := run("trace", sessionID, "--jsonl")
	if !strings.Contains(trace, `"session_id":"`+sessionID+`"`) || !strings.Contains(trace, `"kind":"tool_telemetry"`) {
		t.Fatalf("trace omitted persisted tool telemetry: %s", trace)
	}

	chatPath := filepath.Join(workspace, "chat.md")
	run("export", "--session", sessionID, "--kind", "chat", "--output", chatPath)
	chat, err := os.ReadFile(chatPath)
	if err != nil {
		t.Fatalf("read chat export: %v", err)
	}
	for _, want := range []string{"read note.txt and report it", "needle from workspace", "first reply", "resumed reply", "continued reply"} {
		if !bytes.Contains(chat, []byte(want)) {
			t.Fatalf("chat export omitted %q: %s", want, chat)
		}
	}
	lastPath := filepath.Join(workspace, "last.md")
	run("export", "--session", sessionID, "--kind", "last", "--output", lastPath)
	last, err := os.ReadFile(lastPath)
	if err != nil || !bytes.Contains(last, []byte("continued reply")) {
		t.Fatalf("last-message export = %q, err=%v", last, err)
	}

	firstRequest := <-requests
	if !bytes.Contains(firstRequest, []byte("read note.txt and report it")) {
		t.Fatalf("first provider request lost the initial prompt: %s", firstRequest)
	}
	toolRequest := <-requests
	if !bytes.Contains(toolRequest, []byte(`"role":"tool"`)) || !bytes.Contains(toolRequest, []byte("needle from workspace")) {
		t.Fatalf("provider did not receive the read result: %s", toolRequest)
	}
	resumed := <-requests
	for _, want := range []string{"read note.txt and report it", "needle from workspace", "first reply", "follow-up"} {
		if !bytes.Contains(resumed, []byte(want)) {
			t.Fatalf("resumed provider request lost %q: %s", want, resumed)
		}
	}
	continuedRequest := <-requests
	for _, want := range []string{"read note.txt and report it", "needle from workspace", "first reply", "follow-up", "resumed reply", "continue from current directory"} {
		if !bytes.Contains(continuedRequest, []byte(want)) {
			t.Fatalf("continued provider request lost %q: %s", want, continuedRequest)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("provider received %d requests, want 4", calls.Load())
	}
}

func writeSSEReply(w http.ResponseWriter, reply string) {
	text, _ := json.Marshal(reply)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\n", text)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
