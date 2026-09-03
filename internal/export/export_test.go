package export

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func TestRenderPlanMarkdown(t *testing.T) {
	plan := agent.Plan{
		Goal:             "Implement cache eviction",
		Assumptions:      []string{"Memory limit is 100MB"},
		Steps:            []string{"Add LRU tracker", "Hook into eviction loop"},
		AcceptanceChecks: []string{"Unit tests pass", "Benchmark shows <1ms eviction"},
		Risks:            []string{"Lock contention under high load"},
	}

	md := RenderPlanMarkdown(plan)
	if !strings.Contains(md, "# Plan: Implement cache eviction") {
		t.Fatalf("missing goal title: %s", md)
	}
	if !strings.Contains(md, "## Assumptions\n\n- Memory limit is 100MB") {
		t.Fatalf("missing assumptions: %s", md)
	}
	if !strings.Contains(md, "## Steps\n\n1. Add LRU tracker\n2. Hook into eviction loop") {
		t.Fatalf("missing steps: %s", md)
	}
	if !strings.Contains(md, "## Acceptance checks\n\n- Unit tests pass\n- Benchmark shows <1ms eviction") {
		t.Fatalf("missing acceptance checks: %s", md)
	}
	if !strings.Contains(md, "## Risks\n\n- Lock contention under high load") {
		t.Fatalf("missing risks: %s", md)
	}
}

func TestRenderReviewMarkdown(t *testing.T) {
	review := agent.Review{
		Summary: "Architecture is good with one high severity issue.",
		Verdict: "request_changes",
		Findings: []agent.ReviewFinding{
			{
				Title:          "Minor typo in comment",
				Severity:       "low",
				File:           "internal/foo/bar.go",
				Line:           15,
				Recommendation: "Fix typo",
			},
			{
				Title:          "Race condition in counter update",
				Severity:       "critical",
				File:           "internal/foo/counter.go",
				Line:           88,
				Evidence:       "Count++ without mutex",
				Recommendation: "Use atomic.Int64",
			},
		},
		ChecksPerformed: []string{"Static analysis", "Race detector"},
	}

	md := RenderReviewMarkdown(review)
	if !strings.Contains(md, "# Review: REQUEST_CHANGES") {
		t.Fatalf("missing verdict: %s", md)
	}
	if !strings.Contains(md, "## Checks performed\n\n- Static analysis\n- Race detector") {
		t.Fatalf("missing checks performed: %s", md)
	}

	// Critical finding must appear before low finding
	critIdx := strings.Index(md, "[CRITICAL]")
	lowIdx := strings.Index(md, "[LOW]")
	if critIdx == -1 || lowIdx == -1 || critIdx > lowIdx {
		t.Fatalf("expected critical before low in findings: crit=%d, low=%d\n%s", critIdx, lowIdx, md)
	}
	if !strings.Contains(md, "- **Location**: `internal/foo/counter.go:88`") {
		t.Fatalf("missing location: %s", md)
	}
}

func TestRenderResultJSONAndMarkdown(t *testing.T) {
	record := session.WorkflowResultRecord{
		ResultID:  "res-1",
		SessionID: "sess-1",
		Kind:      "plan",
		Version:   1,
		Payload:   `{"goal":"My goal","steps":["Step 1"],"acceptance_checks":["Check 1"]}`,
		CreatedAt: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
	}

	mdBytes, err := RenderResult(record, FormatMarkdown)
	if err != nil {
		t.Fatalf("render markdown: %v", err)
	}
	if !strings.Contains(string(mdBytes), "# Plan: My goal") {
		t.Fatalf("rendered markdown mismatch: %s", string(mdBytes))
	}

	jsonBytes, err := RenderResult(record, FormatJSON)
	if err != nil {
		t.Fatalf("render json: %v", err)
	}
	if !strings.Contains(string(jsonBytes), `"goal": "My goal"`) || !strings.Contains(string(jsonBytes), `"id": "res-1"`) {
		t.Fatalf("rendered json mismatch: %s", string(jsonBytes))
	}

	v2 := record
	v2.Version = 2
	v2.Payload = `{"markdown":"# Markdown plan\n\n1. inspect\n"}`
	v2MD, err := RenderResult(v2, FormatMarkdown)
	if err != nil || string(v2MD) != "# Markdown plan\n\n1. inspect\n" {
		t.Fatalf("rendered v2 markdown = %q, err = %v", string(v2MD), err)
	}
	v2JSON, err := RenderResult(v2, FormatJSON)
	if err != nil || !strings.Contains(string(v2JSON), `"markdown": "# Markdown plan\n\n1. inspect\n"`) {
		t.Fatalf("rendered v2 json = %s, err = %v", string(v2JSON), err)
	}
}

func TestWriteExportFileAtomicAndPermissions(t *testing.T) {
	ws := t.TempDir()
	dest := "exports/my-plan.md"
	content := []byte("# Test plan\n")

	// Initial write
	path, err := WriteExportFile(dest, content, false, ws)
	if err != nil {
		t.Fatalf("WriteExportFile error: %v", err)
	}
	if expected := filepath.Join(ws, "exports", "my-plan.md"); path != expected {
		t.Fatalf("path = %q, want %q", path, expected)
	}

	// Verify file mode (0600)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("file permissions = %o, want 0600", perm)
	}

	// Verify content
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(content) {
		t.Fatalf("content = %q, want %q", string(data), string(content))
	}

	// Attempting to overwrite without force must fail
	_, err = WriteExportFile(dest, []byte("overwrite"), false, ws)
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("expected ErrDestinationExists, got: %v", err)
	}

	// Overwriting with force must succeed
	newContent := []byte("# Overwritten\n")
	path, err = WriteExportFile(dest, newContent, true, ws)
	if err != nil {
		t.Fatalf("WriteExportFile with force failed: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != string(newContent) {
		t.Fatalf("overwritten content = %q, want %q", string(data), string(newContent))
	}
}

func TestRenderChatMarkdownAndJSON(t *testing.T) {
	msgs := []models.Message{
		{Role: "system", Content: "Standard prompt"},
		{Role: "user", Content: "How do I test this?"},
		{
			Role:    "assistant",
			Content: "Run the tests with `go test`.",
			ToolCalls: []models.ToolCall{
				{
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      "bash",
						Arguments: `{"command":"go test ./..."}`,
					},
				},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "PASS\nok"},
	}

	payloadBytes, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}

	record := session.WorkflowResultRecord{
		ResultID:  "chat-1",
		SessionID: "sess-abc",
		Kind:      "chat",
		Version:   1,
		Payload:   string(payloadBytes),
	}

	directMD := RenderChat("sess-abc", msgs)
	if !strings.Contains(directMD, "How do I test this?") {
		t.Fatalf("direct render chat missing user message: %s", directMD)
	}

	mdBytes, err := RenderResult(record, FormatMarkdown)
	if err != nil {
		t.Fatalf("render chat markdown: %v", err)
	}
	md := string(mdBytes)
	if !strings.Contains(md, "# Conversation: sess-abc") {
		t.Fatalf("missing title: %s", md)
	}
	if !strings.Contains(md, "### User\n\nHow do I test this?") {
		t.Fatalf("missing user message: %s", md)
	}
	if !strings.Contains(md, "### Assistant\n\nRun the tests with `go test`.") {
		t.Fatalf("missing assistant text: %s", md)
	}
	if !strings.Contains(md, "`Tool Call: bash`") || !strings.Contains(md, "go test ./...") {
		t.Fatalf("missing tool call: %s", md)
	}
	if !strings.Contains(md, "`Tool Result (call_1)`") || !strings.Contains(md, "PASS\nok") {
		t.Fatalf("missing tool result: %s", md)
	}

	jsonBytes, err := RenderResult(record, FormatJSON)
	if err != nil {
		t.Fatalf("render chat json: %v", err)
	}
	if !strings.Contains(string(jsonBytes), "How do I test this?") {
		t.Fatalf("missing user message in json: %s", string(jsonBytes))
	}
}
