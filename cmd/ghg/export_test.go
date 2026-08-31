package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

func TestExportCLI(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("GHG_HOME", tempDir)

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

	planRes := session.WorkflowResultRecord{
		ResultID:  "res-plan-1",
		SessionID: sessionID,
		Kind:      "plan",
		Version:   1,
		Payload:   `{"goal":"implement export","steps":["step 1","step 2"],"acceptance_checks":["check 1"]}`,
		Role:      "smart",
		Provider:  "prov-test",
		Model:     "model-test",
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
		Payload:   `{"summary":"all good","verdict":"approve","findings":[],"checks_performed":["lint"]}`,
		Role:      "smart",
		Provider:  "prov-test",
		Model:     "model-test",
		CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveWorkflowResult(ctx, reviewRes); err != nil {
		t.Fatal(err)
	}

	// 1. Export latest review to file
	outFile := filepath.Join(tempDir, "review.md")
	err = exportCLI([]string{"--session", sessionID, "--kind", "review", "--output", outFile})
	if err != nil {
		t.Fatalf("export review to file failed: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if !strings.Contains(string(data), "# Review: APPROVE") || !strings.Contains(string(data), "all good") {
		t.Fatalf("unexpected exported review content: %s", string(data))
	}

	// 2. Overwriting without --force must fail
	err = exportCLI([]string{"--session", sessionID, "--kind", "review", "--output", outFile})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got: %v", err)
	}

	// 3. Overwriting with --force must succeed
	err = exportCLI([]string{"--session", sessionID, "--kind", "review", "--output", outFile, "--force"})
	if err != nil {
		t.Fatalf("export review with force failed: %v", err)
	}

	// 4. Export plan in JSON format
	outJSON := filepath.Join(tempDir, "plan.json")
	err = exportCLI([]string{"--session", sessionID, "--result", "res-plan-1", "--format", "json", "--output", outJSON})
	if err != nil {
		t.Fatalf("export plan json failed: %v", err)
	}
	jsonData, err := os.ReadFile(outJSON)
	if err != nil {
		t.Fatalf("read json file: %v", err)
	}
	if !strings.Contains(string(jsonData), `"goal": "implement export"`) || !strings.Contains(string(jsonData), `"id": "res-plan-1"`) {
		t.Fatalf("unexpected exported json content: %s", string(jsonData))
	}

	// 5. Export last message
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "This is the final assistant response text."},
	}
	_ = st.Save(sessionID, 0, msgs, "model-test", "prov-test")

	outMsg := filepath.Join(tempDir, "last.md")
	err = exportCLI([]string{"--session", sessionID, "--kind", "last", "--output", outMsg})
	if err != nil {
		t.Fatalf("export last message failed: %v", err)
	}
	msgData, err := os.ReadFile(outMsg)
	if err != nil {
		t.Fatalf("read exported last message: %v", err)
	}
	if !strings.Contains(string(msgData), "This is the final assistant response text.") {
		t.Fatalf("unexpected exported message content: %s", string(msgData))
	}

	// 6. Export whole chat log
	outChat := filepath.Join(tempDir, "chat.md")
	err = exportCLI([]string{"--session", sessionID, "--kind", "chat", "--output", outChat})
	if err != nil {
		t.Fatalf("export chat log failed: %v", err)
	}
	chatData, err := os.ReadFile(outChat)
	if err != nil {
		t.Fatalf("read exported chat: %v", err)
	}
	if !strings.Contains(string(chatData), "### User\n\nhello") || !strings.Contains(string(chatData), "### Assistant\n\nThis is the final assistant response text.") {
		t.Fatalf("unexpected exported chat content: %s", string(chatData))
	}
}
