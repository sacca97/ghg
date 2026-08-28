package goal

import "testing"

func TestUpdateValidationKeepsHostStatesHostControlled(t *testing.T) {
	for _, status := range []Status{StatusPaused, StatusUsageLimited, StatusBudgetLimited} {
		if err := (Update{Status: status, Progress: "note", Blocker: "blocker"}).Validate("goal-1"); err == nil {
			t.Fatalf("model must not set host status %q", status)
		}
	}
}

func TestUpdateValidationRequiresMeaningfulNotes(t *testing.T) {
	cases := []Update{
		{Status: StatusActive},
		{Status: StatusActive, Blocker: "something"},
		{Status: StatusBlocked},
		{Status: StatusComplete},
	}
	for _, update := range cases {
		if err := update.Validate("goal-1"); err == nil {
			t.Fatalf("update should require a meaningful note: %+v", update)
		}
	}
	if err := (Update{GoalID: "other", Status: StatusActive, Progress: "made progress"}).Validate("goal-1"); err == nil {
		t.Fatal("mismatched goal ID should fail")
	}
}

func TestRecordValidationAndLifecycleHelpers(t *testing.T) {
	record := New("ship it")
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.Status != StatusActive || record.Status.Terminal() || record.Status.Resumable() {
		t.Fatalf("new record: %+v", record)
	}
	if !StatusComplete.Terminal() || StatusComplete.Resumable() || !StatusPaused.Resumable() {
		t.Fatal("lifecycle helper mismatch")
	}
}
