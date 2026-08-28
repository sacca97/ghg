package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

// GoalContext and GoalUpdate keep the agent's public API independent from the
// storage package while sharing the durable lifecycle contract.
type GoalContext = goalstate.Record
type GoalUpdate = goalstate.Update

const goalUpdateToolName = "update_goal"

// goalContextBlock is injected into one provider request at a time. It is
// deliberately not appended to Agent.Messages: the persisted goal record is
// the source of truth, and a fresh request should see the latest checkpoint.
func goalContextBlock(record goalstate.Record) string {
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

func goalUpdateTool(record goalstate.Record) tools.Tool {
	return tools.Tool{
		Def: llm.NewTool(goalUpdateToolName,
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
			update := goalstate.Update{
				GoalID:   strings.TrimSpace(input.GoalID),
				Status:   goalstate.Status(strings.TrimSpace(input.Status)),
				Progress: strings.TrimSpace(input.Progress),
				Blocker:  strings.TrimSpace(input.Blocker),
			}
			if err := update.Validate(record.ID); err != nil {
				return tools.ToolResult{}, err
			}
			result := tools.TextResult(goalUpdateMessage(update), "")
			result.Source = goalUpdateToolName
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

func goalUpdateMessage(update goalstate.Update) string {
	switch update.Status {
	case goalstate.StatusComplete:
		return "Goal completion recorded after verification."
	case goalstate.StatusBlocked:
		return "Goal blocker recorded; the current run is paused until it can be resumed."
	default:
		return "Goal progress checkpoint recorded."
	}
}

func goalUpdateFromResult(result tools.ToolResult) (goalstate.Update, bool) {
	if result.Source != goalUpdateToolName || result.ExitCode != 0 || result.Metadata == nil {
		return goalstate.Update{}, false
	}
	return goalstate.Update{
		GoalID:   result.Metadata["goal_id"],
		Status:   goalstate.Status(result.Metadata["status"]),
		Progress: result.Metadata["progress"],
		Blocker:  result.Metadata["blocker"],
	}, true
}
