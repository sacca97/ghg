package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// planCommand switches to Plan mode and submits the goal as an ordinary turn.
func (m *model) planCommand(text string) (tea.Model, tea.Cmd) {
	goal := strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
	if goal == "" {
		if err := m.setMode(uiModePlan); err != nil {
			m.append(errStyle.Render("plan mode failed: " + err.Error()))
			return m, nil
		}
		m.append(dimStyle.Render("switched to plan mode (read-only exploration)"))
		return m, nil
	}
	if err := m.setMode(uiModePlan); err != nil {
		m.append(errStyle.Render("plan mode failed: " + err.Error()))
		return m, nil
	}
	return m.submitTurn(goal, true)
}

// executeCommand runs a supplied plan, or the most recent /plan proposal,
// through the fast role in Execute mode.
func (m *model) executeCommand(text string) (tea.Model, tea.Cmd) {
	if m.busy {
		m.append(dimStyle.Render("(busy — /execute after this turn)"))
		return m, nil
	}
	if !m.requireAgent() {
		return m, nil
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, "/execute"))
	var planMD string
	if arg == "" {
		if m.proposedPlanMD == "" {
			m.append(errStyle.Render("no plan to execute — use /plan <goal> or /execute <plan>"))
			return m, nil
		}
		planMD = m.proposedPlanMD
	} else {
		planMD = arg
	}

	if err := m.setMode(uiModeExecute); err != nil {
		m.append(errStyle.Render("execute mode failed: " + err.Error()))
		return m, nil
	}

	prompt := fmt.Sprintf("Execute the following approved plan. Create and maintain a todowrite\nchecklist while implementing it.\n\n%s", planMD)
	return m.submitTurn(prompt, true)
}
