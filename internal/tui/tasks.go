// tasks.go: the persistent background-subagent area and the per-task detail
// view.
//
// The dock is a strip rendered above the input box (below the queue) whenever
// background tasks exist — running or recently settled — so the user always
// knows how many subagents are in flight without running /tasks. ctrl+t
// focuses it; ↑/↓ (or the mouse wheel over the strip) moves the selection,
// enter opens the selected task's detail view, and esc backs out: detail →
// dock → main thread. The detail view is a scrollback pane filled from the
// task's live event stream (registry.Subscribe) while it runs, and from the
// stored Report once it settles.
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

// taskEventMsg is one live event from an opened background task (OnText /
// OnToolStart / OnToolEnd fire from the subagent's worker goroutine; prog.Send
// funnels them onto the UI thread like every other stream).
type taskEventMsg struct {
	id   string
	kind int // 0 = text, 1 = tool start, 2 = tool end
	s    string
	s2   string // tool args (start) or result (end)
}

// sendTaskMsg hands a task event to the UI without ever blocking the subagent
// worker goroutine: prog.Send parks while the UI event queue is backed up, so
// the send is detached into its own goroutine. Program.Send is safe for
// concurrent use (it just selects on the program's msg channel), and if the
// program exits first, bubbletea unblocks every pending Send — no leak. The
// pane resyncs from the task's Report on the next paint, so a reordered or
// lost interim frame is cosmetic; the worker must never stall on the UI.
func sendTaskMsg(p *tea.Program, msg taskEventMsg) {
	sendProg(p, msg)
}

// taskView is the open per-task pane: the live transcript of one background
// subagent (or its stored report once settled).
type taskView struct {
	id   string
	vp   viewport.Model
	buf  strings.Builder // full transcript text; vp shows a window into it
	live bool            // subscribed to the task's event stream
}

// tasksDockHeight is the maximum number of screen rows the dock strip
// occupies (hint row + task rows); the strip scrolls if there are more tasks.
const tasksDockHeight = 6

// dockSettledGrace is how long a settled task stays in the dock after
// finishing — long enough to notice the ✓ and open the report, then the
// strip cleans itself. Restored tasks (--resume history) never show: their
// subagents died with the previous process. /tasks lists everything.
const dockSettledGrace = time.Minute

// dockTasks returns the dock's tasks — running ones plus those settled
// within dockSettledGrace, never restored ones — newest first. Bare test
// models have no agent; the dock is simply empty.
func (m *model) dockTasks() []agent.BackgroundTask {
	if m.workerOnly {
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
		return out
	}
	if m.agent == nil {
		return nil
	}
	var out []agent.BackgroundTask
	for _, t := range m.agent.Tasks().List() {
		if t.Restored {
			continue // resume history belongs in /tasks, not the dock
		}
		// running always shows; zero EndedAt (never settled) sorts with them
		if t.Status == agent.TaskRunning || time.Since(t.EndedAt) < dockSettledGrace {
			out = append(out, t)
		}
	}
	slices.Reverse(out)
	return out
}

// clampTaskSel keeps the dock selection inside the current task list.
func (m *model) clampTaskSel(n int) {
	v := max(len(m.dockTasks()), n)
	if m.taskSel >= v {
		m.taskSel = max(v-1, 0)
	}
}

// tasksDock renders the persistent strip: one row per task with a live
// status icon, plus a hint row when the dock is focused.
func (m *model) tasksDock() string {
	tasks := m.dockTasks()
	if len(tasks) == 0 {
		return ""
	}
	m.clampTaskSel(len(tasks))

	rows := make([]string, 0, len(tasks)+2)
	if m.tasksFocus {
		rows = append(rows, dimStyle.Render(" ⚙ subagents — ↑/↓ select · enter open · x cancel · esc back"))
	}

	budget := tasksDockHeight - len(rows)
	if len(tasks) > budget { // reserve a row for the "+N more" counter
		budget--
	}
	lo := 0
	if m.tasksFocus && m.taskSel >= budget {
		lo = m.taskSel - budget + 1 // keep the selection visible
	}
	hi := min(lo+budget, len(tasks))

	for i := lo; i < hi; i++ {
		t := tasks[i]
		icon := toolStyle.Render("⏳")
		switch t.Status {
		case agent.TaskDone:
			icon = "✓"
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
		case m.tasksFocus && i == m.taskSel:
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
// the prompt, then the live event stream while the task runs, or the stored
// report once it has settled.
func (m *model) openTask(id string) {
	if m.workerOnly {
		t, ok := m.workerTasks[id]
		if !ok {
			return
		}
		tv := &taskView{id: id, live: t.Status == string(agent.TaskRunning)}
		fmt.Fprintf(&tv.buf, "%s %s  %s\n\n%s %s\n", toolStyle.Render("⚙"), t.ID, t.Description, youStyle.Render("prompt:"), t.Prompt)
		if t.Status == string(agent.TaskRunning) {
			fmt.Fprintf(&tv.buf, "\n%s\n", dimStyle.Render("  running…"))
		} else {
			fmt.Fprintf(&tv.buf, "\n%s %s\n", toolStyle.Render(t.Status+":"), t.Report)
		}
		m.taskVP = tv
		m.refreshTaskVP()
		return
	}
	if m.agent == nil {
		return
	}
	t, ok := m.agent.Tasks().Get(id)
	if !ok {
		return
	}
	tv := &taskView{id: id, live: t.Status == agent.TaskRunning}
	fmt.Fprintf(&tv.buf, "%s %s  %s\n\n%s %s\n",
		toolStyle.Render("⚙"), t.ID, t.Description,
		youStyle.Render("prompt:"), t.Prompt)
	if t.Status == agent.TaskRunning {
		fmt.Fprintf(&tv.buf, "\n%s\n", dimStyle.Render("  running…"))
		p := m.prog
		m.agent.Tasks().Subscribe(id, agent.Events{
			OnText: func(s string) {
				sendTaskMsg(p, taskEventMsg{id: id, kind: 0, s: s})
			},
			OnToolStart: func(_, n, a string) {
				sendTaskMsg(p, taskEventMsg{id: id, kind: 1, s: n, s2: a})
			},
			OnToolEnd: func(_, n, r string) {
				sendTaskMsg(p, taskEventMsg{id: id, kind: 2, s: n, s2: r})
			},
		})
	} else {
		fmt.Fprintf(&tv.buf, "\n%s %s\n", toolStyle.Render(string(t.Status)+":"), t.Report)
	}
	m.taskVP = tv
	m.refreshTaskVP()
}

// refreshTaskVP resizes the open task pane to the free screen area and
// reloads its content, following the tail while the task streams.
func (m *model) refreshTaskVP() {
	tv := m.taskVP
	if tv == nil {
		return
	}
	// 2 rows of chrome: the header and the footer hint
	tv.vp.Width, tv.vp.Height = m.width, max(m.height-2, 1)
	atBottom := tv.vp.AtBottom()
	tv.vp.SetContent(tv.buf.String())
	if atBottom {
		tv.vp.GotoBottom()
	}
}

// taskViewKey handles input while a task detail view is open: scroll keys go
// to the pane, x cancels a running task, esc backs out to the main thread.
func (m *model) taskViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	tv := m.taskVP
	if tv == nil || (m.agent == nil && !m.workerOnly) {
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
			if m.workerOnly {
				m.append(dimStyle.Render("task cancellation is not available from the worker view"))
			} else {
				m.agent.Tasks().Cancel(tv.id)
			}
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
	if (m.agent == nil && !m.workerOnly) || m.taskVP == nil {
		return dimStyle.Render("no provider configured — run /auth first")
	}
	status := "running"
	description := ""
	var restored bool
	if m.workerOnly {
		t, ok := m.workerTasks[m.taskVP.id]
		if ok {
			status, description, restored = t.Status, t.Description, t.Restored
		}
	} else {
		t, ok := m.agent.Tasks().Get(m.taskVP.id)
		if ok {
			status, description, restored = string(t.Status), t.Description, t.Restored
		}
	}
	if restored {
		status += ", restored"
	}
	head := toolStyle.Render(fmt.Sprintf(" ⚙ %s — %s", m.taskVP.id, truncLine(description, max(m.width-30, 8)))) +
		dimStyle.Render("  ("+status+")")
	foot := dimStyle.Render(" esc back · PgUp/PgDn scroll · x cancel")
	return head + "\n" + sanitizeView(m.taskVP.vp.View()) + "\n" + foot
}
