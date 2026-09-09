package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

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

func TestReviewBudgetInventoryAndBaseline(t *testing.T) {
	workspace := t.TempDir()
	files := map[string]string{
		"a.go":                         "package a\n",
		"backend/service.go":           "package backend\nfunc Run() {}\n",
		"frontend/app.js":              "const app = true;\nconst ready = true;\nconst value = 1;\n",
		"frontend/index.html":          "<html>\n<body>\napp\n</body>\n",
		"frontend/styles.css":          "body {\n  color: red;\n}\nmain {}\nfooter {}\n",
		"desktop/main.m":               "#import <Foundation/Foundation.h>\nint main() {\nreturn 0;\n}\nvoid done() {}\nint value = 1;\n",
		"p_test.go":                    "package a\n",
		"frontend/app.test.js":         "it('works', () => {});\n",
		"docs/example.js":              "const documentation = true;\n",
		"frontend/bundle.generated.js": "const generated = true;\n",
		"frontend/blob.js":             "binary\x00data\n",
	}
	for name, content := range files {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name       string
		target     string
		inventory  ReviewInventory
		wantBudget int
	}{
		{name: "minimum", target: "review a.go", inventory: reviewInventoryAt(workspace, "a.go"), wantBudget: 6},
		{name: "broad scope", target: "review bugs optimization frontend backend", inventory: reviewInventoryAt(workspace, ""), wantBudget: 15},
		{name: "large-file bonus", target: "review", inventory: ReviewInventory{ProductionFiles: 15, ProductionLOC: 6000, LargeFiles: []string{"a.go", "b.go", "c.go"}, explicitScope: true}, wantBudget: 11},
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
	if inventory.ProductionFiles != 6 || inventory.TestFiles != 2 || inventory.ProductionLOC != 21 {
		t.Fatalf("inventory = %+v", inventory)
	}
	wantLanguages := map[string]ReviewLanguageStat{
		"Go": {Files: 2, LOC: 3}, "JavaScript": {Files: 1, LOC: 3},
		"HTML": {Files: 1, LOC: 4}, "CSS": {Files: 1, LOC: 5},
		"Objective-C": {Files: 1, LOC: 6},
	}
	if len(inventory.LanguageStats) != len(wantLanguages) {
		t.Fatalf("language stats = %+v", inventory.LanguageStats)
	}
	for language, want := range wantLanguages {
		if got := inventory.LanguageStats[language]; got != want {
			t.Errorf("%s stats = %+v, want %+v", language, got, want)
		}
	}
	for _, excluded := range []string{"docs/example.js", "frontend/bundle.generated.js", "frontend/blob.js"} {
		if slices.Contains(inventory.Files, excluded) {
			t.Errorf("excluded file %q was inventoried", excluded)
		}
	}
	if got := reviewExtensionAllocation(36, reviewExplorationHardMax); got != 2 {
		t.Fatalf("36-round lease = %d, want 2", got)
	}
	if got := reviewExtensionAllocation(reviewExplorationHardMax, reviewExplorationHardMax); got != 0 {
		t.Fatalf("hard-limit lease = %d, want 0", got)
	}
}

func TestReviewBudgetCheckpointAllowsUntouchedRequestedArea(t *testing.T) {
	prompt := reviewBudgetCheckpointReminder(&ReviewBudget{CurrentRound: 10, Allocation: 10, HardLimit: reviewExplorationHardMax})
	for _, phrase := range []string{
		"explicitly requested but untouched area",
		"frontend, backend, desktop, authentication, or UI",
		"name that area and the next files or entry points",
		"final evidence batch only for one narrow confirmation after requested areas have been sampled",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("checkpoint prompt missing %q: %s", phrase, prompt)
		}
	}
}

func TestReviewInventoryExcludesVSCodeOutput(t *testing.T) {
	workspace := t.TempDir()
	generated := filepath.Join(workspace, "editors", "vscode", "out")
	if err := os.MkdirAll(generated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generated, "extension.js"), []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "source.go"), []byte("package source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := reviewInventoryAt(workspace, "")
	foundSource, foundGenerated := false, false
	for _, path := range inventory.Files {
		foundSource = foundSource || path == "source.go"
		foundGenerated = foundGenerated || path == "editors/vscode/out/extension.js"
	}
	if !foundSource || foundGenerated {
		t.Fatalf("inventory files = %v", inventory.Files)
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
	initialAllocation := 19
	responses := make([]models.Message, 0, 47)
	for i := 0; i < initialAllocation; i++ {
		responses = append(responses, read(i))
	}
	for allocation := initialAllocation; allocation < reviewExplorationHardMax; allocation += reviewLeaseRounds {
		responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call(fmt.Sprintf("extend-%d", allocation), "request_review_extension", `{"unresolved_issue":"trace the boundary","evidence":"the rewind caller and persisted cutoff","remaining_lookup":"read the rewind path and compaction mapping","rounds":4}`)}})
		lease := reviewExtensionAllocation(allocation, reviewExplorationHardMax)
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
	if len(progress) < 2 || progress[0].Phase != "inventory" || progress[0].Allocation != initialAllocation || len(progress[0].Focus) != 5 || progress[1].Phase != "assessment" || progress[1].Allocation != initialAllocation {
		t.Fatalf("malformed deterministic budget progress = %+v", progress[:min(len(progress), 2)])
	}
	if got := backend.requests[initialAllocation].Tools[len(backend.requests[initialAllocation].Tools)-1].Function.Name; got != "request_review_extension" {
		t.Fatalf("checkpoint tools end with %q", got)
	}
	finalEvidenceIndex := len(responses) - 3
	if got := backend.requests[finalEvidenceIndex].Tools; len(got) != 2 || got[0].Function.Name != "read" || got[1].Function.Name != "submit_review" || !requestContains(backend.requests[finalEvidenceIndex], "review_budget_checkpoint") {
		t.Fatalf("final evidence tools = %+v", got)
	}
	for _, index := range []int{finalEvidenceIndex + 1, finalEvidenceIndex + 2} {
		if got := backend.requests[index].Tools; len(got) != 1 || got[0].Function.Name != "submit_review" {
			t.Fatalf("post-evidence tools at %d = %+v", index, got)
		}
	}
	if !requestContains(backend.requests[finalEvidenceIndex+2], reviewFinalizationToolError) {
		t.Fatalf("post-close navigation did not receive finalization error: %+v", backend.requests[finalEvidenceIndex+2].Messages)
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
	if extensions != 5 || lastAllocation != 38 || boundaries != 6 || finals != 1 || closed != 1 {
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

func TestReviewFinalizationRetriesWithoutReopeningExploration(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	if err := os.WriteFile(filepath.Join(workspace, "evidence.go"), []byte("package evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := newReviewFinalizationBackend(10, 1)
	ag := newReviewFinalizationAgent(backend)
	final, err := ag.TurnAuthored(context.Background(), "review evidence", Events{})
	if err != nil {
		t.Fatalf("Turn error: %v", err)
	}
	if final != reviewArgsForTest {
		t.Fatalf("final = %q, want %q", final, reviewArgsForTest)
	}
	if backend.calls != 11 {
		t.Fatalf("model calls = %d, want 11", backend.calls)
	}
	if len(ag.Messages) != 22 {
		t.Fatalf("messages after retry = %d, want 22", len(ag.Messages))
	}
}

func TestReviewFinalizationFailureRetainsEvidence(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	if err := os.WriteFile(filepath.Join(workspace, "evidence.go"), []byte("package evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := newReviewFinalizationBackend(10, 2)
	ag := newReviewFinalizationAgent(backend)
	_, err := ag.TurnAuthored(context.Background(), "review evidence", Events{})
	if err == nil || !strings.Contains(err.Error(), "review evidence was retained but final submission failed") {
		t.Fatalf("finalization error = %v", err)
	}
	if backend.calls != 11 {
		t.Fatalf("model calls = %d, want 11", backend.calls)
	}
	if len(ag.Messages) != 20 {
		t.Fatalf("retained evidence messages = %d, want 20", len(ag.Messages))
	}
	if !ag.ReviewPending() {
		t.Fatal("failed review did not retain continuation state")
	}

	ag.ReviewMode = true
	var resumed ReviewProgress
	final, err := ag.Continue(context.Background(), Events{OnReviewProgress: func(progress ReviewProgress) {
		if progress.Reason == "resumed" {
			resumed = progress
		}
	}})
	if err != nil {
		t.Fatalf("continued review error: %v", err)
	}
	if final != reviewArgsForTest {
		t.Fatalf("continued final = %q, want %q", final, reviewArgsForTest)
	}
	if resumed.CurrentRound == 0 {
		t.Fatal("continued review restarted at round zero")
	}
	last := backend.requests[len(backend.requests)-1]
	if len(last.Tools) != 1 || last.Tools[0].Function.Name != "submit_review" {
		t.Fatalf("continued finalization tools = %+v, want submit_review only", last.Tools)
	}
	if ag.ReviewPending() {
		t.Fatal("successful review retained stale continuation state")
	}
}

const reviewArgsForTest = `{"summary":"done","verdict":"approve","findings":[]}`

func newReviewFinalizationAgent(backend *reviewFinalizationBackend) *Agent {
	ag := New(backend, "review-model", 4096, "sys")
	ag.ReviewMode = true
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "evidence", nil },
	}}
	return ag
}

func newReviewFinalizationBackend(failAt, failures int) *reviewFinalizationBackend {
	responses := make([]models.Message, 0, 10)
	for i := 0; i < 9; i++ {
		responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{{
			ID: fmt.Sprintf("read-%d", i), Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"evidence.go"}`},
		}}})
	}
	responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{{
		ID: "submit", Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "submit_review", Arguments: reviewArgsForTest},
	}}})
	return &reviewFinalizationBackend{responses: responses, failAt: failAt, failures: failures}
}

type reviewFinalizationBackend struct {
	responses []models.Message
	failAt    int
	failures  int
	calls     int
	requests  []models.Request
}

func (b *reviewFinalizationBackend) Stream(_ context.Context, request models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
	b.calls++
	b.requests = append(b.requests, request)
	if b.calls >= b.failAt && b.calls < b.failAt+b.failures {
		return models.Message{}, models.Usage{}, errors.New("temporary finalization transport failure")
	}
	index := b.calls - 1
	if b.calls >= b.failAt {
		index -= b.failures
	}
	response := b.responses[index]
	return response, models.Usage{}, nil
}

func (b *reviewFinalizationBackend) Complete(ctx context.Context, req models.Request) (models.Message, models.Usage, error) {
	return b.Stream(ctx, req, models.EventSink{})
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

func TestReviewCompletionRetainsHistory(t *testing.T) {
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
	if compactionCalled {
		t.Fatal("review completion must not trigger compaction")
	}
	if rawMessagesCount != 0 || compactedSummary != "" {
		t.Fatalf("unexpected compaction callback data: count=%d summary=%q", rawMessagesCount, compactedSummary)
	}
	if len(ag.Messages) < 3 {
		t.Fatalf("expected raw history to remain available, got %d messages", len(ag.Messages))
	}
	if ag.compacted {
		t.Fatal("review completion must not mark the agent compacted")
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
	if len(notices) != 0 {
		t.Fatalf("unexpected review completion notice: %v", notices)
	}
}
