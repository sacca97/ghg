package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
)

func TestRolloutBudgetWeightedUsageAndThresholds(t *testing.T) {
	budget := newPlanRolloutBudget()
	if budget.Remaining() != defaultPlanBudgetLimit {
		t.Fatalf("initial remaining = %f, want %f", budget.Remaining(), defaultPlanBudgetLimit)
	}
	if budget.ReminderBlock() != "" {
		t.Fatalf("expected no reminder initially, got %q", budget.ReminderBlock())
	}

	// 100k prompt tokens, 50k cached, 5k completion
	// fresh = 50k -> 50,000 * 0.1 = 5,000
	// completion = 5,000 * 1.0 = 5,000
	// total = 10,000 units
	budget.RecordUsage(llm.Usage{
		PromptTokens:     100_000,
		CompletionTokens: 5_000,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{
			CachedTokens: 50_000,
		},
	}, 0)

	if budget.freshInput != 50_000 || budget.cachedInput != 50_000 || budget.outputTokens != 5_000 {
		t.Fatalf("unexpected token counters: fresh=%d cached=%d output=%d", budget.freshInput, budget.cachedInput, budget.outputTokens)
	}
	if budget.usedUnits != 10_000 {
		t.Fatalf("usedUnits = %f, want 10000", budget.usedUnits)
	}
	if budget.Remaining() != 90_000 {
		t.Fatalf("remaining = %f, want 90000", budget.Remaining())
	}

	// Spend 45,000 more units -> 55,000 used, 45,000 remaining (<= 50,000 threshold)
	budget.RecordUsage(llm.Usage{CompletionTokens: 45_000}, 0)
	rem := budget.ReminderBlock()
	if !strings.Contains(rem, "50000 weighted tokens remaining") {
		t.Fatalf("expected 50k reminder, got: %s", rem)
	}

	// Spend 30,000 more units -> 85,000 used, 15,000 remaining (<= 25,000 threshold)
	budget.RecordUsage(llm.Usage{CompletionTokens: 30_000}, 0)
	rem = budget.ReminderBlock()
	if !strings.Contains(rem, "25000 weighted tokens remaining") {
		t.Fatalf("expected 25k reminder, got: %s", rem)
	}

	// Spend 6,000 more units -> 91,000 used, 9,000 remaining (<= 10,000 reserve)
	budget.RecordUsage(llm.Usage{CompletionTokens: 6_000}, 0)
	if !budget.IsReserveCrossed() {
		t.Fatal("expected reserve crossed")
	}
	rem = budget.ReminderBlock()
	if !strings.Contains(rem, "reached the planning budget reserve") {
		t.Fatalf("expected reserve finalization prompt, got: %s", rem)
	}
}

func TestAssembleRequestMessagesStablePrefix(t *testing.T) {
	ag := New(nil, "m", 100, "base system")
	ag.PlanMode = true

	history := []llm.Message{
		{Role: "system", Content: "base system"},
		{Role: "user", Content: "user prompt"},
		{Role: "assistant", Content: "assistant thought"},
		{Role: "tool", Content: "tool result", ToolCallID: "tc-1"},
	}

	assembled := ag.assembleRequestMessages(history, "todo block", "", "<rollout_budget>reminder</rollout_budget>")
	if len(assembled) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(assembled))
	}
	if assembled[0].Content != "base system" {
		t.Fatalf("idx 0: %q, want base system", assembled[0].Content)
	}
	if assembled[1].Content != planModePrompt {
		t.Fatalf("idx 1: %q, want planModePrompt", assembled[1].Content)
	}
	if assembled[2].Content != "<rollout_budget>reminder</rollout_budget>" {
		t.Fatalf("idx 2: %q, want budget reminder", assembled[2].Content)
	}
	if assembled[3].Content != "user prompt" || assembled[4].Content != "assistant thought" || assembled[5].Content != "tool result" || assembled[6].Content != "todo block" {
		t.Fatalf("unexpected assembled messages: %+v", assembled)
	}
}

func TestPlanModeTerminatesOnBudgetReserve(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			// First call: model calls read tool, returns usage that crosses 10k reserve
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"agent.go\"}"}}]}}],"usage":{"prompt_tokens":1000,"completion_tokens":91000}}`+"\n\n")
		} else {
			// Second call: tools disabled, model emits final proposed plan
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Synthesized plan:\n\n<proposed_plan>\n# Plan\n1. Do it\n</proposed_plan>"}}],"finish_reason":"stop"}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.PlanMode = true

	final, err := ag.TurnAuthored(context.Background(), "make a plan", Events{})
	if err != nil {
		t.Fatalf("TurnAuthored failed: %v", err)
	}
	if !strings.Contains(final, "<proposed_plan>") {
		t.Fatalf("expected final plan, got: %s", final)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestPlanModeBudgetExhaustedError(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			// Cross reserve and exhaust budget
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"agent.go\"}"}}]}}],"usage":{"prompt_tokens":1000,"completion_tokens":100000}}`+"\n\n")
		} else {
			// Final synthesis request fails with 500 error
			http.Error(w, `{"error":{"message":"internal error"}}`, http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.PlanMode = true

	_, err := ag.TurnAuthored(context.Background(), "make a plan", Events{})
	if err == nil {
		t.Fatal("expected error on exhausted budget failure, got nil")
	}
	var exhausted *ErrPlanBudgetExhausted
	if !strings.Contains(err.Error(), "Plan rollout budget exhausted") {
		t.Fatalf("expected ErrPlanBudgetExhausted error message, got: %v", err)
	}
	_ = exhausted
}
