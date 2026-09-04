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
	"github.com/sacca97/ghg/internal/tools"
)

func TestParsePlanAndSeedTodos(t *testing.T) {
	p, err := ParsePlan(`{"goal":"ship it","steps":["write code","run tests"],"acceptance_checks":["tests pass"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "ship it" || len(p.Steps) != 2 || len(p.AcceptanceChecks) != 1 {
		t.Fatalf("parsed plan: %+v", p)
	}
	todos := p.Todos()
	if len(todos) != 2 || todos[0].Status != "in_progress" || todos[1].Status != "pending" {
		t.Fatalf("seeded todos: %+v", todos)
	}
	a := &Agent{}
	if err := a.SetTodos(todos); err != nil {
		t.Fatal(err)
	}
	if got := a.TodosJSON(); got == "" {
		t.Fatal("validated plan should be serializable")
	}
}
func TestPlanStreamParser(t *testing.T) {
	cases := []struct {
		name    string
		chunks  []string
		wantVis string
		wantPln string
	}{
		{
			name:    "plain text no block",
			chunks:  []string{"hello ", "world"},
			wantVis: "hello world",
			wantPln: "",
		},
		{
			name:    "single chunk full block",
			chunks:  []string{"Let me plan.\n\n<proposed_plan>\n- one\n- two\n</proposed_plan>\n"},
			wantVis: "Let me plan.\n\n\n",
			wantPln: "\n- one\n- two\n",
		},
		{
			name:    "block split across every byte",
			chunks:  splitEvery("<proposed_plan>\nstep 1\n</proposed_plan>", 1),
			wantVis: "",
			wantPln: "\nstep 1\n",
		},
		{
			name:    "text then block then text",
			chunks:  []string{"pre ", "<proposed_plan>", "A", "</proposed_plan>", " post"},
			wantVis: "pre  post",
			wantPln: "A",
		},
		{
			name:    "unclosed block at end",
			chunks:  []string{"text ", "<proposed_plan>", "abc"},
			wantVis: "text ",
			wantPln: "abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &planStreamParser{}
			var vis, pln strings.Builder
			p.visible = func(s string) { vis.WriteString(s) }
			p.onPlan = func(s string) { pln.WriteString(s) }
			for _, c := range tc.chunks {
				p.feed(c)
			}
			p.close()
			if vis.String() != tc.wantVis {
				t.Errorf("visible: got %q want %q", vis.String(), tc.wantVis)
			}
			if pln.String() != tc.wantPln {
				t.Errorf("plan: got %q want %q", pln.String(), tc.wantPln)
			}
		})
	}
}

func TestExtractProposedPlan(t *testing.T) {
	if body, ok := ExtractProposedPlan("no plan here"); ok || body != "" {
		t.Errorf("expected no plan, got ok=%v body=%q", ok, body)
	}
	body, ok := ExtractProposedPlan("intro\n<proposed_plan>\n- x\n</proposed_plan>\neta")
	if !ok || body != "- x" {
		t.Errorf("got ok=%v body=%q", ok, body)
	}
	if body, ok := ExtractProposedPlan("<proposed_plan>first</proposed_plan><proposed_plan>second</proposed_plan>"); !ok || body != "second" {
		t.Errorf("last block should win: got %q %v", body, ok)
	}
}

// splitEvery returns s split into chunks of at most n bytes.
func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

func TestPlanModeRestrictsTools(t *testing.T) {
	ag := &Agent{}
	ag.Tools = []tools.Tool{
		{Def: models.NewTool("read", "", "")},
		{Def: models.NewTool("grep", "", "")},
		{Def: models.NewTool("structural_search", "", "")},
		{Def: models.NewTool("bash", "", "")},
		{Def: models.NewTool("write", "", "")},
		{Def: models.NewTool("edit", "", "")},
		{Def: models.NewTool("todowrite", "", "")},
		{Def: models.NewTool("lsp", "", "")},
	}

	planTools := ag.planTools()
	if len(planTools) != 4 {
		t.Fatalf("expected 4 safe tools, got %d", len(planTools))
	}
	for _, pt := range planTools {
		name := pt.Def.Function.Name
		if name != "read" && name != "grep" && name != "structural_search" && name != "lsp" {
			t.Errorf("unexpected tool in plan mode: %s", name)
		}
	}
}

func TestAskModeAnswersWithReadOnlyTools(t *testing.T) {
	backend := &fakeBackend{}
	ag := New(backend, "model", 100, "system")
	ag.AskMode = true
	ag.Tools = []tools.Tool{
		{Def: models.NewTool("read", "", "")},
		{Def: models.NewTool("grep", "", "")},
		{Def: models.NewTool("bash", "", "")},
		{Def: models.NewTool("write", "", "")},
		{Def: models.NewTool("task", "", "")},
	}

	answer, err := ag.TurnAuthored(context.Background(), "what is this?", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "reply" {
		t.Fatalf("answer = %q, want reply", answer)
	}
	if len(backend.streamRequests) != 1 {
		t.Fatalf("stream calls = %d, want 1", len(backend.streamRequests))
	}
	req := backend.streamRequests[0]
	if !requestContains(req, askModePrompt) {
		t.Fatal("ask prompt was not included")
	}
	if len(req.Tools) != 2 {
		t.Fatalf("ask tools = %d, want 2", len(req.Tools))
	}
	for _, tool := range req.Tools {
		if tool.Function.Name != "read" && tool.Function.Name != "grep" {
			t.Fatalf("unexpected ask tool %q", tool.Function.Name)
		}
	}
}

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
	budget.RecordUsage(models.Usage{
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
	if budget.Remaining() != 190_000 {
		t.Fatalf("remaining = %f, want 190000", budget.Remaining())
	}

	// Spend 140,000 more units -> 150,000 used, 50,000 remaining.
	budget.RecordUsage(models.Usage{CompletionTokens: 140_000}, 0)
	rem := budget.ReminderBlock()
	if !strings.Contains(rem, "50000 weighted tokens remaining") {
		t.Fatalf("expected 50k reminder, got: %s", rem)
	}

	// Spend 11,000 more units -> 161,000 used, 39,000 remaining (<= 40,000 reserve)
	budget.RecordUsage(models.Usage{CompletionTokens: 11_000}, 0)
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

	history := []models.Message{
		{Role: "system", Content: "base system"},
		{Role: "user", Content: "user prompt"},
		{Role: "assistant", Content: "assistant thought"},
		{Role: "tool", Content: "tool result", ToolCallID: "tc-1"},
	}

	assembled := ag.assembleRequestMessages(history, "todo block", "", "<rollout_budget>reminder</rollout_budget>", "", "")
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
	var requests []models.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			// First call: model calls read tool, returns usage that crosses 40k reserve
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"agent.go\"}"}}]}}],"usage":{"prompt_tokens":1000,"completion_tokens":160000}}`+"\n\n")
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
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if len(requests[0].Tools) == 0 {
		t.Fatal("the request crossing the reserve must still expose exploration tools")
	}
	if len(requests[1].Tools) != 0 {
		t.Fatalf("final plan request exposed tools: %+v", requests[1].Tools)
	}
	if !requestContains(requests[1], "reached the planning budget reserve") {
		t.Fatal("final plan request is missing the terminal budget reminder")
	}

	t.Run("review", func(t *testing.T) {
		var requests []models.Request
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req models.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
				return
			}
			requests = append(requests, req)
			w.Header().Set("Content-Type", "text/event-stream")
			switch len(requests) {
			case 1:
				// Cross the weighted reserve after an exploratory read.
				fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"review-read-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"agent.go\"}"}}]}}],"usage":{"prompt_tokens":1600000}}`+"\n\n")
			case 2:
				// The model emitted a stale exploratory call despite the restricted schema.
				fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"review-read-2","type":"function","function":{"name":"read","arguments":"{\"path\":\"agent.go\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`+"\n\n")
			default:
				fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"review-submit-1","type":"function","function":{"name":"submit_review","arguments":"{\"summary\":\"ok\",\"verdict\":\"approve\",\"findings\":[]}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`+"\n\n")
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
		ag.ReviewMode = true
		final, err := ag.TurnAuthored(context.Background(), "review the change", Events{})
		if err != nil {
			t.Fatalf("review TurnAuthored failed: %v", err)
		}
		if !strings.Contains(final, `"verdict":"approve"`) {
			t.Fatalf("expected submitted review, got: %s", final)
		}
		if len(requests) != 3 {
			t.Fatalf("review requests = %d, want 3", len(requests))
		}
		if len(requests[0].Tools) == 0 {
			t.Fatal("the request crossing the weighted reserve must expose exploration tools")
		}
		if len(requests[1].Tools) != 1 || requests[1].Tools[0].Function.Name != "submit_review" {
			t.Fatalf("review finalization tools = %+v, want only submit_review", requests[1].Tools)
		}
		if !requestContains(requests[1], "reached the review budget reserve") {
			t.Fatal("review finalization request is missing the terminal budget reminder")
		}
		if !requestContains(requests[2], reviewFinalizationToolError) {
			t.Fatal("stale read result is missing the review finalization error")
		}
		if len(requests[2].Tools) != 1 || requests[2].Tools[0].Function.Name != "submit_review" {
			t.Fatalf("valid review request tools = %+v, want only submit_review", requests[2].Tools)
		}
	})
}

func requestContains(req models.Request, fragment string) bool {
	for _, msg := range req.Messages {
		if strings.Contains(msg.Content, fragment) {
			return true
		}
	}
	return false
}

func TestPlanModeBudgetExhaustedError(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			// Cross reserve and exhaust budget
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"agent.go\"}"}}]}}],"usage":{"prompt_tokens":1000,"completion_tokens":200000}}`+"\n\n")
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

func TestRolloutBudgetCallCeiling(t *testing.T) {
	// Model calls ceiling triggers reserve at 120 calls
	b := newPlanRolloutBudget()
	for i := 0; i < 120; i++ {
		b.RecordUsage(models.Usage{PromptTokens: 10, CompletionTokens: 10}, 0)
	}
	if !b.IsReserveCrossed() {
		t.Fatal("expected reserve crossed from call count ceiling")
	}
	if !strings.Contains(b.ReminderBlock(true), "reached the review budget reserve") {
		t.Fatalf("call ceiling should produce terminal reminder, got: %s", b.ReminderBlock(true))
	}
}

func TestParsePlanRejectsIncompleteOutput(t *testing.T) {
	for _, response := range []string{
		"not json",
		`{"goal":"ship it","steps":[],"acceptance_checks":["tests pass"]}`,
		`{"goal":"ship it","steps":["write code"],"acceptance_checks":[]}`,
	} {
		if _, err := ParsePlan(response); err == nil {
			t.Fatalf("expected invalid plan for %q", response)
		}
	}
}
