package agent

import "testing"

func TestParsePlanAndSeedTodos(t *testing.T) {
	p, err := ParsePlan(`{"goal":"ship it","steps":["write code","run tests"],"acceptance_checks":["tests pass"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "ship it" || len(p.Steps) != 2 || len(p.AcceptanceChecks) != 1 {
		t.Fatalf("parsed plan: %+v", p)
	}
	todos := p.Todos()
	if len(todos) != 2 || todos[0].Status != "in_progress" || todos[1].Status != "pending" {
		t.Fatalf("seeded todos: %+v", todos)
	}
	a := &Agent{}
	if err := a.SetTodos(todos); err != nil {
		t.Fatal(err)
	}
	if got := a.TodosJSON(); got == "" {
		t.Fatal("validated plan should be serializable")
	}
}

func TestParsePlanRejectsIncompleteOutput(t *testing.T) {
	for _, response := range []string{
		"not json",
		`{"goal":"ship it","steps":[],"acceptance_checks":["tests pass"]}`,
		`{"goal":"ship it","steps":["write code"],"acceptance_checks":[]}`,
	} {
		if _, err := ParsePlan(response); err == nil {
			t.Fatalf("expected invalid plan for %q", response)
		}
	}
}
