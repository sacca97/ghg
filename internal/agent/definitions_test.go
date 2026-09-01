package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDefinition(t *testing.T, dir, file, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentDefinitionsPrecedenceAndBuiltInReviewer(t *testing.T) {
	project, user := t.TempDir(), t.TempDir()
	writeDefinition(t, project, "review.md", `---
name: review
description: project reviewer
role: smart
tools: [read, grep]
max_rounds: 3
---
Project prompt.
`)
	writeDefinition(t, user, "review.md", `---
name: review
description: user reviewer
role: tiny
tools: [read]
max_rounds: 1
---
User prompt.
`)
	writeDefinition(t, user, "quick.md", `---
name: quick
description: quick helper
role: fast
tools: []
max_rounds: 1
---
Quick prompt.
`)

	defs, err := LoadAgentDefinitions(DefinitionLoadOptions{
		ProjectDir: project, UserDir: user, ProjectTrusted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := defs["review"]; got.Description != "project reviewer" || got.Prompt != "Project prompt." {
		t.Fatalf("project definition should win: %+v", got)
	}
	if got := defs["quick"]; got.Role != "fast" || len(got.Tools) != 0 {
		t.Fatalf("user definition: %+v", got)
	}
	reviewer, ok := defs[builtInReviewerName]
	if !ok || reviewer.Role != "smart" || reviewer.MaxRounds == 0 {
		t.Fatalf("built-in reviewer missing: %+v", reviewer)
	}
}

func TestLoadAgentDefinitionsRejectsUnknownTool(t *testing.T) {
	dir := t.TempDir()
	writeDefinition(t, dir, "bad.md", `---
name: bad
description: bad helper
role: tiny
tools: [read, imaginary]
max_rounds: 1
---
Prompt.
`)
	_, err := LoadAgentDefinitions(DefinitionLoadOptions{UserDir: dir})
	if err == nil || !strings.Contains(err.Error(), `unknown tool "imaginary"`) {
		t.Fatalf("unknown tool should be a load error, got %v", err)
	}
}

func TestSubagentGuidanceMatchesBoundedExplorationTools(t *testing.T) {
	prompt := subagentPrompt()
	for _, fragment := range []string{"grep", "glob", "find_files", "lsp", "lsp_rename", "bounded read", "observed edit ranges"} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("subagent prompt lacks %q: %s", fragment, prompt)
		}
	}
	parent := New(nil, "model", 100, "system")
	description := taskTool(parent).Def.Function.Description
	for _, fragment := range []string{"grep", "glob", "find_files", "lsp", "lsp_rename", "observed edit ranges"} {
		if !strings.Contains(description, fragment) {
			t.Errorf("task description lacks %q: %s", fragment, description)
		}
	}
}
