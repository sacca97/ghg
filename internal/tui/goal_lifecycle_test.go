package tui

import (
	"path/filepath"
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/session"
)

// A prose claim never completes an active goal. Completion must arrive as a
// validated update_goal result carrying a verification note.
func TestGoalLoopRequiresStructuredCompletion(t *testing.T) {
	m := &model{goal: "ship the thing", goalRounds: 0}
	m.goalTurnFinished(turnDoneMsg{final: "Verified: all checks pass."}, false)
	record, ok := m.currentGoalRecord()
	if !ok || record.Status != goalstate.StatusActive {
		t.Fatalf("prose must not complete the goal: %+v", record)
	}
	if record.Rounds != 1 {
		t.Fatalf("rounds = %d, want 1", record.Rounds)
	}
}

func TestGoalLoopEndsOnStructuredCompletion(t *testing.T) {
	m := &model{goal: "ship the thing", goalRounds: 1}
	record, _ := m.currentGoalRecord()
	updates := []agent.GoalUpdate{{
		GoalID:   record.ID,
		Status:   goalstate.StatusComplete,
		Progress: "tests and the release check passed",
	}}
	if m.goalTurnFinished(turnDoneMsg{final: "done", goalUpdates: updates}, false) {
		t.Fatal("complete goal must not submit a continuation")
	}
	got, ok := m.currentGoalRecord()
	if !ok || got.Status != goalstate.StatusComplete {
		t.Fatalf("goal status = %+v, want complete", got)
	}
	if m.goal != "" {
		t.Fatalf("completed goal must leave the active compatibility mirror empty: %q", m.goal)
	}
}

func TestResumePausesActiveGoalUntilExplicitResume(t *testing.T) {
	m := compactCmdModel()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m.store = st

	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	record := goalstate.New("finish the migration")
	record.ID = "goal-resume"
	if err := st.CheckpointGoal(id, record); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LoadGoal(id)
	if err != nil || !ok || got.Status != goalstate.StatusPaused {
		t.Fatalf("resume should pause active work: %+v %v %v", got, ok, err)
	}
	if m.goal != "" {
		t.Fatalf("paused goal must not remain active in the compatibility mirror: %q", m.goal)
	}
	if !m.resumeGoal() {
		t.Fatal("explicit resume should reactivate a paused goal")
	}
	got, ok, err = st.LoadGoal(id)
	if err != nil || !ok || got.Status != goalstate.StatusActive {
		t.Fatalf("explicit resume status: %+v %v %v", got, ok, err)
	}
}
