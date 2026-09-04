package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

// GoalStatus is the durable lifecycle state of a goal.
type GoalStatus = session.GoalStatus

const (
	GoalStatusActive        = session.GoalStatusActive
	GoalStatusPaused        = session.GoalStatusPaused
	GoalStatusBlocked       = session.GoalStatusBlocked
	GoalStatusUsageLimited  = session.GoalStatusUsageLimited
	GoalStatusBudgetLimited = session.GoalStatusBudgetLimited
	GoalStatusComplete      = session.GoalStatusComplete
)

const (
	// MaxObjectiveBytes and MaxNoteBytes bound user/model-authored data that is
	// persisted in the goal ledger and injected into later requests.
	MaxObjectiveBytes = session.MaxObjectiveBytes
	MaxNoteBytes      = session.MaxNoteBytes
)

// GoalRecord is one goal's current durable state. Usage fields count requests made
// while this goal was active; session-wide usage remains owned by agent.Agent.
type GoalRecord = session.GoalRecord

func NewGoal(objective string) GoalRecord { return session.NewGoal(objective) }

func NewGoalID() string { return session.NewGoalID() }

func ValidGoalStatus(status GoalStatus) bool { return session.ValidGoalStatus(status) }

// GoalUpdate is the structured result of the model-facing update_goal tool.
// Active is a progress checkpoint; complete and blocked are terminal for the
// current goal run. Paused and limit states are controlled by the host.
type GoalUpdate struct {
	GoalID   string
	Status   GoalStatus
	Progress string
	Blocker  string
}

// Validate checks a model-authored state change. The model may checkpoint
// progress, declare a genuine blocker, or declare completion. Host-controlled
// pause and limit states cannot be forged through the tool.
func (u GoalUpdate) Validate(currentID string) error {
	if strings.TrimSpace(u.GoalID) != "" && strings.TrimSpace(u.GoalID) != currentID {
		return fmt.Errorf("goal id %q does not match the active goal", u.GoalID)
	}
	if len(u.Progress) > MaxNoteBytes || len(u.Blocker) > MaxNoteBytes {
		return fmt.Errorf("goal checkpoint exceeds %d bytes", MaxNoteBytes)
	}
	switch u.Status {
	case GoalStatusActive:
		if strings.TrimSpace(u.Blocker) != "" {
			return errors.New("an active goal cannot have a blocker; use status blocked")
		}
		if strings.TrimSpace(u.Progress) == "" {
			return errors.New("active goal updates require a progress note")
		}
	case GoalStatusBlocked:
		if strings.TrimSpace(u.Blocker) == "" {
			return errors.New("blocked goal updates require a blocker")
		}
	case GoalStatusComplete:
		if strings.TrimSpace(u.Progress) == "" {
			return errors.New("completed goal updates require a verification note")
		}
	default:
		return fmt.Errorf("model may set only active, blocked, or complete (got %q)", u.Status)
	}
	return nil
}

// ApplyUpdate validates and applies one model-authored goal checkpoint, and
// reports whether the record changed. Progress and blocker notes are truncated
// to MaxNoteBytes so model-authored data cannot bloat the goal ledger.
func ApplyUpdate(record *GoalRecord, update GoalUpdate) (bool, error) {
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
	record.Progress = truncateNote(update.Progress)
	record.Blocker = truncateNote(update.Blocker)
	return true, nil
}

func truncateNote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= MaxNoteBytes {
		return value
	}
	return value[:utf8Prefix(value, MaxNoteBytes)]
}

const GoalToolName = "update_goal"

// GoalContextBlock renders the active goal for one model request.
func GoalContextBlock(record GoalRecord) string {
	var b strings.Builder
	b.WriteString("Active goal context (request-scoped; do not treat this block as conversation history):\n")
	fmt.Fprintf(&b, "Goal ID: %s\n", record.ID)
	fmt.Fprintf(&b, "Objective: %s\n", record.Objective)
	fmt.Fprintf(&b, "Status: %s\n", record.Status)
	fmt.Fprintf(&b, "Completed goal turns: %d\n", record.Rounds)
	if strings.TrimSpace(record.Progress) == "" {
		b.WriteString("Latest progress: none recorded\n")
	} else {
		fmt.Fprintf(&b, "Latest progress: %s\n", record.Progress)
	}
	if strings.TrimSpace(record.Blocker) != "" {
		fmt.Fprintf(&b, "Current blocker: %s\n", record.Blocker)
	}
	b.WriteString("Work on this objective using the available tools. Verify changes before claiming completion. Use update_goal with a concise progress checkpoint, a genuine blocker, or a verification note when the objective is complete. Do not claim completion in prose alone.")
	return b.String()
}

// GoalTool returns the model-facing goal checkpoint tool for record.
func GoalTool(record GoalRecord) tools.Tool {
	return tools.Tool{
		Def: models.NewTool(GoalToolName,
			"Checkpoint the active goal, report a genuine blocker, or mark the verified objective complete. Use status active for progress, blocked when work cannot continue, and complete only after verification.",
			`{"type":"object","properties":{"goal_id":{"type":"string","description":"The active goal ID from the goal context"},"status":{"type":"string","enum":["active","blocked","complete"]},"progress":{"type":"string","description":"What was completed or verified in this checkpoint"},"blocker":{"type":"string","description":"The concrete blocker and what is needed to proceed"}},"required":["status"]}`),
		RunResult: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var input struct {
				GoalID   string `json:"goal_id"`
				Status   string `json:"status"`
				Progress string `json:"progress"`
				Blocker  string `json:"blocker"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return tools.ToolResult{}, fmt.Errorf("update_goal arguments: %w", err)
			}
			update := GoalUpdate{
				GoalID:   strings.TrimSpace(input.GoalID),
				Status:   GoalStatus(strings.TrimSpace(input.Status)),
				Progress: strings.TrimSpace(input.Progress),
				Blocker:  strings.TrimSpace(input.Blocker),
			}
			if err := update.Validate(record.ID); err != nil {
				return tools.ToolResult{}, err
			}
			result := tools.TextResult(goalUpdateMessage(update), "")
			result.Source = GoalToolName
			result.Metadata = map[string]string{
				"goal_id":  record.ID,
				"status":   string(update.Status),
				"progress": update.Progress,
				"blocker":  update.Blocker,
			}
			return result, nil
		},
	}
}

func goalUpdateMessage(update GoalUpdate) string {
	switch update.Status {
	case GoalStatusComplete:
		return "Goal completion recorded after verification."
	case GoalStatusBlocked:
		return "Goal blocker recorded; the current run is paused until it can be resumed."
	default:
		return "Goal progress checkpoint recorded."
	}
}

// GoalUpdateFromResult extracts a validated goal update from a tool result.
func GoalUpdateFromResult(result tools.ToolResult) (GoalUpdate, bool) {
	if result.Source != GoalToolName || result.ExitCode != 0 || result.Metadata == nil {
		return GoalUpdate{}, false
	}
	return GoalUpdate{
		GoalID:   result.Metadata["goal_id"],
		Status:   GoalStatus(result.Metadata["status"]),
		Progress: result.Metadata["progress"],
		Blocker:  result.Metadata["blocker"],
	}, true
}

const GoalFromContextDefaultWindow = 8

// GoalFromContextMessages returns the conversation tail used to formulate a goal.
func GoalFromContextMessages(msgs []models.Message, n int) ([]models.Message, error) {
	if n <= 0 {
		n = GoalFromContextDefaultWindow
	}
	if len(msgs) == 0 {
		return nil, errors.New("not enough context to formulate a goal — chat a bit first")
	}
	conv := msgs[1:]
	if len(conv) < 2 {
		return nil, errors.New("not enough context to formulate a goal — chat a bit first")
	}
	if n > len(conv) {
		n = len(conv)
	}
	return conv[len(conv)-n:], nil
}

// BuildGoalFromContextPrompt asks the model to distill a concrete goal.
func BuildGoalFromContextPrompt(tail []models.Message) string {
	var b strings.Builder
	b.WriteString("Distill the end of this conversation into a detailed goal the assistant should keep working on until it is verifiably done.\n\n")
	b.WriteString("Reply with ONLY the goal: a first line stating the concrete outcome, then a short bullet list of the specific, checkable completion criteria ")
	b.WriteString("(files to change, commands that must pass, behavior to confirm). Include the key constraints, decisions, and identifiers (file paths, function names, ")
	b.WriteString("error messages) from the conversation so the goal stands alone. No preamble, no quotes, no explanation.\n\n---\n\n")
	WriteTranscript(&b, tail)
	b.WriteString("\n---\n\nWrite the goal now.")
	return b.String()
}

// ContinuePrompt re-opens the active goal for another turn.
func ContinuePrompt(objective string) string {
	return fmt.Sprintf(`[goal continuation] Continue working on the active objective: %s

Inspect and verify the remaining work. Use the request-scoped goal context as the source of truth. Call update_goal with status active and a concise progress note when you have made meaningful progress; call it with status blocked only for a genuine blocker; call it with status complete only after verification. A prose claim alone never completes the goal.`, objective)
}

// WriteTranscript renders bounded message data for a model-authored summary.
func WriteTranscript(b *strings.Builder, msgs []models.Message) {
	for _, m := range msgs {
		switch m.Role {
		case "user":
			fmt.Fprintf(b, "user: %s\n", truncateField(m.TextContent(), 2000))
		case "assistant":
			if c := strings.TrimSpace(m.TextContent()); c != "" {
				fmt.Fprintf(b, "assistant: %s\n", truncateField(c, 2000))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(b, "assistant called %s(%s) id=%s", tc.Function.Name, truncateField(tc.Function.Arguments, 500), tc.ID)
				if tc.DurationMs > 0 || tc.ExitCode != 0 {
					fmt.Fprintf(b, " [duration_ms=%d exit_code=%d]", tc.DurationMs, tc.ExitCode)
				}
				b.WriteByte('\n')
			}
		case "tool":
			source := m.Source
			if source == "" {
				source = m.Name
			}
			fmt.Fprintf(b, "tool result source=%s exit_code=%d: %s", source, m.ExitCode, truncateField(m.Content, 500))
			if m.Output != nil {
				ref := m.Output
				fmt.Fprintf(b, " [output id=%s hash=%s original_bytes=%d stored_bytes=%d complete=%t]", ref.ID, ref.Hash, ref.OriginalBytes, ref.StoredBytes, ref.Complete)
			}
			b.WriteByte('\n')
		}
	}
}

func truncateField(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > n {
		const suffix = "…"
		if n <= len(suffix) {
			return s[:utf8Prefix(s, n)]
		}
		return s[:utf8Prefix(s, n-len(suffix))] + suffix
	}
	return s
}
