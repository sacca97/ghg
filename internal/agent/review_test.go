package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

type reviewHistoryCatalog struct{}

func (reviewHistoryCatalog) SearchHistory(context.Context, string, string, string, *int, int) ([]HistoryHit, error) {
	return nil, nil
}

func (reviewHistoryCatalog) ReadHistory(context.Context, string, int, int, *int, int) ([]HistoryMessage, []string, error) {
	return nil, nil, nil
}

func TestParseReviewValid(t *testing.T) {
	input := `{
		"summary": "The PR looks well-structured with minor issues.",
		"verdict": "request_changes",
		"findings": [
			{
				"title": "Missing bounds check in slice indexing",
				"severity": "high",
				"file": "internal/foo/bar.go",
				"line": 42,
				"evidence": "buf[idx] can panic if idx >= len(buf)",
				"recommendation": "Add if idx >= len(buf) check before access"
			},
			{
				"title": "Unused variable",
				"severity": "info",
				"file": "internal/foo/bar.go",
				"line": 10,
				"recommendation": "Remove unused var"
			}
		],
		"checks_performed": [
			"Static analysis",
			"Bounds check analysis",
			"Concurrency review"
		]
	}`

	review, err := ParseReview(input)
	if err != nil {
		t.Fatalf("unexpected error parsing valid review: %v", err)
	}
	if review.Verdict != "request_changes" {
		t.Errorf("verdict = %q, want request_changes", review.Verdict)
	}
	if len(review.Findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(review.Findings))
	}
	if review.Findings[0].Severity != "high" || review.Findings[0].Line != 42 {
		t.Errorf("finding 0 mismatch: %+v", review.Findings[0])
	}
	if len(review.ChecksPerformed) != 3 {
		t.Errorf("got %d checks, want 3", len(review.ChecksPerformed))
	}
}

func TestParseReviewRejectsInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"invalid json", "{bad"},
		{"no summary", `{"verdict":"approve","findings":[]}`},
		{"invalid verdict", `{"summary":"sum","verdict":"pass","findings":[]}`},
		{"finding without title", `{"summary":"sum","verdict":"approve","findings":[{"severity":"high"}]}`},
		{"finding invalid severity", `{"summary":"sum","verdict":"approve","findings":[{"title":"t","severity":"extreme"}]}`},
		{"negative line number", `{"summary":"sum","verdict":"approve","findings":[{"title":"t","severity":"low","line":-5}]}`},
		{"empty check", `{"summary":"sum","verdict":"approve","findings":[],"checks_performed":[""]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReview(tc.input)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tc.name)
			}
		})
	}
}

func TestReviewBudgetInventoryAndAssessment(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("package p\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "p_test.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		target     string
		inventory  ReviewInventory
		wantBudget int
	}{
		{name: "minimum", target: "review a.go", inventory: reviewInventoryAt(workspace, "a.go"), wantBudget: 5},
		{name: "broad scope", target: "review bugs performance cleanup", inventory: reviewInventoryAt(workspace, ""), wantBudget: 7},
		{name: "large-file bonus", target: "review", inventory: ReviewInventory{ProductionFiles: 15, ProductionLOC: 6000, LargeFiles: []string{"a.go", "b.go", "c.go"}}, wantBudget: 11},
		{name: "partial uses maximum", target: "review", inventory: ReviewInventory{Partial: true}, wantBudget: 24},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewBudgetBaseline(tc.target, tc.inventory); got != tc.wantBudget {
				t.Fatalf("baseline = %d, want %d", got, tc.wantBudget)
			}
		})
	}
	inventory := reviewInventoryAt(workspace, "")
	if inventory.ProductionFiles != 5 || inventory.TestFiles != 1 || inventory.ProductionLOC != 5 {
		t.Fatalf("inventory = %+v", inventory)
	}

	assessment, err := parseReviewBudgetAssessment(`{"budget":7,"rationale":"the scope is small","focus":["a.go"]}`, 5, inventory)
	if err != nil || assessment.Budget != 7 {
		t.Fatalf("assessment = %+v, err = %v", assessment, err)
	}
	if _, err := parseReviewBudgetAssessment(`{"budget":8,"rationale":"ok","focus":[],"extra":true}`, 5, inventory); err == nil {
		t.Fatal("unknown assessment fields should be rejected")
	}
	if _, err := parseReviewBudgetAssessment(`{"budget":13,"rationale":"too broad","focus":[]}`, 10, inventory); err == nil {
		t.Fatal("assessment beyond baseline plus two should be rejected")
	}
	if got := reviewExtensionAllocation(36, reviewExplorationHardMax); got != 2 {
		t.Fatalf("36-round lease = %d, want 2", got)
	}
	if got := reviewExtensionAllocation(reviewExplorationHardMax, reviewExplorationHardMax); got != 0 {
		t.Fatalf("hard-limit lease = %d, want 0", got)
	}
}

func TestReviewBudgetUsesLeasesAndFinalEvidence(t *testing.T) {
	workspace := t.TempDir()
	for i := 0; i < 20; i++ {
		lines := 394
		if i < 5 {
			lines = 501
		} else if i < 10 {
			lines = 395
		}
		name := filepath.Join(workspace, fmt.Sprintf("file-%d.go", i))
		if err := os.WriteFile(name, []byte(strings.Repeat("x\n", lines)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workspace)
	call := func(id, name, args string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: args}}
	}
	read := func(i int) models.Message {
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call(fmt.Sprintf("read-%d", i), "read", fmt.Sprintf(`{"path":"file-%d.go"}`, i%5))}}
	}
	responses := make([]models.Message, 0, 47)
	for i := 0; i < 16; i++ {
		responses = append(responses, read(i))
	}
	for allocation := 16; allocation <= 36; allocation += 4 {
		responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call(fmt.Sprintf("extend-%d", allocation), "request_review_extension", `{"unresolved_issue":"trace the boundary","evidence":"the rewind caller and persisted cutoff","remaining_lookup":"read the rewind path and compaction mapping","rounds":4}`)}})
		lease := 4
		if allocation == 36 {
			lease = 2 // 36 → 38 is the truncated final lease.
		}
		for i := 0; i < lease; i++ {
			responses = append(responses, read(200+allocation+i))
		}
	}
	responses = append(responses,
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("pending-final", "read", `{"path":"file-0.go"}`)}},
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("final-read", "read", `{"path":"file-0.go"}`)}},
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("submit", "submit_review", `{"summary":"done","verdict":"approve","findings":[]}`)}},
	)
	backend := &checkpointBackend{responses: responses}
	ag := New(backend, "model", 100, "system")
	ag.ReviewMode = true
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}, {
		Def: models.NewTool("glob", "glob", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "unexpected", nil },
	}, {
		Def: models.NewTool("find_files", "find_files", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "unexpected", nil },
	}}
	var progress []ReviewProgress
	final, err := ag.TurnAuthored(context.Background(), "review bugs performance cleanup", Events{
		OnReviewProgress: func(value ReviewProgress) { progress = append(progress, value) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if final != `{"summary":"done","verdict":"approve","findings":[]}` {
		t.Fatalf("final = %q", final)
	}
	if len(backend.requests) != len(responses) {
		t.Fatalf("model calls = %d, want %d", len(backend.requests), len(responses))
	}
	if !requestContains(backend.requests[0], "<review_preflight>") || !requestContains(backend.requests[0], "file-0.go") {
		t.Fatalf("first request omitted deterministic review preflight: %+v", backend.requests[0].Messages)
	}
	for _, tool := range backend.requests[0].Tools {
		if tool.Function.Name == "glob" || tool.Function.Name == "find_files" {
			t.Fatalf("review discovery tool exposed: %s", tool.Function.Name)
		}
	}
	if len(progress) < 2 || progress[0].Phase != "inventory" || progress[0].Allocation != 16 || len(progress[0].Focus) != 5 || progress[1].Phase != "assessment" || progress[1].Allocation != 16 {
		t.Fatalf("malformed/unavailable assessment fallback progress = %+v", progress[:min(len(progress), 2)])
	}
	if got := backend.requests[16].Tools[len(backend.requests[16].Tools)-1].Function.Name; got != "request_review_extension" {
		t.Fatalf("checkpoint tools end with %q", got)
	}
	if got := backend.requests[44].Tools; len(got) != 2 || got[0].Function.Name != "read" || got[1].Function.Name != "submit_review" || !requestContains(backend.requests[44], "review_budget_checkpoint") {
		t.Fatalf("final evidence tools = %+v", got)
	}
	for _, index := range []int{45, 46} {
		if got := backend.requests[index].Tools; len(got) != 1 || got[0].Function.Name != "submit_review" {
			t.Fatalf("post-evidence tools at %d = %+v", index, got)
		}
	}
	if !requestContains(backend.requests[46], reviewFinalizationToolError) {
		t.Fatalf("post-close navigation did not receive finalization error: %+v", backend.requests[46].Messages)
	}
	var extensions, boundaries, finals, closed int
	var lastAllocation int
	closedAt := -1
	for index, event := range progress {
		switch event.Phase {
		case "extension":
			extensions++
			lastAllocation = event.ToAllocation
		case "budget_boundary":
			boundaries++
		case "final_evidence":
			finals++
		case "exploration_closed":
			closed++
			closedAt = index
		}
	}
	if extensions != 6 || lastAllocation != 38 || boundaries != 7 || finals != 1 || closed != 1 {
		t.Fatalf("progress leases/boundaries/final/closed = %d/%d/%d/%d/%d", extensions, boundaries, lastAllocation, finals, closed)
	}
	for _, event := range progress[closedAt+1:] {
		if event.Phase == "budget_boundary" {
			t.Fatalf("closed review re-entered budget boundary: %+v", event)
		}
	}
	for _, event := range progress {
		if event.CurrentRound > reviewExplorationHardMax {
			t.Fatalf("exploration exceeded hard limit: %+v", event)
		}
	}
}

func TestReviewModeNormalTurn(t *testing.T) {
	var attempts int
	reviewArgs := `{"summary":"all clean","verdict":"approve","findings":[]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, tool := range req.Tools {
			name := tool.Function.Name
			if name == "write" || name == "edit" || name == "bash" {
				t.Errorf("mutating tool %s exposed in review mode", name)
			}
		}
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			// First attempt calls submit_review with invalid JSON
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"rev-invalid","type":"function","function":{"name":"submit_review","arguments":"{invalid json}"}}]}}]}`+"\n\n")
		} else {
			// Second attempt corrects and calls submit_review with valid args
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"rev-valid\",\"type\":\"function\",\"function\":{\"name\":\"submit_review\",\"arguments\":%q}}]}}]}\n\n", reviewArgs)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	be := testBackend(ts.URL, "test-key")
	ag := New(be, "test-model", 4096, "sys")
	ag.ReviewMode = true

	final, err := ag.Turn(context.Background(), "review codebase", Events{})
	if err != nil {
		t.Fatalf("Turn error: %v", err)
	}
	if final != reviewArgs {
		t.Fatalf("final = %q, want %q", final, reviewArgs)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestWithdrawnReviewDiscoveryToolsCannotRun(t *testing.T) {
	ran := make(chan string, 2)
	tool := func(name string) tools.Tool {
		return tools.Tool{
			Def: models.NewTool(name, name, `{"type":"object"}`),
			Run: func(context.Context, json.RawMessage) (string, error) {
				ran <- name
				return "unexpected", nil
			},
		}
	}
	call := func(id, name string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: `{}`}}
	}
	ag := New(nil, "model", 100, "system")
	results := ag.runToolResultsWithPolicy(context.Background(), []models.ToolCall{
		call("glob", "glob"), call("find", "find_files"),
	}, Events{}, []tools.Tool{submitReviewTool()}, []tools.Tool{
		tool("glob"), tool("find_files"),
	}, reviewFinalizationToolError, nil)
	for i, result := range results {
		if result.Preview != reviewFinalizationToolError {
			t.Fatalf("result %d = %q, want unavailable-tool error", i, result.Preview)
		}
	}
	if len(ran) != 0 {
		t.Fatalf("withdrawn discovery tools executed: %v", ran)
	}
}

func TestReviewCheckpointHandoffPersisted(t *testing.T) {
	id := "review-session"

	reviewArgs := `{"summary":"Found security flaw","verdict":"request_changes","findings":[{"title":"SQL injection","severity":"high","file":"db.go","line":10,"evidence":"raw string concat","recommendation":"use parameterized query"}],"checks_performed":["security scan"]}`

	backend := &mockAgentBackend{
		responses: []models.Message{
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "call-submit",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "submit_review",
							Arguments: reviewArgs,
						},
					},
				},
			},
		},
	}

	ag := New(backend, "test-model", 4096, "sys")
	ag.ReviewMode = true
	ag.HistoryCatalog = reviewHistoryCatalog{}
	ag.SetSessionID(id)

	var compactionCalled bool
	var rawMessagesCount int
	var compactedSummary string
	ev := Events{
		OnCompactionReady: func(messages []models.Message, summary string, cutoff int) error {
			compactionCalled = true
			rawMessagesCount = len(messages)
			compactedSummary = summary
			return nil
		},
	}

	final, err := ag.Turn(context.Background(), "review sql code", ev)
	if err != nil {
		t.Fatalf("Turn error: %v", err)
	}
	if final != reviewArgs {
		t.Fatalf("final = %q, want %q", final, reviewArgs)
	}
	if !compactionCalled {
		t.Fatal("expected OnCompactionReady to be called on persisted review completion")
	}
	if rawMessagesCount < 3 {
		t.Fatalf("expected raw messages to include prompt, assistant turn, and tool result, got count %d", rawMessagesCount)
	}
	if !strings.Contains(compactedSummary, "Found security flaw") || !strings.Contains(compactedSummary, "SQL injection") {
		t.Fatalf("compactedSummary missing key findings: %q", compactedSummary)
	}
	// Verify active messages were compacted to system prompt + checkpoint summary
	if len(ag.Messages) != 2 {
		t.Fatalf("expected active messages to be compacted to 2 messages, got %d: %+v", len(ag.Messages), ag.Messages)
	}
	if !ag.compacted {
		t.Fatal("expected ag.compacted to be true")
	}
}

func TestReviewHandoffWithoutPersistenceRetainsHistory(t *testing.T) {
	reviewArgs := `{"summary":"Clean code","verdict":"approve","findings":[]}`

	backend := &mockAgentBackend{
		responses: []models.Message{
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "call-submit",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "submit_review",
							Arguments: reviewArgs,
						},
					},
				},
			},
		},
	}

	ag := New(backend, "test-model", 4096, "sys")
	ag.ReviewMode = true
	// Notice: ag.HistoryCatalog and session ID are NOT set (no durable persistence)
	var notices []string

	final, err := ag.Turn(context.Background(), "review clean code", Events{
		OnNotice: func(value string) { notices = append(notices, value) },
	})
	if err != nil {
		t.Fatalf("Turn error: %v", err)
	}
	if final != reviewArgs {
		t.Fatalf("final = %q, want %q", final, reviewArgs)
	}
	// Verify messages remain uncompacted (retained in full)
	if len(ag.Messages) <= 2 {
		t.Fatalf("expected raw history to be retained without durable persistence, got %d messages", len(ag.Messages))
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "checkpoint skipped") {
		t.Fatalf("expected skipped checkpoint notice, got %v", notices)
	}
}
