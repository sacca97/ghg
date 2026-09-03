package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
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

	final, err := ag.Turn(context.Background(), "review clean code", Events{})
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
}
