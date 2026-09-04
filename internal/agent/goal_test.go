package agent

import (
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
)

func TestUpdateValidationKeepsHostStatesHostControlled(t *testing.T) {
	for _, status := range []GoalStatus{GoalStatusPaused, GoalStatusUsageLimited, GoalStatusBudgetLimited} {
		if err := (GoalUpdate{Status: status, Progress: "note", Blocker: "blocker"}).Validate("goal-1"); err == nil {
			t.Fatalf("model must not set host status %q", status)
		}
	}
}

func TestUpdateValidationRequiresMeaningfulNotes(t *testing.T) {
	cases := []GoalUpdate{
		{Status: GoalStatusActive},
		{Status: GoalStatusActive, Blocker: "something"},
		{Status: GoalStatusBlocked},
		{Status: GoalStatusComplete},
	}
	for _, update := range cases {
		if err := update.Validate("goal-1"); err == nil {
			t.Fatalf("update should require a meaningful note: %+v", update)
		}
	}
	if err := (GoalUpdate{GoalID: "other", Status: GoalStatusActive, Progress: "made progress"}).Validate("goal-1"); err == nil {
		t.Fatal("mismatched goal ID should fail")
	}
}

func TestRecordValidationAndLifecycleHelpers(t *testing.T) {
	record := NewGoal("ship it")
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.Status != GoalStatusActive || record.Status.Terminal() || record.Status.Resumable() {
		t.Fatalf("new record: %+v", record)
	}
	if !GoalStatusComplete.Terminal() || GoalStatusComplete.Resumable() || !GoalStatusPaused.Resumable() {
		t.Fatal("lifecycle helper mismatch")
	}
}

func TestGoalFromContextPrompt(t *testing.T) {
	call := models.ToolCall{}
	call.Function.Name = "bash"
	call.Function.Arguments = `{"cmd":"go test ./..."}`
	tail := []models.Message{
		{Role: "user", Content: "make the tests green"},
		{Role: "assistant", Content: "I'll fix the flaky test and run go test.", ToolCalls: []models.ToolCall{call}},
	}
	p := BuildGoalFromContextPrompt(tail)
	for _, want := range []string{"make the tests green", "flaky test", "assistant called bash(", "ONLY the goal"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}

	msgs := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "older"},
		{Role: "user", Content: "recent ask"},
		{Role: "assistant", Content: "recent reply"},
	}
	got, err := GoalFromContextMessages(msgs, 2)
	if err != nil || len(got) != 2 || got[0].Content != "recent ask" || got[1].Content != "recent reply" {
		t.Fatalf("window: %v %v", got, err)
	}
	got, err = GoalFromContextMessages(msgs, 50)
	if err != nil || len(got) != 4 || got[0].Content != "old" {
		t.Fatalf("clamped window: %v %v", got, err)
	}
	got, err = GoalFromContextMessages(msgs, 0)
	if err != nil || len(got) != 4 {
		t.Fatalf("default window: %v %v", got, err)
	}
	if _, err := GoalFromContextMessages(msgs[:2], 8); err == nil {
		t.Fatal("two conversation messages required")
	}
}
