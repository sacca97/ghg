package goal

import (
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
)

// ApplyUpdate validates and applies one model-authored goal checkpoint.
func ApplyUpdate(record *agent.GoalRecord, update agent.GoalUpdate) (bool, error) {
	if record == nil {
		return false, fmt.Errorf("goal record is nil")
	}
	if err := update.Validate(record.ID); err != nil {
		return false, err
	}
	if record.Status == update.Status && record.Progress == update.Progress && record.Blocker == update.Blocker {
		return true, nil
	}
	record.Status = update.Status
	record.Progress = TruncateNote(update.Progress)
	record.Blocker = TruncateNote(update.Blocker)
	return true, nil
}

// FinishResult contains the state transition after one goal turn.
type FinishResult struct {
	Record     agent.GoalRecord
	Continue   bool
	Checkpoint bool
}

// FinishTurn accounts for a completed goal turn and selects its next state.
func FinishTurn(record agent.GoalRecord, usage models.Usage, turnErr error, canceled bool, maxRounds int, now time.Time) FinishResult {
	if now.IsZero() {
		now = time.Now()
	}
	if record.Status != agent.GoalStatusActive {
		if turnErr == nil && (record.Status == agent.GoalStatusBlocked || record.Status == agent.GoalStatusComplete) {
			AddUsage(&record, usage)
			record.Rounds++
			record.UpdatedAt = now.UTC()
			return FinishResult{Record: record, Checkpoint: true}
		}
		return FinishResult{Record: record}
	}

	AddUsage(&record, usage)
	record.Rounds++
	record.UpdatedAt = now.UTC()
	if turnErr != nil {
		record.Status = StatusForError(turnErr)
		if canceled {
			record.Status = agent.GoalStatusPaused
			record.Blocker = "interrupted by user"
		} else {
			record.Blocker = Blocker(turnErr)
		}
		return FinishResult{Record: record, Checkpoint: true}
	}
	if record.Status == agent.GoalStatusBlocked || record.Status == agent.GoalStatusComplete {
		return FinishResult{Record: record, Checkpoint: true}
	}
	if maxRounds > 0 && record.Rounds >= maxRounds {
		record.Status = agent.GoalStatusBudgetLimited
		record.Blocker = fmt.Sprintf("goal round circuit breaker reached (%d rounds)", record.Rounds)
		return FinishResult{Record: record, Checkpoint: true}
	}
	return FinishResult{Record: record, Continue: true}
}

func ContinuePrompt(objective string) string {
	return fmt.Sprintf(`[goal continuation] Continue working on the active objective: %s

Inspect and verify the remaining work. Use the request-scoped goal context as the source of truth. Call update_goal with status active and a concise progress note when you have made meaningful progress; call it with status blocked only for a genuine blocker; call it with status complete only after verification. A prose claim alone never completes the goal.`, objective)
}

func AddUsage(record *agent.GoalRecord, usage models.Usage) {
	record.PromptTokens += usage.PromptTokens
	record.CompletionTokens += usage.CompletionTokens
	record.CachedTokens += usage.Cached()
}

func Blocker(err error) string {
	if err == nil {
		return ""
	}
	return TruncateNote(err.Error())
}

func TruncateNote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= agent.MaxNoteBytes {
		return value
	}
	return value[:agent.MaxNoteBytes]
}

func StatusForError(err error) agent.GoalStatus {
	if err == nil {
		return agent.GoalStatusActive
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"rate limit", "quota", "usage limit", "credit", "billing", "too many requests", "daily limit"} {
		if strings.Contains(message, marker) {
			return agent.GoalStatusUsageLimited
		}
	}
	return agent.GoalStatusPaused
}
