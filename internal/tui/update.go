package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// scrollMouseWheel advances one terminal row per vertical wheel event. The
// viewport widget defaults to three rows, which makes the transcript feel
// jumpy in terminals that report a wheel event for each detent. Handling the
// vertical path here also works for zero-value viewports used by headless
// tests, before Bubble's lazy viewport initialization has run.
func scrollMouseWheel(vp *viewport.Model, msg tea.MouseMsg) bool {
	if vp == nil || msg.Action != tea.MouseActionPress || msg.Shift {
		return false
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		vp.ScrollUp(1)
	case tea.MouseButtonWheelDown:
		vp.ScrollDown(1)
	default:
		return false
	}
	return true
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer m.layout()

	if vp, ok := msg.(viewProbe); ok { // tests read model state race-safely
		vp.fn(m)
		return m, nil
	}
	switch msg := msg.(type) {
	case cfgSyncTick:
		return m.cfgSync()

	case cfgSyncMsg:
		m.applyCfgSync(msg)
		return m, nil

	case tea.WindowSizeMsg:
		resized := msg.Width != m.width // width change → re-wrap the whole transcript
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(msg.Width - 2)
		if resized {
			m.refreshVP() // every block re-renders at the new width (floored at minRenderWidth)
		}
		return m, nil

	case titleMsg:
		// only fill a title still at its auto placeholder (a /rename wins)
		if m.agent != nil && m.store != nil && m.sessionID != "" {
			if meta, _, err := m.store.Load(m.sessionID); err == nil {
				first := ""
				for _, msg := range m.agent.Messages {
					if msg.Role == "user" && msg.Authored {
						first = truncLine(strings.Join(strings.Fields(msg.TextContent()), " "), 64)
						break
					}
				}
				if meta.Title == first {
					_ = m.store.SetTitle(m.sessionID, msg.title)
					m.append(dimStyle.Render("◎ session titled: " + msg.title))
				}
			}
		}
		return m, nil

	case permRequest:
		m.permDialog = &permDialog{req: msg.req, reply: msg.reply}
		return m, nil

	case workerFrameMsg:
		return m.handleWorkerFrame(msg.frame)

	case workerErrorMsg:
		if msg.process != nil && msg.process == m.workerProcess {
			if m.workerClient != nil {
				_ = m.workerClient.Close()
			}
			m.workerClient = nil
			m.workerProcess = nil
			m.workerRuntime = workerwire.Runtime{}
			m.workerState = workerwire.StateInterrupted
			m.workerLiveWork = false
			m.busy = false
			m.cancel = nil
			m.interrupt1 = false
			m.turnStart = time.Time{}
			m.workerStartFailed = false
		}
		if msg.err != nil && !m.workerDetached {
			m.append(errStyle.Render("worker: " + msg.err.Error()))
		}
		return m, nil

	case workerPermissionMsg:
		m.permDialog = &permDialog{
			req:      tools.GateRequest{Tool: msg.approval.Tool, Command: msg.approval.Command, Rule: msg.approval.Rule},
			workerID: msg.approval.ID,
		}
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)

	case tea.MouseMsg:
		// shift+click/drag must pass through so the terminal's native
		// selection (copy) works while mouse capture is on — consuming the
		// event here is what breaks drag-to-copy
		if msg.Shift {
			return m, nil
		}
		// The middle row of the bottom status box owns the model, effort, and mode
		// controls, so clicks there must not fall through to transcript scrolling.
		if m.settings == nil && m.picker == nil && m.mpicker == nil && m.taskVP == nil &&
			m.height > 0 && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y == statusInfoRow(m.height) {
			if m.statusModelW > 0 && msg.X >= m.statusModelX && msg.X < m.statusModelX+m.statusModelW {
				m.cycleStatusModel()
				return m, nil
			}
			if m.statusEffortW > 0 && msg.X >= m.statusEffortX && msg.X < m.statusEffortX+m.statusEffortW {
				if m.agent != nil {
					m.setEffort(nextEffort(m.effortsFor(), m.agent.Effort))
				}
				return m, nil
			}
			if m.statusModeW > 0 && msg.X >= m.statusModeX && msg.X < m.statusModeX+m.statusModeW {
				m.cycleStatusMode()
				return m, nil
			}
		}
		if m.taskVP != nil {
			// the open task pane owns the free area: wheel scrolls it
			if scrollMouseWheel(&m.taskVP.vp, msg) {
				return m, nil
			}
			return m, nil
		}
		if m.settings != nil {
			return m.paletteMouse(msg)
		}
		if m.picker == nil && m.mpicker == nil && m.settings == nil {
			// dock rows sit just above the input box: click selects/opens,
			// wheel scrolls the selection through the strip
			if top, n := m.dockTop(), len(m.dockTasks()); n > 0 && msg.Y >= top && msg.Y < top+n {
				if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
					m.tasksFocus = true
					if msg.Button == tea.MouseButtonWheelUp {
						m.taskSel = max(m.taskSel-1, 0)
					} else {
						m.taskSel = min(m.taskSel+1, n-1)
					}
					return m, nil
				}
				if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
					sel := m.taskSel
					if m.tasksFocus {
						sel = msg.Y - top
					}
					m.tasksFocus = true
					m.taskSel = min(sel, n-1)
					// re-fetch: the list can change between the hitbox check
					// above and this open (settled tasks age out)
					if tasks := m.dockTasks(); len(tasks) > 0 {
						m.openTask(tasks[min(m.taskSel, len(tasks)-1)].ID)
					}
					return m, nil
				}
			}
			// click on a collapsed tool result expands it (and vice versa)
			if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft &&
				msg.Y > 1 && m.settings == nil {
				row := m.vp.YOffset + msg.Y - 2 // viewport starts 2 rows below the header
				if pad := m.contentPad(); row < pad {
					row = -1 // top padding: above the first block
				} else {
					row -= pad
				}
				for i := range m.blocks {
					if row >= m.blocks[i].y0 && row <= m.blocks[i].y1 && m.blocks[i].toggle() {
						m.refreshVP()
						return m, nil
					}
				}
			}
			if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y >= m.height-3 {
				m.follow = true
				m.vp.GotoBottom()
				return m, nil
			}
			if scrollMouseWheel(&m.vp, msg) {
				m.follow = m.vp.AtBottom()
				return m, nil
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
			return m, cmd
		}
		return m, nil

	case textMsg:
		m.flushThink() // reasoning always precedes the answer text
		m.current += string(msg)
		// Move complete lines into the transcript so the streaming area
		// only ever re-renders the last partial line.
		if i := strings.LastIndexByte(m.current, '\n'); i >= 0 {
			done := m.current[:i]
			m.current = m.current[i+1:]
			m.appendAssistant(done)
		}
		return m, nil

	case thinkMsg:
		if m.showThinking {
			m.flushCurrent() // thinking renders above the answer
			if m.thinkStart.IsZero() {
				m.thinkStart = m.nowFn()
			}
		}
		return m, nil

	case toolStartMsg:
		m.flushThink()
		m.flushCurrent()
		row := toolStyle.Render("⚒ "+msg.name+" ") + dimStyle.Render(msg.args)
		m.blocks = append(m.blocks, block{kind: blockToolRun, text: row, toolID: msg.id, toolRunning: true})
		m.refreshVP()
		return m, nil

	case toolEndMsg:
		for i := len(m.blocks) - 1; i >= 0; i-- {
			b := &m.blocks[i]
			if b.kind == blockToolRun && b.toolRunning && b.toolID == msg.id {
				b.toolRunning = false
				b.toolFailed = strings.HasPrefix(msg.result, "Error:")
				if b.toolFailed {
					b.text += errStyle.Render(" — failed")
				}
				b.stale = true
				break
			}
		}
		m.refreshVP()
		return m, nil

	case meEditedMsg:
		if msg.err != nil {
			m.append(errStyle.Render("/me: editor failed: " + msg.err.Error()))
		} else if n := len(config.MeInstructions()); n > 0 {
			m.append(dimStyle.Render("✓ me.md saved — standing instructions updated (" + fmt.Sprint(n) + " chars)"))
		} else {
			m.append(dimStyle.Render("me.md saved — no standing instructions set (all comments)"))
		}
		return m, nil

	case interactiveStartMsg:
		// passthrough mode: route keystrokes into the PTY. The output pane is
		// shown by View(); a fresh toolStartMsg-style banner is appended so the
		// user sees "bash (interactive)" inline with the transcript.
		m.flushThink()
		m.flushCurrent()
		m.iactive = &interactive{keys: msg.keys}
		m.append(toolStyle.Render("⚒ bash ") + dimStyle.Render("(interactive — type to respond, 15s inactivity timeout)"))
		return m, nil

	case interactiveOutMsg:
		if m.iactive == nil {
			return m, nil
		}
		m.iactive.output += msg.chunk
		// any output means the command is producing, not waiting
		m.iactive.await = false
		return m, nil

	case interactiveAwaitMsg:
		if m.iactive == nil {
			return m, nil
		}
		m.iactive.await = true
		m.iactive.awaitcd = msg.secsLeft
		return m, nil

	case interactiveDoneMsg:
		if m.iactive != nil {
			// fold the streamed output + exit into the transcript as a normal
			// tool result so the session record matches the non-interactive path
			lines := strings.Split(strings.TrimRight(msg.output, "\n"), "\n")
			// cap the persisted preview like toolEndMsg, but keep the full text
			// available to the model (it's already in the tool result string)
			preview := lines
			if len(preview) > 5 {
				preview = preview[:5]
			}
			out := dimStyle.Render("  " + strings.Join(preview, "\n  "))
			if len(lines) > 5 {
				out += dimStyle.Render(fmt.Sprintf("\n  … +%d lines", len(lines)-5))
			}
			if msg.exit != "" {
				out += "\n" + dimStyle.Render("  ("+msg.exit+")")
			}
			m.append(out)
			m.iactive = nil
		}
		return m, nil

	case steeredMsg:
		m.flushThink()
		m.flushCurrent()
		m.append(youStyle.Render("❯ ") + linkifyFilePaths(string(msg), realFileExists) + dimStyle.Render("  (steered)"))
		return m, nil

	case shellDoneMsg:
		// a `!` escape finished; its output lands behind any in-flight text
		m.flushThink()
		m.flushCurrent()
		m.applyShellDone(msg)
		return m, nil

	case goalUpdateMsg:
		m.applyGoalUpdate(msg.update)
		return m, nil

	case goalFromContextMsg:
		// the formulation call finished between turns; on success set the
		// goal and kick off the goal loop exactly like /goal <text>
		m.flushThink()
		m.flushCurrent()
		switch {
		case msg.err == context.Canceled:
			m.busy = false
			m.cancel = nil
			m.append(dimStyle.Render("(interrupted)"))
		case msg.err != nil:
			m.busy = false
			m.cancel = nil
			m.append(errStyle.Render("goal-from-context failed: " + msg.err.Error()))
		case strings.TrimSpace(msg.goal) == "":
			m.busy = false
			m.cancel = nil
			m.append(errStyle.Render("goal-from-context: model returned an empty goal"))
		default:
			goal := strings.TrimSpace(msg.goal)
			m.setGoal(goal)
			m.append(dimStyle.Render("◎ goal set: " + goal))
			return m.submit(goal)
		}
		return m, nil

	case planProposalMsg:
		return m.finishPlanProposal(msg)

	case compactMsg:
		// compaction lands between turns after its event is durable; note it
		// inline. The raw message log stays on disk — Load derives the
		// compacted view from the event, so a bad summary is inspectable and
		// retryable (/compact retry). A live turn fires two compactMsgs per
		// compaction (OnCompact's counts, then OnCompacted's summary); only the
		// one carrying the summary adds the note.
		m.flushThink()
		m.flushCurrent()
		switch {
		case msg.err != nil:
			m.append(errStyle.Render("compact failed: " + msg.err.Error()))
		case msg.summary == "":
			// counts-only path (no summary means no event was produced);
			// nothing to record
		default:
			m.append(dimStyle.Render(fmt.Sprintf("◎ compacted — summarized %d msgs, %d kept · raw history preserved", msg.took, msg.kept)))
			m.future = nil   // compaction rewrote history; stale redo entries would resurrect it
			m.msgBlock = nil // indices no longer match; rebuilt as blocks stream in
			// The compacted prompt is a derived view. Align the local save
			// cursor with that view once a session already owns the raw rows;
			// Store.Save then appends later messages to the raw SQLite tail.
			// With no session yet, leave saved at its initial value so the
			// turn-complete persist still creates and saves the conversation.
			if m.agent != nil && m.store != nil && m.sessionID != "" {
				m.saved = len(m.agent.MessagesSnapshot())
			}
			m.persist() // append the new tail; the raw pre-cutover rows are already saved
		}
		return m, nil

	case workerCompactDoneMsg:
		m.flushThink()
		m.flushCurrent()
		m.busy = false
		m.cancel = nil
		m.interrupt1 = false
		m.turnStart = time.Time{}
		m.future = nil
		m.msgBlock = nil
		if m.agent != nil {
			m.agent.SetUsage(msg.usage)
		}
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				m.append(dimStyle.Render("(compaction interrupted)"))
			} else {
				m.append(errStyle.Render("compact failed: " + msg.err.Error()))
			}
		}
		return m, nil

	case turnDoneMsg:
		m.flushThink()
		m.flushCurrent()
		m.busy = false
		m.cancel = nil
		m.interrupt1 = false
		m.turnStart = time.Time{}
		m.maybeTitle()
		// Cancellation arrives wrapped from the in-flight http request
		// ("Post ...: context canceled"), so identity comparison misses it —
		// which would strand the queue instead of draining it.
		canceled := errors.Is(msg.err, context.Canceled)
		if msg.err != nil && !canceled {
			m.append(errStyle.Render("error: " + msg.err.Error()))
		} else if canceled {
			m.append(dimStyle.Render("(interrupted — any running tool calls will be recorded as interrupted; ghg can retry them next turn)"))
		}
		continueGoal := m.goalTurnFinished(msg, canceled)
		m.persist()
		switch {
		case msg.snap != "" && msg.clean:
			dropSnapshot(msg.snap) // the turn changed no files; nothing to roll back
		case msg.snap != "":
			if m.snapshots == nil {
				m.snapshots = map[int]string{}
			}
			m.snapshots[msg.at] = msg.snap
			if m.store != nil && m.sessionID != "" {
				_ = m.store.SetSnapshot(m.sessionID, msg.at, msg.snap)
			}
		}
		// codex-style follow-up: send queued messages one turn at a time;
		// `!` shell escapes execute locally instead of starting a turn.
		// A canceled turn also drains the queue: the empty-enter steer path
		// cancels intentionally so the queued messages go out immediately.
		for len(m.queue) > 0 && (msg.err == nil || canceled) {
			next := m.queue[0]
			if strings.HasPrefix(next, "!") {
				m.queue = m.queue[1:]
				m.queueSel = -1
				m.runShellQueued(next)
				continue
			}
			return m.drainQueueHead()
		}
		// Structured update_goal controls continuation. A plain assistant
		// response never completes or pauses an active goal.
		if continueGoal {
			record, ok := m.goalRecordForSession()
			if ok && record.Status == goalstate.StatusActive {
				return m.submitGoal(goalContinuePrompt(record.Objective))
			}
		}
		return m, nil

	case catalogsMsg:
		m.updateCatalogs(msg)
		return m, nil

	case authResultMsg:
		m.applyAuthResult(msg)
		return m, nil

	case noticeMsg:
		m.append(dimStyle.Render(string(msg)))
		return m, nil

	case usageMsg:
		// Turn already folds usage into the agent's session totals (header
		// reads those); this message just forces a redraw mid-stream.
		return m, nil

	case quitArmMsg:
		m.quit1 = false // the arm window closed; next ctrl+c starts fresh
		return m, nil

	case escArmMsg:
		m.esc1 = false   // the double-esc rewind window closed
		m.escClr = false // the double-esc draft-clear window closed
		return m, nil

	case taskUpdateMsg:
		// a background subagent started or settled; the dock shows it. An
		// open view of a settled task reloads from the stored report.
		if m.agent != nil && m.taskVP != nil {
			if t, ok := m.agent.Tasks().Get(m.taskVP.id); ok && t.Status != agent.TaskRunning && m.taskVP.live {
				m.openTask(m.taskVP.id) // reseed with the final report
			} else {
				m.refreshTaskVP()
			}
		}
		return m, nil

	case mcpStatusMsg:
		// An MCP server changed state. Announce each server's FIRST settle in
		// the transcript (one line, once per session per server) so arrivals
		// and failures are visible without typing /mcp — later transitions
		// (auto-reconnect, toggles) stay quiet to avoid flapping noise.
		if m.mcpMgr != nil {
			if m.mcpSeen == nil {
				m.mcpSeen = map[string]bool{}
			}
			for _, srv := range m.mcpMgr.Statuses() {
				if m.mcpSeen[srv.Name] || srv.Status == mcp.StatusConnecting {
					continue
				}
				m.mcpSeen[srv.Name] = true
				switch srv.Status {
				case mcp.StatusReady:
					m.append(dimStyle.Render(fmt.Sprintf("⚡ mcp: %s ready (%d tools)", srv.Name, srv.Tools)))
				case mcp.StatusFailed:
					line := fmt.Sprintf("✗ mcp: %s failed: %s", srv.Name, srv.Err)
					if srv.Source != "" {
						line += " (" + srv.Source + ")"
					}
					m.append(errStyle.Render(line + fmt.Sprintf(" (/mcp %s reconnect)", srv.Name)))
				case mcp.StatusDisabled:
					m.append(dimStyle.Render(fmt.Sprintf("○ mcp: %s disabled", srv.Name)))
				}
			}
		}
		return m, nil

	case taskEventMsg:
		// one live event from the open task's subagent stream; append it to
		// the pane's transcript (deltas coalesce into lines before append)
		tv := m.taskVP
		if tv == nil || msg.id != tv.id {
			return m, nil
		}
		switch msg.kind {
		case 0: // text delta
			tv.buf.WriteString(msg.s)
		case 1: // tool start
			fmt.Fprintf(&tv.buf, "\n%s %s %s\n", toolStyle.Render("⚒"), msg.s, dimStyle.Render(msg.s2))
		case 2: // tool end
			preview := strings.Split(strings.TrimRight(msg.s2, "\n"), "\n")
			if len(preview) > 4 {
				preview = append(preview[:4], fmt.Sprintf("… +%d lines", len(msg.s2)-4))
			}
			fmt.Fprintf(&tv.buf, "%s\n", dimStyle.Render("  "+strings.Join(preview, "\n  ")))
		}
		m.refreshTaskVP()
		return m, nil

	case imageMsg:
		switch {
		case msg.err != nil:
			m.append(errStyle.Render("image paste failed: " + msg.err.Error()))
		case msg.path == "":
			m.append(dimStyle.Render("(no image on clipboard)"))
		default:
			m.input.InsertString("@" + msg.path + " ")
			m.refreshMenu()
		}
		return m, nil

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case scheduleTickMsg:
		if m.agent == nil {
			return m, scheduleTick()
		}
		return m, tea.Batch(scheduleTick(), m.fireDueSchedules())
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}
