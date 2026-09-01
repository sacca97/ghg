package agent

import (
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

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
		{Def: llm.NewTool("read", "", "")},
		{Def: llm.NewTool("grep", "", "")},
		{Def: llm.NewTool("bash", "", "")},
		{Def: llm.NewTool("write", "", "")},
		{Def: llm.NewTool("edit", "", "")},
		{Def: llm.NewTool("todowrite", "", "")},
		{Def: llm.NewTool("lsp", "", "")},
	}

	planTools := ag.planTools()
	if len(planTools) != 3 {
		t.Fatalf("expected 3 safe tools, got %d", len(planTools))
	}
	for _, pt := range planTools {
		name := pt.Def.Function.Name
		if name != "read" && name != "grep" && name != "lsp" {
			t.Errorf("unexpected tool in plan mode: %s", name)
		}
	}
}
