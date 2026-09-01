package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/llm"
)

type planTestBackend struct {
	mu     sync.Mutex
	stream chan llm.Request
	done   chan struct{}
}

func (b *planTestBackend) Stream(_ context.Context, req llm.Request, sink llm.EventSink) (llm.Message, llm.Usage, error) {
	if b.stream != nil {
		b.stream <- req
	}
	if sink.OnText != nil {
		sink.OnText("plan text\n<proposed_plan>\n# Plan: ship it\n1. write code\n2. run tests\n</proposed_plan>")
	}
	if b.done != nil {
		select {
		case b.done <- struct{}{}:
		default:
		}
	}
	return llm.Message{Role: "assistant", Content: "plan text\n<proposed_plan>\n# Plan: ship it\n1. write code\n2. run tests\n</proposed_plan>"}, llm.Usage{}, nil
}

func (b *planTestBackend) Complete(_ context.Context, req llm.Request) (llm.Message, llm.Usage, error) {
	return llm.Message{Role: "assistant", Content: "done"}, llm.Usage{}, nil
}

func TestPlanThenExecuteConversational(t *testing.T) {
	b := &planTestBackend{
		stream: make(chan llm.Request, 2),
		done:   make(chan struct{}),
	}
	m := &model{
		agent: agent.New(b, "model", 100, "sys"),
		input: newInput(),
	}

	// 1. /plan switches to plan mode and submits goal
	m.command("/plan ship it")
	if m.uiMode() != uiModePlan {
		t.Fatalf("expected mode plan, got %q", m.uiMode())
	}
	if !m.agent.PlanMode {
		t.Fatal("expected agent.PlanMode to be true")
	}

	// Simulate turn completion
	finalMsg := "Here is the plan:\n<proposed_plan>\n# Plan: ship it\n1. write code\n2. run tests\n</proposed_plan>"
	m.Update(turnDoneMsg{final: finalMsg})

	if m.proposedPlanMD == "" || !strings.Contains(m.proposedPlanMD, "# Plan: ship it") {
		t.Fatalf("expected proposedPlanMD to contain plan, got: %q", m.proposedPlanMD)
	}

	// 2. /execute switches to execute mode and submits approved plan
	m.command("/execute")
	if m.uiMode() != uiModeExecute {
		t.Fatalf("expected mode execute, got %q", m.uiMode())
	}
	if m.agent.PlanMode {
		t.Fatal("expected agent.PlanMode to be false after /execute")
	}
}

func TestExecuteWithoutPlanFails(t *testing.T) {
	m := &model{
		agent: agent.New(&planTestBackend{}, "model", 100, "sys"),
		input: newInput(),
	}

	m.command("/execute")
	var foundErr bool
	for _, blk := range m.blocks {
		if strings.Contains(blk.text, "no plan to execute") {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Fatalf("expected 'no plan to execute' error, got blocks: %+v", m.blocks)
	}
}
