package session

import (
	"path/filepath"
	"testing"

	goalstate "github.com/sacca97/ghg/internal/goal"
)

func TestGoalLifecycleRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sessionID, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	record := goalstate.New("ship the feature")
	record.ID = "goal-1"
	record.Rounds = 2
	record.PromptTokens = 100
	record.CachedTokens = 25
	record.CompletionTokens = 40
	record.Progress = "implementation is complete; verification remains"
	if err := st.CheckpointGoal(sessionID, record); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.LoadGoal(sessionID)
	if err != nil || !ok {
		t.Fatalf("load goal: %+v %v %v", got, ok, err)
	}
	if got.ID != record.ID || got.Status != goalstate.StatusActive || got.Rounds != 2 ||
		got.PromptTokens != 100 || got.CachedTokens != 25 || got.CompletionTokens != 40 ||
		got.Progress != record.Progress {
		t.Fatalf("goal did not round-trip: %+v", got)
	}

	record.Status = goalstate.StatusBlocked
	record.Blocker = "the deployment environment is unavailable"
	if err := st.CheckpointGoal(sessionID, record); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := st.GoalCheckpoints(sessionID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 || checkpoints[0].Status != goalstate.StatusActive || checkpoints[1].Status != goalstate.StatusBlocked {
		t.Fatalf("checkpoints: %+v", checkpoints)
	}

	if err := st.ClearGoal(sessionID); err != nil {
		t.Fatal(err)
	}
	got, ok, err = st.LoadGoal(sessionID)
	if err != nil || !ok || got.Status != goalstate.StatusPaused || got.Blocker != "cleared by user" {
		t.Fatalf("clear should preserve a paused record: %+v %v %v", got, ok, err)
	}
	meta, _, err := st.Load(sessionID)
	if err != nil || meta.Goal != "" {
		t.Fatalf("legacy active mirror should be empty after clear: %+v %v", meta, err)
	}
	checkpoints, err = st.GoalCheckpoints(sessionID, record.ID)
	if err != nil || len(checkpoints) != 3 || checkpoints[2].Status != goalstate.StatusPaused {
		t.Fatalf("clear checkpoint: %+v %v", checkpoints, err)
	}
}

func TestLegacyGoalMigration(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sessionID, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM goals WHERE session_id=?`, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE sessions SET goal=? WHERE id=?`, "legacy objective", sessionID); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyGoals(st.db); err != nil {
		t.Fatal(err)
	}
	record, ok, err := st.LoadGoal(sessionID)
	if err != nil || !ok || record.ID != "legacy-"+sessionID || record.Status != goalstate.StatusActive || record.Objective != "legacy objective" {
		t.Fatalf("legacy record: %+v %v %v", record, ok, err)
	}
}

func TestForkAndDeleteCopyGoalLedger(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	source, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	record := goalstate.New("copy this goal")
	record.ID = "goal-copy"
	if err := st.CheckpointGoal(source, record); err != nil {
		t.Fatal(err)
	}
	record.Status = goalstate.StatusComplete
	record.Progress = "verified"
	if err := st.CheckpointGoal(source, record); err != nil {
		t.Fatal(err)
	}

	fork, err := st.Fork(source, 0, "fork")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LoadGoal(fork)
	if err != nil || !ok || got.ID != record.ID || got.Status != goalstate.StatusComplete {
		t.Fatalf("forked goal: %+v %v %v", got, ok, err)
	}
	checkpoints, err := st.GoalCheckpoints(fork, record.ID)
	if err != nil || len(checkpoints) != 2 {
		t.Fatalf("forked checkpoints: %+v %v", checkpoints, err)
	}
	if err := st.DeleteSession(fork); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.LoadGoal(fork); err != nil || ok {
		t.Fatalf("deleted goal still loaded: %v %v", ok, err)
	}
}
