package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/llm"
)

type planTestBackend struct {
	mu       sync.Mutex
	plans    []string
	complete []llm.Request
	stream   chan llm.Request
	done     chan struct{}
}

func (b *planTestBackend) Stream(_ context.Context, req llm.Request, sink llm.EventSink) (llm.Message, llm.Usage, error) {
	if b.stream != nil {
		b.stream <- req
	}
	if sink.OnText != nil {
		sink.OnText("done")
	}
	if b.done != nil {
		close(b.done)
	}
	return llm.Message{Role: "assistant", Content: "done"}, llm.Usage{}, nil
}

func (b *planTestBackend) Complete(_ context.Context, req llm.Request) (llm.Message, llm.Usage, error) {
	b.mu.Lock()
	b.complete = append(b.complete, req)
	if len(b.plans) == 0 {
		b.mu.Unlock()
		return llm.Message{}, llm.Usage{}, nil
	}
	response := b.plans[0]
	b.plans = b.plans[1:]
	b.mu.Unlock()
	if strings.TrimSpace(response) != "not json" {
		var call llm.ToolCall
		call.ID = "plan"
		call.Type = "function"
		call.Function.Name = "submit_plan"
		call.Function.Arguments = response
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}}, llm.Usage{}, nil
	}
	return llm.Message{Role: "assistant", Content: response}, llm.Usage{}, nil
}

func TestPlanThenExecuteUsesProposalAndSeedsTodos(t *testing.T) {
	b := &planTestBackend{
		plans:  []string{`{"goal":"ship it","steps":["write code","run tests"],"acceptance_checks":["tests pass"]}`},
		stream: make(chan llm.Request),
		done:   make(chan struct{}),
	}
	m := &model{agent: agent.New(b, "model", 100, "sys")}

	if _, _ = m.command("/plan ship it"); m.proposedPlan == nil {
		t.Fatal("/plan should retain the validated proposal")
	}
	if m.busy {
		t.Fatal("planning should be idle after an inline completion")
	}

	_, _ = m.command("/execute")
	select {
	case req := <-b.stream:
		if req.Model != "model" {
			t.Fatalf("execution request model: %q", req.Model)
		}
		if len(req.Tools) == 0 {
			t.Fatal("execution should use the normal acting tool set")
		}
	case <-time.After(time.Second):
		t.Fatal("execution did not reach the backend")
	}
	<-b.done
	if m.agent.Role != "fast" {
		t.Fatalf("execution should select the fast role, got %q", m.agent.Role)
	}
	if len(m.agent.Todos) != 2 || m.agent.Todos[0].Status != "in_progress" || m.agent.Todos[1].Status != "pending" {
		t.Fatalf("execution todos: %+v", m.agent.Todos)
	}
}

func TestPlanRetriesInvalidProposalOnce(t *testing.T) {
	b := &planTestBackend{plans: []string{
		"not json",
		`{"goal":"ship it","steps":["write code"],"acceptance_checks":["tests pass"]}`,
	}}
	planner := agent.New(b, "smart-model", 100, "sys")
	if _, err := requestPlan(context.Background(), planner, nil, "ship it"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.complete) != 2 {
		t.Fatalf("invalid output should be retried once, got %d calls", len(b.complete))
	}
}
