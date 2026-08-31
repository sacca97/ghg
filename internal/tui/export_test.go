package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/session"
)

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
	m.finishPlanProposal(planProposalMsg{
		plan: agent.Plan{
			Goal:             "Migrate database",
			Steps:            []string{"step 1: backup", "step 2: apply migrations"},
			AcceptanceChecks: []string{"check table exists"},
		},
	})

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
