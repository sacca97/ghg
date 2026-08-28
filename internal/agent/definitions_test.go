package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
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

func TestLoadAgentDefinitionsPrecedenceAndBuiltInPlanner(t *testing.T) {
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
	planner, ok := defs[builtInPlannerName]
	if !ok || planner.Role != "smart" || planner.MaxRounds == 0 {
		t.Fatalf("built-in planner missing: %+v", planner)
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

func TestRunDefinitionStopsAtSubmitPlanAndReportsTelemetry(t *testing.T) {
	args := `{"goal":"ship it","steps":["write code"],"acceptance_checks":["tests pass"]}`
	backend := &definitionBackend{messages: []llm.Message{{
		Role: "assistant", StopReason: "tool_use", ToolCalls: []llm.ToolCall{definitionToolCall(args)},
	}}, usages: []llm.Usage{{PromptTokens: 11, CompletionTokens: 7}}}
	a := New(backend, "smart-model", 100, "system")
	a.Role, a.Provider, a.Protocol = "smart", "test-provider", "openai-chat-completions"
	var starts []ModelCallStart
	var ends []ModelCallEnd
	result, err := a.RunDefinition(context.Background(), "ship it", BuiltInPlannerDefinition(), Events{
		OnModelCallStart: func(call ModelCallStart) { starts = append(starts, call) },
		OnModelCallEnd:   func(call ModelCallEnd) { ends = append(ends, call) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TerminalName != "submit_plan" || string(result.TerminalArgs) != args {
		t.Fatalf("terminal result = %+v", result)
	}
	if len(backend.requests) != 1 || len(backend.requests[0].Tools) != 4 {
		t.Fatalf("planner request/tools = %d/%d", len(backend.requests), len(backend.requests[0].Tools))
	}
	for _, tool := range backend.requests[0].Tools {
		switch tool.Function.Name {
		case "read", "grep", "glob", "submit_plan":
		default:
			t.Fatalf("unexpected planner tool %q", tool.Function.Name)
		}
	}
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("telemetry start/end = %d/%d", len(starts), len(ends))
	}
	if starts[0].Role != "smart" || starts[0].Provider != "test-provider" || starts[0].Model != "smart-model" || starts[0].Protocol != "openai-chat-completions" {
		t.Fatalf("start telemetry = %+v", starts[0])
	}
	if ends[0].FinishReason != "tool_use" || ends[0].Usage.PromptTokens != 11 || ends[0].Usage.CompletionTokens != 7 {
		t.Fatalf("end telemetry = %+v", ends[0])
	}
}

func TestProposePlanRetriesWhenPlannerDoesNotSubmit(t *testing.T) {
	args := `{"goal":"ship it","steps":["write code"],"acceptance_checks":["tests pass"]}`
	backend := &definitionBackend{messages: []llm.Message{
		{Role: "assistant", Content: "not a terminal call"},
		{Role: "assistant", StopReason: "tool_use", ToolCalls: []llm.ToolCall{definitionToolCall(args)}},
	}}
	a := New(backend, "smart-model", 100, "system")
	plan, err := ProposePlan(context.Background(), a, "ship it", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Goal != "ship it" || len(backend.requests) != 2 {
		t.Fatalf("plan/retry = %+v/%d", plan, len(backend.requests))
	}
}

func definitionToolCall(args string) llm.ToolCall {
	var call llm.ToolCall
	call.ID = "plan-1"
	call.Type = "function"
	call.Function.Name = "submit_plan"
	call.Function.Arguments = args
	return call
}

type definitionBackend struct {
	mu       sync.Mutex
	requests []llm.Request
	messages []llm.Message
	usages   []llm.Usage
}

func (b *definitionBackend) Stream(context.Context, llm.Request, llm.EventSink) (llm.Message, llm.Usage, error) {
	return llm.Message{}, llm.Usage{}, nil
}

func (b *definitionBackend) Complete(_ context.Context, req llm.Request) (llm.Message, llm.Usage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, req)
	i := len(b.requests) - 1
	return b.messages[i], b.usagesAt(i), nil
}

func (b *definitionBackend) usagesAt(i int) llm.Usage {
	if i < len(b.usages) {
		return b.usages[i]
	}
	return llm.Usage{}
}
