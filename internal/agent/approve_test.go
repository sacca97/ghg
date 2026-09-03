package agent

import (
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/tools"
)

func TestParseApprovalResultIsStrictAndBounded(t *testing.T) {
	got, err := parseApprovalResult(`{"decision":"approve_once","reason":"one-shot cache download","confidence":0.91}`)
	if err != nil || got.Decision != tools.ApprovalApproveOnce || got.Confidence != 0.91 {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	for _, input := range []string{
		`Here is the decision: {"decision":"approve_once","reason":"x","confidence":1}`,
		`{"decision":"approve_once","reason":"","confidence":1}`,
		`{"decision":"approve_once","reason":"x","confidence":0.7,"extra":{}} trailing`,
		`{"decision":"approve_once","reason":"x","confidence":0.7,"extra":{}}`,
		`{"decision":"approve_once","reason":"x","confidence":0.7} {"decision":"deny","reason":"x","confidence":1}`,
		`{"decision":"approve_once","reason":"x","confidence":2}`,
		`{"decision":"maybe","reason":"x","confidence":1}`,
	} {
		if _, err := parseApprovalResult(input); err == nil {
			t.Errorf("parseApprovalResult(%q) unexpectedly succeeded", input)
		}
	}
	if _, err := parseApprovalResult(strings.Repeat("x", 4097)); err == nil {
		t.Fatal("oversized reviewer response should fail closed")
	}
}
