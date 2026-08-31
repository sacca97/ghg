package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Plan is the structured result produced by the planning workflow. Steps are
// deliberately plain text: todowrite owns execution state, while acceptance
// checks remain part of the acting prompt.
// Plan is the structured result produced by the planning workflow. Steps are
// deliberately plain text: todowrite owns execution state, while acceptance
// checks remain part of the acting prompt.
type Plan struct {
	Goal             string   `json:"goal"`
	Assumptions      []string `json:"assumptions,omitempty"`
	Steps            []string `json:"steps"`
	AcceptanceChecks []string `json:"acceptance_checks"`
	Risks            []string `json:"risks,omitempty"`
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
	for i, a := range p.Assumptions {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("assumption %d is empty", i+1)
		}
	}
	for i, r := range p.Risks {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("risk %d is empty", i+1)
		}
	}
	return nil
}

// ParsePlan accepts the JSON object returned by the planner.
func ParsePlan(response string) (Plan, error) {
	response = strings.TrimSpace(response)
	var p Plan
	if err := json.Unmarshal([]byte(response), &p); err != nil {
		return Plan{}, fmt.Errorf("planner returned invalid JSON: %w", err)
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
	for i := range p.Assumptions {
		p.Assumptions[i] = strings.TrimSpace(p.Assumptions[i])
	}
	for i := range p.Risks {
		p.Risks[i] = strings.TrimSpace(p.Risks[i])
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
