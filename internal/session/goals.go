package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	goalstate "github.com/sacca97/ghg/internal/goal"
)

// GoalCheckpoint is one append-only snapshot of a goal's lifecycle and
// accounting. The current state is available through LoadGoal; checkpoints
// make progress and blockers inspectable after resume or a process failure.
type GoalCheckpoint struct {
	Seq              int
	GoalID           string
	Status           goalstate.Status
	Rounds           int
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	Progress         string
	Blocker          string
	CreatedAt        time.Time
}

// migrateLegacyGoals gives pre-Phase-2.5 sessions a stable goal identity. The
// old sessions.goal column remains a compatibility mirror; new code reads the
// structured ledger first.
func migrateLegacyGoals(db *sql.DB) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO goals
		(session_id, goal_id, objective, status, rounds, usage_in, usage_cached,
		 usage_out, progress, blocker, created_at, updated_at)
		SELECT id, 'legacy-' || id, TRIM(goal), ?, 0, 0, 0, 0, '', '',
		 CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		FROM sessions WHERE TRIM(goal) <> ''`, goalstate.StatusActive)
	if err != nil {
		return fmt.Errorf("migrate legacy goals: %w", err)
	}
	return nil
}

// LoadGoal returns the most recently updated goal for a session. A missing
// goal is not an error; callers use the boolean to distinguish a fresh
// session from a database failure.
func (s *Store) LoadGoal(sessionID string) (goalstate.Record, bool, error) {
	var record goalstate.Record
	var status, created, updated string
	err := s.db.QueryRowContext(context.Background(), `SELECT goal_id, objective,
		status, rounds, usage_in, usage_cached, usage_out, progress, blocker,
		created_at, updated_at FROM goals WHERE session_id=?
		ORDER BY updated_at DESC, created_at DESC, goal_id DESC LIMIT 1`, sessionID).
		Scan(&record.ID, &record.Objective, &status, &record.Rounds,
			&record.PromptTokens, &record.CachedTokens, &record.CompletionTokens,
			&record.Progress, &record.Blocker, &created, &updated)
	if err != nil {
		if err == sql.ErrNoRows {
			return goalstate.Record{}, false, nil
		}
		return goalstate.Record{}, false, fmt.Errorf("load goal %s: %w", sessionID, err)
	}
	record.Status = goalstate.Status(status)
	record.CreatedAt, err = parseGoalTime(created)
	if err != nil {
		return goalstate.Record{}, false, fmt.Errorf("load goal %s created_at: %w", sessionID, err)
	}
	record.UpdatedAt, err = parseGoalTime(updated)
	if err != nil {
		return goalstate.Record{}, false, fmt.Errorf("load goal %s updated_at: %w", sessionID, err)
	}
	if err := record.Validate(); err != nil {
		return goalstate.Record{}, false, fmt.Errorf("load goal %s: %w", sessionID, err)
	}
	return record, true, nil
}

// SaveGoal writes the current goal state without adding a checkpoint. It is
// used by ordinary session persistence, while lifecycle transitions use
// CheckpointGoal below.
func (s *Store) SaveGoal(sessionID string, record goalstate.Record) error {
	return s.writeGoal(sessionID, record, false)
}

// CheckpointGoal atomically updates the current state and appends a durable
// progress/blocker checkpoint. The session's legacy goal column is updated as
// a compatibility mirror for old pickers and databases.
func (s *Store) CheckpointGoal(sessionID string, record goalstate.Record) error {
	return s.writeGoal(sessionID, record, true)
}

func (s *Store) writeGoal(sessionID string, record goalstate.Record, checkpoint bool) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	if err := record.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO goals
		(session_id, goal_id, objective, status, rounds, usage_in, usage_cached,
		 usage_out, progress, blocker, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id, goal_id) DO UPDATE SET
		 objective=excluded.objective, status=excluded.status,
		 rounds=excluded.rounds, usage_in=excluded.usage_in,
		 usage_cached=excluded.usage_cached, usage_out=excluded.usage_out,
		 progress=excluded.progress, blocker=excluded.blocker,
		 updated_at=excluded.updated_at`,
		sessionID, record.ID, record.Objective, record.Status, record.Rounds,
		record.PromptTokens, record.CachedTokens, record.CompletionTokens,
		record.Progress, record.Blocker, formatGoalTime(record.CreatedAt),
		formatGoalTime(record.UpdatedAt)); err != nil {
		return fmt.Errorf("save goal: %w", err)
	}
	legacyObjective := record.Objective
	if record.Status != goalstate.StatusActive {
		legacyObjective = ""
	}
	if _, err := tx.Exec(`UPDATE sessions SET goal=? WHERE id=?`, legacyObjective, sessionID); err != nil {
		return fmt.Errorf("mirror goal: %w", err)
	}
	if checkpoint {
		if _, err := tx.Exec(`INSERT INTO goal_checkpoints
			(session_id, goal_id, seq, status, rounds, usage_in, usage_cached,
			 usage_out, progress, blocker, created_at)
			SELECT ?, ?, COALESCE(MAX(seq),0)+1, ?, ?, ?, ?, ?, ?, ?, ?
			FROM goal_checkpoints WHERE session_id=? AND goal_id=?`,
			sessionID, record.ID, record.Status, record.Rounds,
			record.PromptTokens, record.CachedTokens, record.CompletionTokens,
			record.Progress, record.Blocker, formatGoalTime(record.UpdatedAt),
			sessionID, record.ID); err != nil {
			return fmt.Errorf("record goal checkpoint: %w", err)
		}
	}
	return tx.Commit()
}

// ClearGoal records an explicit user drop as paused, then clears the legacy
// active-goal mirror. Keeping the paused record preserves an auditable state
// and lets an explicit future resume restore the same goal identity.
func (s *Store) ClearGoal(sessionID string) error {
	record, ok, err := s.LoadGoal(sessionID)
	if err != nil {
		return err
	}
	if !ok {
		_, err := s.db.Exec(`UPDATE sessions SET goal='' WHERE id=?`, sessionID)
		return err
	}
	record.Status = goalstate.StatusPaused
	record.Blocker = "cleared by user"
	record.Progress = ""
	record.UpdatedAt = time.Now().UTC()
	if err := s.CheckpointGoal(sessionID, record); err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE sessions SET goal='' WHERE id=?`, sessionID)
	return err
}

// GoalCheckpoints returns the durable lifecycle history oldest first.
func (s *Store) GoalCheckpoints(sessionID, goalID string) ([]GoalCheckpoint, error) {
	rows, err := s.db.QueryContext(context.Background(), `SELECT seq, goal_id,
		status, rounds, usage_in, usage_cached, usage_out, progress, blocker,
		created_at FROM goal_checkpoints WHERE session_id=? AND goal_id=?
		ORDER BY seq`, sessionID, goalID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []GoalCheckpoint
	for rows.Next() {
		var checkpoint GoalCheckpoint
		var status, created string
		if err := rows.Scan(&checkpoint.Seq, &checkpoint.GoalID, &status,
			&checkpoint.Rounds, &checkpoint.PromptTokens, &checkpoint.CachedTokens,
			&checkpoint.CompletionTokens, &checkpoint.Progress, &checkpoint.Blocker,
			&created); err != nil {
			return nil, err
		}
		checkpoint.Status = goalstate.Status(status)
		checkpoint.CreatedAt, err = parseGoalTime(created)
		if err != nil {
			return nil, err
		}
		out = append(out, checkpoint)
	}
	return out, rows.Err()
}

func formatGoalTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseGoalTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05", value)
}
