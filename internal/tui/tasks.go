// Package tui tasks manages background subagent dock and detail views.
package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
)

// taskView renders the scrollback transcript for one background task.
type taskView struct {
	id   string
	vp   viewport.Model
	text string // full transcript text; vp shows a window into it
}

// tasksDockHeight bounds the maximum row height for the dock strip.
const tasksDockHeight = 6

// dockSettledGrace specifies duration finished tasks remain in the dock before removal.
const dockSettledGrace = time.Minute

// dockTasks returns active and recently settled tasks.
func (m *model) dockTasks() []agent.BackgroundTask {
	if m.dockTasksValid {
		return m.dockTasksCache
	}
	var out []agent.BackgroundTask
	for _, task := range m.workerTasks {
		if task.Restored {
			continue
		}
		t := agent.BackgroundTask{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: agent.TaskStatus(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt}
		if t.Status == agent.TaskRunning || time.Since(t.EndedAt) < dockSettledGrace {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b agent.BackgroundTask) int {
		if n := b.StartedAt.Compare(a.StartedAt); n != 0 {
			return n
		}
		return strings.Compare(b.ID, a.ID)
	})
	m.dockTasksCache = out
	m.dockTasksValid = true
	return out
}

// clampTaskSel keeps the dock selection inside the current task list.
func (m *model) clampTaskSel() {
	n := len(m.dockTasks())
	if n == 0 {
		m.taskSel = 0
		return
	}
	m.taskSel = min(max(m.taskSel, 0), n-1)
}

// tasksDock renders the persistent strip: one row per task with a live
// status icon, plus a hint row when the dock is focused.
func (m *model) tasksDock() string {
	tasks := m.dockTasks()
	if len(tasks) == 0 {
		return ""
	}
	sel := min(max(m.taskSel, 0), len(tasks)-1)

	rows := make([]string, 0, len(tasks)+2)
	if m.tasksFocus {
		rows = append(rows, dimStyle.Render(" ⚙ subagents — ↑/↓ select · enter open · esc back"))
	}

	budget := tasksDockHeight - len(rows)
	if len(tasks) > budget { // reserve a row for the "+N more" counter
		budget--
	}
	lo := 0
	if m.tasksFocus && sel >= budget {
		lo = sel - budget + 1 // keep the selection visible
	}
	hi := min(lo+budget, len(tasks))

	for i := lo; i < hi; i++ {
		t := tasks[i]
		icon := toolStyle.Render("⏳")
		switch t.Status {
		case agent.TaskDone:
			icon = toolStyle.Render("✓")
		case agent.TaskError, agent.TaskCancelled:
			icon = errStyle.Render("✗")
		}
		line := fmt.Sprintf("%s %s  %s", icon, t.ID, truncLine(t.Description, max(m.width-24, 8)))
		var meta string
		if t.Status == agent.TaskRunning {
			meta = fmt.Sprintf("  %ds", int(time.Since(t.StartedAt).Seconds()))
		} else {
			meta = "  " + string(t.Status)
		}
		switch {
		case m.tasksFocus && i == sel:
			line = botStyle.Render(" → "+line) + toolStyle.Render(meta)
		case t.Status == agent.TaskRunning:
			line = "   " + toolStyle.Render(line) + dimStyle.Render(meta)
		default:
			line = "   " + line + dimStyle.Render(meta)
		}
		rows = append(rows, line)
	}
	if more := len(tasks) - hi; more > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("   … +%d more (ctrl+t to browse)", more)))
	}
	return strings.Join(rows, "\n")
}

// openTask opens the detail view for one task: a scrollback pane seeded with
// the prompt, or the stored report once it has settled.
func (m *model) openTask(id string) {
	t, ok := m.workerTasks[id]
	if !ok {
		return
	}
	tv := &taskView{id: id}
	tv.text = fmt.Sprintf("%s %s  %s\n\n%s %s\n", toolStyle.Render("⚙"), t.ID, t.Description, youStyle.Render("prompt:"), t.Prompt)
	if t.Status == string(agent.TaskRunning) {
		tv.text += fmt.Sprintf("\n%s\n", dimStyle.Render("  running…"))
	} else {
		tv.text += fmt.Sprintf("\n%s %s\n", toolStyle.Render(t.Status+":"), t.Report)
	}
	m.taskVP = tv
	m.refreshTaskVP()
}

// refreshTaskVP resizes the open task pane to the free screen area and reloads
// its stored content.
func (m *model) refreshTaskVP() {
	tv := m.taskVP
	if tv == nil {
		return
	}
	// 2 rows of chrome: the header and the footer hint
	tv.vp.Width, tv.vp.Height = m.width, max(m.height-2, 1)
	atBottom := tv.vp.AtBottom()
	tv.vp.SetContent(tv.text)
	if atBottom {
		tv.vp.GotoBottom()
	}
}

// taskViewKey handles input while a task detail view is open: scroll keys go
// to the pane, esc backs out to the main thread.
func (m *model) taskViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	tv := m.taskVP
	if tv == nil {
		m.taskVP = nil
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.taskVP = nil
		m.tasksFocus = true // land on the dock so ↑/↓ keep working; esc again unfocuses
		return m, nil
	case tea.KeyCtrlT:
		m.taskVP = nil
		m.tasksFocus = true
		return m, nil
	case tea.KeyRunes:
		if string(msg.Runes) == "x" {
			m.append(dimStyle.Render("task cancellation is not available from the worker view"))
			return m, nil
		}
	}
	var cmd tea.Cmd
	tv.vp, cmd = tv.vp.Update(msg)
	return m, cmd
}

// taskViewView renders the open task pane full-screen, between the header
// row and a footer hint (View's layout mirrors View's structure).
func (m *model) taskViewView() string {
	if m.taskVP == nil {
		return dimStyle.Render("no provider configured — run /auth first")
	}
	status := "running"
	description := ""
	var restored bool
	if t, ok := m.workerTasks[m.taskVP.id]; ok {
		status, description, restored = t.Status, t.Description, t.Restored
	}
	if restored {
		status += ", restored"
	}
	head := toolStyle.Render(fmt.Sprintf(" ⚙ %s — %s", m.taskVP.id, truncLine(description, max(m.width-30, 8)))) +
		dimStyle.Render("  ("+status+")")
	foot := dimStyle.Render(" esc back · PgUp/PgDn scroll")
	return head + "\n" + sanitizeView(m.taskVP.vp.View()) + "\n" + foot
}
