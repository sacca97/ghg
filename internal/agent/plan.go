package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Plan is the structured result produced by the planning workflow. Steps are
// deliberately plain text: todowrite owns execution state, while acceptance
// checks remain part of the acting prompt.
type Plan struct {
	Goal             string   `json:"goal"`
	Steps            []string `json:"steps"`
	AcceptanceChecks []string `json:"acceptance_checks"`
}

// Validate checks the limits shared with the conversation's todowrite plan.
func (p Plan) Validate() error {
	if strings.TrimSpace(p.Goal) == "" {
		return fmt.Errorf("plan has no goal")
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}
	if len(p.Steps) > maxTodos {
		return fmt.Errorf("plan has more than %d steps", maxTodos)
	}
	for i, step := range p.Steps {
		if step = strings.TrimSpace(step); step == "" {
			return fmt.Errorf("plan step %d is empty", i+1)
		} else if len(step) > maxTodoContent {
			return fmt.Errorf("plan step %d exceeds %d characters", i+1, maxTodoContent)
		}
	}
	if len(p.AcceptanceChecks) == 0 {
		return fmt.Errorf("plan has no acceptance checks")
	}
	for i, check := range p.AcceptanceChecks {
		if check = strings.TrimSpace(check); check == "" {
			return fmt.Errorf("acceptance check %d is empty", i+1)
		} else if len(check) > maxTodoContent {
			return fmt.Errorf("acceptance check %d exceeds %d characters", i+1, maxTodoContent)
		}
	}
	return nil
}

// ParsePlan accepts the JSON object returned by the planner, including a
// fenced JSON block or a response with a small amount of surrounding prose.
// The planner caller still treats a validation error as a failed proposal.
func ParsePlan(response string) (Plan, error) {
	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "```") {
		if nl := strings.IndexByte(response, '\n'); nl >= 0 {
			response = strings.TrimSpace(response[nl+1:])
		}
		response = strings.TrimSpace(strings.TrimSuffix(response, "```"))
	}
	start := strings.IndexByte(response, '{')
	end := strings.LastIndexByte(response, '}')
	if start < 0 || end < start {
		return Plan{}, fmt.Errorf("planner returned non-JSON output")
	}
	response = response[start : end+1]

	var wire struct {
		Goal             string   `json:"goal"`
		Steps            []string `json:"steps"`
		AcceptanceChecks []string `json:"acceptance_checks"`
		Checks           []string `json:"checks"`
		Plan             *struct {
			Goal             string   `json:"goal"`
			Steps            []string `json:"steps"`
			AcceptanceChecks []string `json:"acceptance_checks"`
			Checks           []string `json:"checks"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(response), &wire); err != nil {
		return Plan{}, fmt.Errorf("planner returned invalid JSON: %w", err)
	}
	p := Plan{Goal: wire.Goal, Steps: wire.Steps, AcceptanceChecks: wire.AcceptanceChecks}
	if len(p.AcceptanceChecks) == 0 {
		p.AcceptanceChecks = wire.Checks
	}
	if wire.Plan != nil {
		p = Plan{Goal: wire.Plan.Goal, Steps: wire.Plan.Steps, AcceptanceChecks: wire.Plan.AcceptanceChecks}
		if len(p.AcceptanceChecks) == 0 {
			p.AcceptanceChecks = wire.Plan.Checks
		}
	}
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	p.Goal = strings.TrimSpace(p.Goal)
	for i := range p.Steps {
		p.Steps[i] = strings.TrimSpace(p.Steps[i])
	}
	for i := range p.AcceptanceChecks {
		p.AcceptanceChecks[i] = strings.TrimSpace(p.AcceptanceChecks[i])
	}
	return p, nil
}

// Todos converts a validated plan into the existing execution checklist.
func (p Plan) Todos() []Todo {
	items := make([]Todo, len(p.Steps))
	for i, step := range p.Steps {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		items[i] = Todo{ID: fmt.Sprintf("t%d", i+1), Content: step, Status: status}
	}
	return items
}

// SetTodos validates and replaces the current conversation plan. It is the
// public boundary used by the TUI when a planner proposal becomes executable.
func (a *Agent) SetTodos(items []Todo) error {
	_, err := a.setTodos(append([]Todo(nil), items...))
	return err
}
