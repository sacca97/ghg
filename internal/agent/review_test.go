package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestProposeReviewRetriesWhenReviewerDoesNotSubmit(t *testing.T) {
	var attempts int
	reviewArgs := `{"summary":"code is clean","verdict":"approve","findings":[],"checks_performed":["go test"]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			// First attempt fails to call terminal tool
			fmt.Fprintln(w, `{"choices":[{"message":{"role":"assistant","content":"I think everything looks okay without calling submit_review."},"finish_reason":"stop"}]}`)
			return
		}
		// Second attempt calls submit_review
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"rev-1","type":"function","function":{"name":"submit_review","arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`, reviewArgs)
	}))
	defer ts.Close()

	be := testBackend(ts.URL, "test-key")
	reviewer := New(be, "test-model", 4096, "sys")
	reviewer.Role = "smart"

	rev, err := ProposeReview(context.Background(), reviewer, "review main.go", Events{})
	if err != nil {
		t.Fatalf("ProposeReview error: %v", err)
	}
	if rev.Verdict != "approve" || rev.Summary != "code is clean" {
		t.Fatalf("unexpected review output: %+v", rev)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}
