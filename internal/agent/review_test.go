package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	stringified, err := ParseReview(`{"summary":"ok","verdict":"approve","findings":[],"checks_performed":"[\"read tests\",\"ran go test\"]"}`)
	if err != nil || !slices.Equal(stringified.ChecksPerformed, []string{"read tests", "ran go test"}) {
		t.Fatalf("stringified checks = %v, err=%v", stringified.ChecksPerformed, err)
	}
	stringifiedFindings, err := ParseReview(`{"summary":"ok","verdict":"comment","findings":"[{\"title\":\"one\",\"severity\":\"low\"}]"}`)
	if err != nil || len(stringifiedFindings.Findings) != 1 || stringifiedFindings.Findings[0].Title != "one" {
		t.Fatalf("stringified findings = %+v, err=%v", stringifiedFindings.Findings, err)
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
		{"prose checks", `{"summary":"sum","verdict":"approve","findings":[],"checks_performed":"ran tests"}`},
		{"stringified non-array", `{"summary":"sum","verdict":"approve","findings":[],"checks_performed":"null"}`},
		{"non-string check", `{"summary":"sum","verdict":"approve","findings":[],"checks_performed":[1]}`},
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
		{name: "large-file bonus", target: "review", inventory: ReviewInventory{ProductionFiles: 15, ProductionLOC: 6000, LargeFiles: []string{"a.go", "b.go", "c.go"}, explicitScope: true}, wantBudget: 15},
		{name: "listed files set the floor", target: "review", inventory: ReviewInventory{Files: []string{"a.go", "a_test.go", "b.go", "b_test.go", "c.go", "c_test.go", "d.go", "d_test.go"}, ProductionFiles: 4, TestFiles: 4}, wantBudget: 8},
		{name: "partial stays bounded by inventory", target: "review", inventory: ReviewInventory{Partial: true}, wantBudget: 6},
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
	if got := reviewExtensionAllocation(38, 40, 20); got != 2 {
		t.Fatalf("capped lease = %d, want 2", got)
	}
	if got := reviewExtensionAllocation(20, 40, 20); got != 5 {
		t.Fatalf("quarter-budget lease = %d, want 5", got)
	}
	if got := reviewExtensionAllocation(20, 40, 5); got != 2 {
		t.Fatalf("rounded quarter-budget lease = %d, want 2", got)
	}
	if got := reviewExtensionAllocation(40, 40, 20); got != 0 {
		t.Fatalf("hard-limit lease = %d, want 0", got)
	}
}

func TestReviewBudgetCheckpointAllowsUntouchedRequestedArea(t *testing.T) {
	prompt := reviewBudgetCheckpointReminder(&ReviewBudget{CurrentRound: 10, Allocation: 10, HardLimit: 20})
	for _, phrase := range []string{
		"Budget is an estimate; sufficient semantic coverage is the completion criterion",
		"For a broad review, request an extension when a meaningful requested area remains unreviewed",
		"For a focused review, request one mainly when unresolved evidence could change a finding",
		"Do not extend merely for line-by-line completeness",
		"reason (incomplete_coverage or unresolved_finding)",
		"one concise why_it_matters sentence",
		"The final evidence batch is only for one narrow confirmation after requested areas have been sampled",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("checkpoint prompt missing %q: %s", phrase, prompt)
		}
	}
	for _, phrase := range []string{
		"Budget is an estimate; sufficient semantic coverage is the completion criterion",
		"Broad reviews may extend when meaningful requested areas remain unreviewed",
		"Focused reviews should extend mainly when unresolved evidence could change a finding",
		"Do not extend merely because the remaining code is unread if it adds no meaningful semantic coverage",
	} {
		if !strings.Contains(reviewModePrompt, phrase) {
			t.Errorf("review prompt missing %q: %s", phrase, reviewModePrompt)
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
	initialAllocation := 20
	hardLimit := 2 * initialAllocation
	responses := make([]models.Message, 0, 47)
	for i := 0; i < initialAllocation; i++ {
		responses = append(responses, read(i))
	}
	for allocation := initialAllocation; allocation < hardLimit; {
		responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call(fmt.Sprintf("extend-%d", allocation), "request_review_extension", `{"reason":"unresolved_finding","remaining_area":"rewind and compaction boundary","why_it_matters":"the persisted cutoff may affect which history is retained","remaining_lookup":"read the rewind path and compaction mapping"}`)}})
		lease := reviewExtensionAllocation(allocation, hardLimit, initialAllocation)
		for i := 0; i < lease; i++ {
			responses = append(responses, read(200+allocation+i))
		}
		allocation += lease
	}
	responses = append(responses,
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("pending-final", "read", `{"path":"file-0.go"}`)}},
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("final-read", "read", `{"path":"file-0.go"}`)}},
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("submit", "submit_review", `{"summary":"done","verdict":"approve","findings":[]}`)}},
	)
	backend := &checkpointBackend{responses: responses}
	// Keep enough stable prompt weight to cross the generic Plan reserve if
	// ReviewMode accidentally creates that unrelated budget.
	ag := New(backend, "model", 100, strings.Repeat("system ", 10000))
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
	preflight := func(request models.Request) string {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "<review_preflight>") {
				return message.Content
			}
		}
		return ""
	}
	if got := preflight(backend.requests[0]); got == "" || got != preflight(backend.requests[initialAllocation]) {
		t.Fatalf("review preflight changed after extension")
	}
	var sawGlob, sawFind bool
	for _, tool := range backend.requests[0].Tools {
		sawGlob = sawGlob || tool.Function.Name == "glob"
		sawFind = sawFind || tool.Function.Name == "find_files"
	}
	if !sawGlob || !sawFind {
		t.Fatalf("review discovery tools missing: glob=%t find_files=%t", sawGlob, sawFind)
	}
	if len(progress) < 2 || progress[0].Phase != "inventory" || progress[0].Allocation != initialAllocation || len(progress[0].Focus) != 5 || progress[1].Phase != "assessment" || progress[1].Allocation != initialAllocation {
		t.Fatalf("malformed deterministic budget progress = %+v", progress[:min(len(progress), 2)])
	}
	for i, request := range backend.requests {
		if !reflect.DeepEqual(request.Tools, backend.requests[0].Tools) {
			t.Fatalf("review tool schema changed at request %d: got %+v, baseline %+v", i, request.Tools, backend.requests[0].Tools)
		}
	}
	finalEvidenceIndex := len(responses) - 3
	if !requestContains(backend.requests[finalEvidenceIndex], "review_budget_checkpoint") {
		t.Fatalf("final evidence request omitted checkpoint reminder: %+v", backend.requests[finalEvidenceIndex].Messages)
	}
	hasExtension := false
	for _, tool := range backend.requests[0].Tools {
		hasExtension = hasExtension || tool.Function.Name == "request_review_extension"
	}
	if !hasExtension {
		t.Fatal("stable review schema omitted request_review_extension")
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
	if extensions != 4 || lastAllocation != 40 || boundaries != 5 || finals != 1 || closed != 1 {
		t.Fatalf("progress leases/boundaries/final/closed = %d/%d/%d/%d/%d", extensions, boundaries, lastAllocation, finals, closed)
	}
	for _, event := range progress[closedAt+1:] {
		if event.Phase == "budget_boundary" {
			t.Fatalf("closed review re-entered budget boundary: %+v", event)
		}
	}
	for _, event := range progress {
		if event.CurrentRound > hardLimit {
			t.Fatalf("exploration exceeded hard limit: %+v", event)
		}
	}
}

func TestReviewModeNormalTurn(t *testing.T) {
	reviewArgs := `{"summary":"all clean","verdict":"approve","findings":[]}`
	call := 0
	var backend *mockAgentBackend
	backend = &mockAgentBackend{streamFn: func(_ context.Context, req models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
		for _, tool := range req.Tools {
			name := tool.Function.Name
			if name == "write" || name == "edit" || name == "bash" {
				t.Errorf("mutating tool %s exposed in review mode", name)
			}
		}
		call++
		if call == 1 {
			return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("rev-invalid", "submit_review", `{invalid json}`)}}, models.Usage{}, nil
		}
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("rev-valid", "submit_review", reviewArgs)}}, models.Usage{}, nil
	}}
	ag := New(backend, "test-model", 4096, "sys")
	ag.ReviewMode = true

	final, err := ag.Turn(context.Background(), "review codebase", Events{})
	if err != nil {
		t.Fatalf("Turn error: %v", err)
	}
	if final != reviewArgs {
		t.Fatalf("final = %q, want %q", final, reviewArgs)
	}
	if len(backend.requests) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(backend.requests))
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
	for _, request := range backend.requests[9:] {
		if request.RequestTimeout != finalizationRequestTimeout {
			t.Fatalf("finalization timeout = %s, want %s", request.RequestTimeout, finalizationRequestTimeout)
		}
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
	if !reflect.DeepEqual(last.Tools, backend.requests[0].Tools) {
		t.Fatalf("continued review changed tool schema: %+v, baseline %+v", last.Tools, backend.requests[0].Tools)
	}
	if ag.ReviewPending() {
		t.Fatal("successful review retained stale continuation state")
	}
}

func TestRestoreReviewContinuationKeepsCheckpointState(t *testing.T) {
	ag := New(nil, "review-model", 4096, "sys")
	targetHash := reviewTargetHash("review target")
	if !ag.RestoreReviewContinuation("review target", []ReviewProgress{
		{TargetHash: targetHash, Phase: "inventory", Baseline: 8, Allocation: 8, HardLimit: 38, Inventory: &ReviewInventory{Scope: []string{"."}}},
		{TargetHash: targetHash, Phase: "exploration", CurrentRound: 7, Allocation: 8, HardLimit: 38},
		{TargetHash: targetHash, Phase: "budget_boundary", CurrentRound: 8, Allocation: 8, HardLimit: 38},
	}) {
		t.Fatal("failed to restore review continuation")
	}
	if !ag.ReviewPending() {
		t.Fatal("restored review is not pending")
	}
	state := ag.reviewContinuation
	if state.target != "review target" || state.budget.CurrentRound != 8 || state.budget.Allocation != 8 || !state.checkpointPending || state.closed {
		t.Fatalf("restored review state = %+v", state)
	}
	if ag.RestoreReviewContinuation("completed", []ReviewProgress{{Phase: "completed", CurrentRound: 8, Allocation: 8, HardLimit: 38}}) {
		t.Fatal("completed review should not be restored")
	}
	if ag.RestoreReviewContinuation("another target", []ReviewProgress{{TargetHash: targetHash, Phase: "budget_boundary", CurrentRound: 8, Allocation: 8, HardLimit: 38}}) {
		t.Fatal("review progress for another target should not be restored")
	}
	completed := New(nil, "review-model", 4096, "sys")
	completed.Messages = []models.Message{
		{Role: "user", Authored: true, Content: "review target"},
		{Role: "tool", Name: "submit_review", Content: "Review accepted."},
	}
	if completed.RestoreReviewContinuation("review target", []ReviewProgress{{Phase: "exploration_closed", CurrentRound: 8, Allocation: 8, HardLimit: 38}}) {
		t.Fatal("accepted legacy review should not be restored")
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

func TestReviewCheckpointKeepsReasoningSelectorAvailable(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	if err := os.WriteFile(filepath.Join(workspace, "evidence.go"), []byte("package evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checkpointSeen := false
	backend := &mockAgentBackend{}
	backend.streamFn = func(_ context.Context, req models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
		if !requestContains(req, "<review_budget_checkpoint>") {
			return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("read", "read", `{"path":"evidence.go"}`)}}, models.Usage{}, nil
		}
		if !checkpointSeen {
			checkpointSeen = true
			for _, tool := range req.Tools {
				if tool.Function.Name == nextReasoningEffortToolName {
					return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("effort", nextReasoningEffortToolName, `{"effort":"high"}`)}}, models.Usage{}, nil
				}
			}
			t.Fatal("review checkpoint omitted the reasoning selector")
		}
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("submit", "submit_review", reviewArgsForTest)}}, models.Usage{}, nil
	}

	ag := New(backend, "model", 100, "system")
	ag.ReviewMode = true
	ag.Effort = "low"
	ag.ReasoningEfforts = []string{"low", "high"}
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "evidence", nil },
	}}
	var selections []ReasoningSelection
	if _, err := ag.TurnAuthored(context.Background(), "evidence.go", Events{
		OnReasoningSelection: func(value ReasoningSelection) { selections = append(selections, value) },
	}); err != nil {
		t.Fatal(err)
	}
	if !checkpointSeen {
		t.Fatal("review never reached a budget checkpoint")
	}
	if len(selections) != 1 || !selections[0].Applied || selections[0].Requested != "high" {
		t.Fatalf("selector result = %+v, want one applied high-effort selection", selections)
	}
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
