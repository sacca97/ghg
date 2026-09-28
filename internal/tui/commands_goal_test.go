package tui

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func TestGoalHelpers(t *testing.T) {
	p := agent.ContinuePrompt("ship the feature")
	if !strings.Contains(p, "ship the feature") || !strings.Contains(p, "update_goal") {
		t.Fatalf("prompt: %q", p)
	}
	record := agent.NewGoal("ship the feature")
	if err := (agent.GoalUpdate{GoalID: record.ID, Status: agent.GoalStatusComplete, Progress: "verified"}).Validate(record.ID); err != nil {
		t.Fatalf("complete update should validate: %v", err)
	}
	if err := (agent.GoalUpdate{GoalID: record.ID, Status: agent.GoalStatusComplete}).Validate(record.ID); err == nil {
		t.Fatal("completion without verification should fail")
	}
}

func TestGoalFromContextMsgHandler(t *testing.T) {
	m := compactCmdModel()
	m.busy = true
	m.cancel = func() {}
	oldGoal := agent.NewGoal("paused old goal")
	oldGoal.Rounds = 20
	oldGoal.Status = agent.GoalStatusPaused
	m.applyGoalRecord(oldGoal)
	tm, cmd := m.Update(goalFromContextMsg{err: errors.New("boom")})
	m = tm.(*model)
	if cmd != nil {
		t.Fatal("a failed formulation must not submit anything")
	}
	if m.busy || m.cancel != nil {
		t.Fatal("the msg handler must clear busy/cancel on failure")
	}
	if m.goalRecord == nil || m.goalRecord.Objective != "paused old goal" {
		t.Fatalf("old goal must survive untouched, got %+v", m.goalRecord)
	}
	if out := lastBlock(m); !strings.Contains(out, "goal-from-context failed") {
		t.Fatalf("expected a failure note, got %q", out)
	}

	// esc-cancel is silent and does not replace the previous failure.
	m.busy, m.cancel = true, func() {}
	previous := lastBlock(m)
	tm, _ = m.Update(goalFromContextMsg{err: context.Canceled})
	m = tm.(*model)
	if m.busy || lastBlock(m) != previous {
		t.Fatalf("cancelled formulation should be silent: busy=%v last=%q", m.busy, lastBlock(m))
	}

	// success: goal trimmed, set, and submitted — busy stays owned by the
	// new turn. The submit's turn goroutine p.Sends on a nil prog, so it
	// must run to completion (or fail) without touching the assertions.
	m2 := compactCmdModel()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	go func() {
		dec := workerwire.NewDecoder(serverConn)
		for {
			if _, err := dec.Read(); err != nil {
				return
			}
		}
	}()
	m2.workerClient = workerwire.NewClient(clientConn, "test")
	m2.busy = true
	m2.cancel = func() {}
	m2.messages = []models.Message{{Role: "system", Content: "sys"}}
	tm2, cmd2 := m2.Update(goalFromContextMsg{goal: "  ship it  "})
	m2 = tm2.(*model)
	if cmd2 == nil {
		t.Fatal("a successful formulation must submit the goal (start the turn)")
	}
	if !m2.busy {
		t.Fatal("busy must stay set — it belongs to the submitted turn now")
	}
	if m2.currentGoal() != "ship it" {
		t.Fatalf("goal should be trimmed and set, got %q", m2.currentGoal())
	}
	found := false
	for _, b := range m2.blocks {
		if strings.Contains(b.text, "◎ goal set: ship it") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a goal-set note in the transcript")
	}
}

func TestGoalFromContextBusyRefuses(t *testing.T) {
	m := compactCmdModel()
	m.messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
	}
	m.busy = true
	m.command("/goal-from-context")
	if out := lastBlock(m); !strings.Contains(out, "busy") {
		t.Fatalf("expected a busy note, got %q", out)
	}
	if m.currentGoal() != "" {
		t.Fatal("busy refusal must not touch the goal")
	}
}
