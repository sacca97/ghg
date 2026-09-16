package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

const nextReasoningEffortToolName = "set_next_reasoning_effort"

func (a *Agent) reasoningSelectorTool() (tools.Tool, bool) {
	if a == nil || a.reasoningSelectorDisabled || (a.DynamicReasoning != nil && !*a.DynamicReasoning) {
		return tools.Tool{}, false
	}
	efforts := advertisedReasoningEfforts(a.ReasoningEfforts)
	if len(efforts) < 2 {
		return tools.Tool{}, false
	}
	enum, _ := json.Marshal(efforts)
	schema := fmt.Sprintf(`{"type":"object","properties":{"effort":{"type":"string","enum":%s}},"required":["effort"],"additionalProperties":false}`, enum)
	return tools.Tool{
		Def: models.NewTool(nextReasoningEffortToolName,
			fmt.Sprintf("Per-call reasoning selection is enabled. The configured effort is %q. Choose one advertised provider effort for the next foreground model call, or omit this tool to keep the configured effort.", a.Effort), schema),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			var request struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(args, &request); err != nil {
				return "", fmt.Errorf("invalid reasoning effort selection: %w", err)
			}
			if _, ok := canonicalReasoningEffort(efforts, request.Effort); !ok {
				return "", fmt.Errorf("unsupported reasoning effort %q", request.Effort)
			}
			return "Reasoning effort selected for the next foreground call.", nil
		},
	}, true
}

func advertisedReasoningEfforts(values []string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		seen := false
		for _, existing := range out {
			if strings.EqualFold(existing, value) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, value)
		}
	}
	return out
}

func canonicalReasoningEffort(values []string, selected string) (string, bool) {
	selected = strings.TrimSpace(selected)
	for _, value := range advertisedReasoningEfforts(values) {
		if strings.EqualFold(value, selected) {
			return value, true
		}
	}
	return "", false
}

func (a *Agent) selectedReasoningEffort(calls []models.ToolCall, results []tools.ToolResult) (string, string, error) {
	var selected string
	var requested string
	count := 0
	for i, call := range calls {
		if call.Function.Name != nextReasoningEffortToolName {
			continue
		}
		count++
		if count > 1 {
			return selected, requested, errors.New("set_next_reasoning_effort may be called only once per model response")
		}
		if i >= len(results) {
			return selected, requested, nil
		}
		if _, failed := toolResultError(results[i]); failed {
			return "", requested, nil
		}
		var request struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &request); err != nil {
			return selected, requested, nil
		}
		requested = request.Effort
		selected, _ = canonicalReasoningEffort(a.ReasoningEfforts, request.Effort)
	}
	return selected, requested, nil
}

func withoutReviewExtension(ts []tools.Tool) []tools.Tool {
	out := make([]tools.Tool, 0, len(ts))
	for _, tool := range ts {
		if tool.Def.Function.Name != "request_review_extension" {
			out = append(out, tool)
		}
	}
	return out
}
