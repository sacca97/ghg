package agent

import "testing"

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
