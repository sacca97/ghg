package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func sendProg(p *tea.Program, msg tea.Msg) {
	if p == nil {
		return
	}
	go p.Send(msg)
}

// scrollMouseWheel advances one terminal row per vertical wheel event. The
// viewport widget defaults to three rows, which makes the transcript feel
// jumpy in terminals that report a wheel event for each detent. Handling the
// vertical path here also works for zero-value viewports used by headless
// tests, before Bubble's lazy viewport initialization has run.
func scrollMouseWheel(vp *viewport.Model, msg tea.MouseMsg) {
	if vp == nil || msg.Action != tea.MouseActionPress || msg.Shift {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		vp.ScrollUp(1)
	case tea.MouseButtonWheelDown:
		vp.ScrollDown(1)
	default:
		return
	}
}

type wheelState struct {
	last         time.Time
	dir          int
	velocity     float64
	fraction     float64
	pending      int
	framePending bool
}

type wheelFrameMsg struct{}

const wheelIdle = 120 * time.Millisecond

// scrollTranscriptWheel keeps isolated detents precise and coalesces rapid
// detents into one small frame. This avoids a repaint for every event without
// introducing momentum or a continuously running animation.
func (m *model) scrollTranscriptWheel(msg tea.MouseMsg) tea.Cmd {
	if msg.Action != tea.MouseActionPress || msg.Shift {
		return nil
	}
	dir := 0
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		dir = -1
	case tea.MouseButtonWheelDown:
		dir = 1
	default:
		return nil
	}
	now := m.nowFn()
	elapsed := now.Sub(m.wheel.last)
	if m.wheel.last.IsZero() || elapsed < 0 || elapsed > wheelIdle || dir != m.wheel.dir {
		m.wheel = wheelState{last: now, dir: dir, velocity: 1}
		m.applyTranscriptWheel(dir)
		return nil
	}
	m.wheel.last = now
	m.wheel.velocity = min(m.wheel.velocity+0.75, 6.0)
	m.wheel.fraction += m.wheel.velocity
	move := int(m.wheel.fraction)
	m.wheel.fraction -= float64(move)
	if move > 0 {
		m.wheel.pending += dir * move
	}
	if m.wheel.pending == 0 || m.wheel.framePending {
		return nil
	}
	m.wheel.framePending = true
	return tea.Tick(16*time.Millisecond, func(time.Time) tea.Msg { return wheelFrameMsg{} })
}

func (m *model) applyTranscriptWheel(delta int) {
	if delta < 0 {
		m.vp.ScrollUp(-delta)
	} else {
		m.vp.ScrollDown(delta)
	}
	m.follow = m.vp.AtBottom()
}

func (m *model) applyWheelFrame() {
	delta := m.wheel.pending
	m.wheel.pending = 0
	m.wheel.framePending = false
	if delta != 0 {
		m.applyTranscriptWheel(delta)
	}
}

func (m *model) flushStreaming() {
	m.flushThink()
	m.flushCurrent()
}

func (m *model) finishTurnState() {
	m.busy = false
	m.cancel = nil
	m.interrupt1 = false
	m.turnStart = time.Time{}
	if m.thinkStart.IsZero() {
		m.thinkEffort = ""
	}
	m.reviewProgress = nil
	m.reviewScopeShown = false
	m.reviewLastExtensionTo = 0
	m.reviewFinalEvidenceShown = false
	m.reviewClosedShown = false
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// The middle row of the bottom status box owns the model, effort, and mode
	// controls, so clicks there must not fall through to transcript scrolling.
	if m.settings == nil && m.picker == nil && m.taskVP == nil && m.permDialog == nil && m.questionDialog == nil && m.rew == nil &&
		m.height > 0 && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y == statusInfoRow(m.height) {
		if m.statusModelW > 0 && msg.X >= m.statusModelX && msg.X < m.statusModelX+m.statusModelW {
			m.cycleStatusModel()
			return m, nil
		}
		if m.statusEffortW > 0 && msg.X >= m.statusEffortX && msg.X < m.statusEffortX+m.statusEffortW {
			m.setEffort(nextEffort(m.effortsFor(), m.currentEffort()))
			return m, nil
		}
		if m.statusModeW > 0 && msg.X >= m.statusModeX && msg.X < m.statusModeX+m.statusModeW {
			m.cycleStatusMode()
			return m, nil
		}
	}
	if m.taskVP != nil {
		// the open task pane owns the free area: wheel scrolls it
		scrollMouseWheel(&m.taskVP.vp, msg)
		return m, nil
	}
	if m.settings != nil {
		return m.paletteMouse(msg)
	}
	if m.picker == nil {
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
				sel := msg.Y - top
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
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y >= m.height-3 {
			m.follow = true
			m.vp.GotoBottom()
			return m, nil
		}
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			return m, m.scrollTranscriptWheel(msg)
		}
		if handled, cmd := m.handleTranscriptMouse(msg); handled {
			return m, cmd
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd
	}
	return m, nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Spinner ticks only change the busy indicator. Avoid relaying them through
	// layout, which otherwise recomputes the whole TUI on every animation frame.
	if tick, ok := msg.(spinner.TickMsg); ok {
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(tick)
		// Task durations share the spinner cadence, but this fast path skips
		// layout; let View rebuild the cached dock for the next frame.
		m.dockView = ""
		m.frameViewsValid = false
		return m, cmd
	}
	if _, ok := msg.(cursor.BlinkMsg); ok {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	m.dockTasksValid = false
	if m.settings != nil {
		m.settings.rootRowsValid = false
	}
	defer m.layout()

	switch msg := msg.(type) {
	case selectionCopyMsg:
		if msg.err != nil {
			m.append(errStyle.Render("copy failed: " + msg.err.Error()))
		} else {
			m.selection = nil
		}
		return m, nil

	case selectionEdgeMsg:
		return m, m.selectionEdgeTick()

	case wheelFrameMsg:
		m.applyWheelFrame()
		return m, nil

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(msg.Width - 2)
		return m, nil

	case recentSessionsMsg:
		return m, m.applyRecentSessions(msg)

	case sessionPreviewMsg:
		m.applySessionPreview(msg)
		return m, nil

	case resumeLoadedMsg:
		if msg.err != nil {
			m.append(errStyle.Render(msg.err.Error()))
			return m, nil
		}
		if err := m.applyResumeData(msg.data); err != nil {
			m.append(errStyle.Render(err.Error()))
			return m, nil
		}
		if m.forkNotice != "" {
			m.append(dimStyle.Render(m.forkNotice))
			m.forkNotice = ""
		}
		return m, m.startWorkerCmd()

	case workerStartedMsg:
		action := m.pendingWorkerAction
		m.pendingWorkerAction = nil
		m.workerStarting = false
		if msg.sessionID != "" && msg.sessionID != m.sessionID {
			if msg.client != nil {
				_ = msg.client.Close()
			}
			if msg.process != nil {
				_ = msg.process.Stop()
			}
			return m, nil
		}
		if msg.err != nil {
			m.workerStartFailed = true
			m.workerStartError = msg.err.Error()
			config.LogEvent("worker.start", msg.err.Error())
			return m, nil
		}
		m.workerStartFailed = false
		m.workerStartError = ""
		m.workerProcess = msg.process
		generation := m.attachWorkerClient(msg.client, msg.runtime)
		if msg.process != nil {
			m.monitorWorker(msg.process, msg.runtime, generation)
		}
		if action != nil {
			return m, action()
		}
		return m, nil

	case workerFrameMsg:
		if msg.generation != 0 && msg.generation != m.workerGeneration {
			return m, nil
		}
		if msg.client != nil && msg.client != m.workerClient {
			return m, nil
		}
		if msg.frame.SessionID != "" && m.sessionID != "" && msg.frame.SessionID != m.sessionID {
			return m, nil
		}
		return m.handleWorkerFrame(msg.frame)

	case workerErrorMsg:
		if msg.generation != 0 && msg.generation != m.workerGeneration {
			return m, nil
		}
		if msg.client != nil && msg.client != m.workerClient {
			// Ignore errors from stale, replaced clients.
			return m, nil
		}
		wasDetached := m.workerDetached
		isCurrentClient := msg.client != nil && msg.client == m.workerClient
		isCurrentProc := msg.process != nil && msg.process == m.workerProcess
		if isCurrentClient || isCurrentProc {
			m.clearPendingRewind()
			m.workerGeneration++
			if m.workerClient != nil {
				_ = m.workerClient.Close()
			}
			m.workerClient = nil
			if isCurrentProc {
				m.workerProcess = nil
				m.workerRuntime = workerwire.Runtime{}
				m.workerStartFailed = false
			}
			m.workerState = workerwire.StateInterrupted
			m.workerLiveWork = false
			m.workerDetached = false
			m.questionDialog = nil
			m.finishTurnState()
			m.flushThink()
			m.thinkStart = time.Time{}
		}
		if msg.err != nil && !wasDetached && !errors.Is(msg.err, context.Canceled) {
			m.append(errStyle.Render("worker: " + msg.err.Error()))
		}
		return m, nil

	case workerPermissionMsg:
		m.permDialog = &permDialog{
			req:      tools.GateRequest{Tool: msg.approval.Tool, Command: msg.approval.Command, Rule: msg.approval.Rule},
			workerID: msg.approval.ID,
		}
		return m, nil

	case workerQuestionMsg:
		if m.questionDialog == nil {
			m.questionDialog = &questionDialog{request: msg.request}
		}
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

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

	case planDeltaMsg:
		m.flushStreaming()
		m.planCurrent += string(msg)
		if i := strings.LastIndexByte(m.planCurrent, '\n'); i >= 0 {
			done := m.planCurrent[:i]
			m.planCurrent = m.planCurrent[i+1:]
			m.appendPlanBlock(done)
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
		m.flushStreaming()
		if msg.name == "request_review_extension" {
			if markdown := reviewExtensionMarkdown(msg.args); markdown != "" {
				m.appendAssistantBlock(markdown)
				m.inMsg = false
				return m, nil
			}
		}
		row := toolStyle.Render("⚒ "+msg.name+" ") + dimStyle.Render(toolCallSummary(msg.name, msg.args))
		m.blocks = append(m.blocks, block{kind: blockToolRun, text: row, toolID: msg.id, toolRunning: true})
		m.transcriptDirty = true
		return m, nil

	case toolEndMsg:
		matched := false
		for i := len(m.blocks) - 1; i >= 0; i-- {
			b := &m.blocks[i]
			if b.kind == blockToolRun && b.toolRunning && b.toolID == msg.id {
				matched = true
				b.toolRunning = false
				b.toolFailed = strings.HasPrefix(msg.result, "Error:")
				if b.toolFailed {
					errMsg := strings.TrimSpace(strings.TrimPrefix(msg.result, "Error:"))
					firstLine := strings.SplitN(errMsg, "\n", 2)[0]
					showBashReason := msg.name == "bash" &&
						(strings.HasPrefix(firstLine, "unknown tool") || strings.HasPrefix(firstLine, "tool unavailable"))
					if (msg.name != "bash" || showBashReason) && firstLine != "" && !strings.HasPrefix(firstLine, "exit status") {
						b.text += errStyle.Render(" — failed: " + firstLine)
					} else {
						b.text += errStyle.Render(" — failed")
					}
				}
				b.stale = true
				m.transcriptDirty = true
				break
			}
		}
		if !matched {
			m.closeAttachedTool()
		}
		return m, nil

	case meEditedMsg:
		if msg.err != nil {
			m.append(errStyle.Render("/me: editor failed: " + msg.err.Error()))
		} else if n := len(config.UserInstructions()); n > 0 {
			m.append(dimStyle.Render("✓ " + filepath.Base(msg.path) + " saved — standing instructions updated (" + fmt.Sprint(n) + " chars)"))
		} else {
			m.append(dimStyle.Render(filepath.Base(msg.path) + " saved — no standing instructions set (all comments)"))
		}
		return m, nil

	case steeredMsg:
		m.flushStreaming()
		m.append(youStyle.Render("❯ ") + linkifyFilePaths(string(msg), realFileExists) + dimStyle.Render("  (steered)"))
		return m, nil

	case shellDoneMsg:
		// a `!` escape finished; its output lands behind any in-flight text
		m.flushStreaming()
		m.applyShellDone(msg)
		return m, nil

	case goalUpdateMsg:
		m.applyGoalUpdate(msg.update)
		return m, nil

	case goalUpdateRecordMsg:
		m.applyGoalRecord(msg.record)
		return m, nil

	case goalFromContextMsg:
		// the formulation call finished between turns; on success set the
		// goal and kick off the goal loop exactly like /goal <text>
		m.flushStreaming()
		switch {
		case msg.interrupted || errors.Is(msg.err, context.Canceled):
			m.finishTurnState()
			// Intentional cancellation is an internal operation boundary, not a
			// transcript error.
		case msg.err != nil:
			m.finishTurnState()
			m.append(errStyle.Render("goal-from-context failed: " + msg.err.Error()))
		case strings.TrimSpace(msg.goal) == "":
			m.finishTurnState()
			m.append(errStyle.Render("goal-from-context: model returned an empty goal"))
		default:
			goal := strings.TrimSpace(msg.goal)
			if msg.record != nil {
				m.applyGoalRecord(*msg.record)
			} else {
				m.setGoal(goal)
			}
			m.append(dimStyle.Render("◎ goal set: " + goal))
			return m.submitGoal(goal)
		}
		return m, nil

	case workerCompactDoneMsg:
		m.flushStreaming()
		m.finishTurnState()
		m.future = nil
		m.usage = msg.usage
		m.rebuildTranscript()
		if msg.contextTokens > 0 {
			m.workerContextTokens = msg.contextTokens
		}
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				// Intentional cancellation is silent.
			} else {
				m.append(errStyle.Render("compact failed: " + msg.err.Error()))
			}
		}
		return m, nil

	case turnDoneMsg:
		return m.handleTurnDone(msg)

	case catalogsMsg:
		m.updateCatalogs(msg)
		return m, nil

	case authResultMsg:
		m.applyAuthResult(msg)
		return m, nil

	case authOAuthWaitingMsg:
		m.append(dimStyle.Render("open the following authorization URL in your browser:\n\n  " + msg.url + "\n\nbrowser login in progress…"))
		return m, nil

	case authOAuthCodeRequestMsg:
		m.openNamePrompt(msg.label, "", func(value string) {
			select {
			case msg.reply <- value:
			default:
			}
		})
		m.namePrompt.onCancel = func() {
			select {
			case msg.cancel <- struct{}{}:
			default:
			}
		}
		return m, nil

	case authOAuthResultMsg:
		m.applyOAuthResult(msg)
		return m, nil

	case noticeMsg:
		m.flushStreaming()
		m.append(dimStyle.Render(string(msg)))
		return m, nil

	case reviewProgressMsg:
		m.reviewProgressHistory = append(m.reviewProgressHistory, msg.progress)
		if msg.progress.Reason == "resumed" {
			m.reviewing = true
		}
		if !m.reviewing {
			return m, nil
		}
		m.flushStreaming()
		progress := msg.progress
		m.reviewProgress = &progress
		if progress.Phase == "inventory" && progress.Inventory != nil && !m.reviewScopeShown {
			m.reviewScopeShown = true
			m.append(dimStyle.Render(renderReviewScope(progress)))
		}
		if progress.Phase == "assessment" && progress.Inventory != nil && len(progress.Inventory.Scope) > 0 {
			m.append(dimStyle.Render("◎ review scope resolved\n  scope: " + strings.Join(progress.Inventory.Scope, ", ")))
		}
		if progress.Phase == "extension" && progress.ToAllocation > progress.FromAllocation && progress.ToAllocation != m.reviewLastExtensionTo {
			m.reviewLastExtensionTo = progress.ToAllocation
			m.append(dimStyle.Render(renderReviewExtension(progress)))
		}
		if progress.Phase == "final_evidence" && !m.reviewFinalEvidenceShown {
			m.reviewFinalEvidenceShown = true
			m.append(dimStyle.Render("◎ review · final evidence batch"))
		}
		if progress.Phase == "exploration_closed" && !m.reviewClosedShown {
			m.reviewClosedShown = true
			m.append(dimStyle.Render("◎ review · exploration closed · synthesizing"))
		}
		return m, nil

	case quitArmMsg:
		m.quit1 = false // the arm window closed; next ctrl+c starts fresh
		return m, nil

	case escArmMsg:
		m.esc1 = false   // the double-esc rewind window closed
		m.escClr = false // the double-esc draft-clear window closed
		return m, nil

	case taskUpdateMsg:
		return m, nil

	case mcpStatusMsg:
		if len(msg.statuses) > 0 {
			m.append(renderWorkerMCPStatuses(msg.statuses))
		}
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

	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) handleTurnDone(msg turnDoneMsg) (tea.Model, tea.Cmd) {
	m.closeAttachedTool()
	m.flushStreaming()
	m.finishTurnState()
	// Cancellation arrives wrapped from the in-flight http request
	// ("Post ...: context canceled"), so identity comparison misses it —
	// which would strand the queue instead of draining it.
	canceled := msg.interrupted || errors.Is(msg.err, context.Canceled)
	if msg.err != nil && !canceled {
		m.append(errStyle.Render("error: " + msg.err.Error()))
	}
	continueGoal := msg.goalContinue && msg.err == nil && !canceled
	if msg.goal != nil {
		m.applyGoalRecord(*msg.goal)
	}
	if m.reviewing {
		m.reviewing = false
		if msg.err == nil && !canceled && msg.final != "" {
			if msg.reviewMarkdown != "" {
				m.appendAssistant(msg.reviewMarkdown)
			} else {
				m.appendAssistant(msg.final)
			}
		}
	} else if m.uiMode() == uiModePlan && msg.final != "" {
		if msg.plan != "" {
			m.proposedPlanMD = msg.plan
		}
	}
	// codex-style follow-up: send queued messages one turn at a time;
	// `!` shell escapes go to the worker instead of starting a turn.
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
		if ok && record.Status == agent.GoalStatusActive {
			return m.submitGoal(agent.ContinuePrompt(record.Objective))
		}
	}
	return m, nil
}

func (m *model) closeAttachedTool() {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		b := &m.blocks[i]
		if b.kind == blockToolRun && b.toolRunning && b.toolID == "attach-active" {
			b.toolRunning = false
			b.stale = true
			m.transcriptDirty = true
			return
		}
	}
}
