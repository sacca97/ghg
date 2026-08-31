package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// planCommand proposes a structured plan with the smart role. The planner
// definition supplies its read-only tool allowlist and terminal submit_plan
// call; it is never used for ordinary chat turns.
func (m *model) planCommand(text string) (tea.Model, tea.Cmd) {
	goal := strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
	if goal == "" {
		m.append(errStyle.Render("usage: /plan <goal>"))
		return m, nil
	}
	m.append(youStyle.Render("❯ ") + linkifyFilePaths("/plan "+goal, realFileExists))
	if m.busy {
		m.append(dimStyle.Render("(busy — /plan after this turn)"))
		return m, nil
	}
	if !m.requireAgent() {
		return m, nil
	}
	if m.workerClient != nil {
		m.busy = true
		m.turnStart = m.nowFn()
		m.append(dimStyle.Render("◎ planning with smart…"))
		requestID := workerRequestID("plan")
		m.cancel = func() {
			if m.workerClient != nil {
				_ = m.workerClient.Send(workerwire.CommandCancel, requestID+"-cancel", nil)
			}
		}
		if err := m.workerClient.Send(workerwire.CommandPlan, requestID, workerPlanRequest{Goal: goal}); err != nil {
			m.busy = false
			m.cancel = nil
			m.append(errStyle.Render("planning failed: " + err.Error()))
		}
		return m, m.spin.Tick
	}

	planner := m.agent
	if m.cfg != nil {
		var err error
		planner, _, _, err = buildAgentForRoleWithProfiles(m.cfg, config.RoleSmart, m.sysPrompt, m.profiles)
		if err != nil {
			m.append(errStyle.Render("planning failed: " + err.Error()))
			return m, nil
		}
	}
	if planner == nil || planner.Backend == nil {
		m.append(errStyle.Render("planning failed: no smart model is configured"))
		return m, nil
	}
	planner.Runtime = m.runtime
	planner.Role = config.RoleSmart
	if effort := m.currentEffort(); effort != "" {
		planner.Effort = effort
	}
	definition := agent.BuiltInPlannerDefinition()
	if loaded, ok := m.definitions[definition.Name]; ok {
		definition = loaded
	}

	m.busy = true
	m.turnStart = m.nowFn()
	m.append(dimStyle.Render("◎ planning with smart…"))
	p := m.prog
	usageAgent := m.agent
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	request := func() (agent.Plan, error) {
		return requestPlanWithDefinition(ctx, planner, usageAgent, goal, definition)
	}
	if p == nil {
		plan, err := request()
		return m.finishPlanProposal(planProposalMsg{plan: plan, err: err})
	}
	go func() {
		plan, err := request()
		go p.Send(planProposalMsg{plan: plan, err: err})
	}()
	return m, m.spin.Tick
}

func requestPlanWithDefinition(ctx context.Context, planner, usageAgent *agent.Agent, goal string, definition agent.Definition) (agent.Plan, error) {
	events := agent.Events{}
	if usageAgent != nil && usageAgent != planner {
		events.OnUsage = usageAgent.AddUsage
	}
	return agent.ProposePlanWithDefinition(ctx, planner, goal, definition, events)
}

func planText(p agent.Plan) string {
	var b strings.Builder
	b.WriteString("◎ plan proposed\n")
	b.WriteString("Goal: " + p.Goal + "\n\nSteps:\n")
	for i, step := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	b.WriteString("\nAcceptance checks:\n")
	for _, check := range p.AcceptanceChecks {
		b.WriteString("- " + check + "\n")
	}
	b.WriteString("\nRun /execute to start this plan with the fast model.")
	return strings.TrimRight(b.String(), "\n")
}

// switchRole replaces the live route while preserving the conversation and
// cumulative usage. Unlike /model, this is an execution detail and does not
// rewrite the user's configured default route.
func (m *model) switchRole(role string) error {
	if m.agent == nil {
		return fmt.Errorf("no agent configured")
	}
	if m.agent.Role == role {
		return nil
	}
	if m.cfg == nil {
		m.agent.Role = role
		return nil
	}
	target, err := m.cfg.ResolveRole(role)
	if err != nil {
		return err
	}
	if target.Model == "" {
		return fmt.Errorf("role %q has no configured agent", role)
	}
	return m.activateRoute(target.Model, target.Provider, role)
}

func (m *model) finishPlanProposal(msg planProposalMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	m.cancel = nil
	m.interrupt1 = false
	m.turnStart = time.Time{}
	if errors.Is(msg.err, context.Canceled) {
		m.append(dimStyle.Render("(planning interrupted)"))
		return m, nil
	}
	if msg.err != nil {
		m.append(errStyle.Render("planning failed: " + msg.err.Error()))
		return m, nil
	}
	m.proposedPlan = &msg.plan
	if m.store != nil {
		if m.sessionID == "" {
			_ = m.ensureSession()
		}
		if m.sessionID != "" {
			planJSON, _ := json.Marshal(msg.plan)
			msgSeq := 0
			if m.agent != nil {
				msgSeq = len(m.agent.MessagesSnapshot())
			}
			_ = m.store.SaveWorkflowResult(context.Background(), session.WorkflowResultRecord{
				ResultID:   fmt.Sprintf("plan-%x", time.Now().UnixNano()&0xffffffff),
				SessionID:  m.sessionID,
				Kind:       "plan",
				Version:    1,
				Payload:    string(planJSON),
				Role:       config.RoleSmart,
				MessageSeq: msgSeq,
				CreatedAt:  time.Now().UTC(),
			})
		}
	}
	m.append(dimStyle.Render(planText(msg.plan)))
	return m, nil
}

// executeCommand runs a supplied plan, or the most recent /plan proposal,
// through the fast role. Plain text is accepted as a user-authored plan;
// valid structured JSON also seeds the persistent todowrite checklist.
func (m *model) executeCommand(text string) (tea.Model, tea.Cmd) {
	if m.busy {
		m.append(dimStyle.Render("(busy — /execute after this turn)"))
		return m, nil
	}
	if !m.requireAgent() {
		return m, nil
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, "/execute"))
	var proposed *agent.Plan
	if arg == "" {
		if m.proposedPlan == nil {
			m.append(errStyle.Render("no plan to execute — use /plan <goal> or /execute <plan>"))
			return m, nil
		}
		copyPlan := clonePlan(*m.proposedPlan)
		proposed = &copyPlan
	} else if parsed, err := agent.ParsePlan(arg); err == nil {
		proposed = &parsed
	} else if strings.HasPrefix(arg, "{") || strings.HasPrefix(arg, "```") {
		m.append(errStyle.Render("invalid structured plan: " + err.Error()))
		return m, nil
	}

	if err := m.switchRole(config.RoleFast); err != nil {
		m.append(errStyle.Render("execution failed: " + err.Error()))
		return m, nil
	}
	m.mode = uiModeExecute
	prompt := arg
	if proposed != nil {
		if err := m.agent.SetTodos(proposed.Todos()); err != nil {
			m.append(errStyle.Render("execution failed: " + err.Error()))
			return m, nil
		}
		m.setGoal(proposed.Goal)
		prompt = executionPrompt(*proposed)
	}
	if strings.TrimSpace(prompt) == "" {
		m.append(errStyle.Render("usage: /execute <plan>"))
		return m, nil
	}
	m.append(dimStyle.Render("◎ executing plan with fast…"))
	return m.submit(prompt)
}

func clonePlan(p agent.Plan) agent.Plan {
	p.Steps = slices.Clone(p.Steps)
	p.AcceptanceChecks = slices.Clone(p.AcceptanceChecks)
	return p
}

func executionPrompt(p agent.Plan) string {
	var b strings.Builder
	b.WriteString("Execute this validated plan now. Use the available tools to make the changes and verify the acceptance checks; do not merely describe what should be done. Keep the todowrite checklist updated.\n\n")
	b.WriteString("Goal: " + p.Goal + "\n\nOrdered steps:\n")
	for i, step := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	b.WriteString("\nAcceptance checks:\n")
	for _, check := range p.AcceptanceChecks {
		b.WriteString("- " + check + "\n")
	}
	return b.String()
}
