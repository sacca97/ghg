package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
)

func TestSuggestTool(t *testing.T) {
	cands := []string{"bash", "read", "edit", "mcp__docs__greet", "mcp__docs__fail", "mcp__github__create_issue"}
	tests := []struct {
		name  string
		cands []string
		want  []string
	}{
		{"mcp__docs__gret", nil, []string{"mcp__docs__greet"}},                // 1 edit
		{"mcp__doc__greet", nil, []string{"mcp__docs__greet"}},                // 1 edit
		{"mcp__docs__greet2", nil, []string{"mcp__docs__greet"}},              // 1 edit
		{"mcp__docs__", nil, []string{"mcp__docs__greet", "mcp__docs__fail"}}, // prefix
		{"mcp__github__create_iss", nil, []string{"mcp__github__create_issue"}},
		{"completely_unrelated_xyz", nil, nil}, // nothing close
		{"bsh", nil, []string{"bash"}},
		{"bash", []string{"read", "edit", "lsp"}, nil}, // short name requires d <= 1; bash must not suggest lsp
	}
	for _, tt := range tests {
		tc := tt.cands
		if tc == nil {
			tc = cands
		}
		got := SuggestTool(tt.name, tc)
		if len(got) != len(tt.want) {
			t.Errorf("SuggestTool(%q) = %v, want %v", tt.name, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("SuggestTool(%q) = %v, want %v", tt.name, got, tt.want)
				break
			}
		}
	}
}

func TestSuggestToolCapsAtTwo(t *testing.T) {
	cands := []string{"mcp__s__aaa", "mcp__s__aab", "mcp__s__aac"}
	if got := SuggestTool("mcp__s__aa", cands); len(got) > 2 {
		t.Errorf("got %v, want at most 2", got)
	}
}

func TestLevenshteinCap(t *testing.T) {
	if d := levenshtein("abc", "abc", 2); d != 0 {
		t.Errorf("identical = %d", d)
	}
	if d := levenshtein("abc", "abd", 2); d != 1 {
		t.Errorf("1 edit = %d", d)
	}
	if d := levenshtein("short", "a-much-longer-string", 3); d <= 3 {
		t.Errorf("should exceed cap, got %d", d)
	}
	if d := levenshtein("", "abcdef", 2); d <= 2 {
		t.Errorf("length gap beyond cap = %d", d)
	}
}

func TestExecuteSuggestsOnUnknownTool(t *testing.T) {
	docs := Tool{Def: models.NewTool("mcp__docs__greet", "", `{}`)}
	out := Execute(context.Background(), []Tool{docs}, "mcp__doc__greet", nil)
	if !strings.Contains(out, `unknown tool "mcp__doc__greet"`) || !strings.Contains(out, "did you mean mcp__docs__greet") {
		t.Errorf("got %q", out)
	}
	other := Tool{Def: models.NewTool("mcp__other__greet", "", `{}`)}
	out = Execute(context.Background(), []Tool{other}, "mcp__other__grete", nil)
	if !strings.Contains(out, "did you mean mcp__other__greet") {
		t.Errorf("current tool set was not used: %q", out)
	}
	out = Execute(context.Background(), []Tool{docs}, "nope", nil)
	if strings.Contains(out, "did you mean") {
		t.Errorf("unrelated tool should have no suggestion, got %q", out)
	}
}
