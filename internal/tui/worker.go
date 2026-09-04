package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

const (
	workerSessionEnv  = "GHG_WORKER_SESSION"
	workerBaseEnv     = "GHG_WORKER_BASE"
	workerCWDEnv      = "GHG_WORKER_CWD"
	workerModelEnv    = "GHG_WORKER_MODEL"
	workerProviderEnv = "GHG_WORKER_PROVIDER"
	workerRoleEnv     = "GHG_WORKER_ROLE"
	workerEffortEnv   = "GHG_WORKER_EFFORT"
	workerModeEnv     = "GHG_WORKER_MODE"
	workerCautiousEnv = "GHG_WORKER_CAUTIOUS"
	workerSandboxEnv  = "GHG_WORKER_SANDBOX"
	workerNetworkEnv  = "GHG_WORKER_NETWORK"
	workerApprovalEnv = "GHG_WORKER_APPROVAL"
)

type workerEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type workerStateEvent struct {
	State    workerwire.State `json:"state"`
	Detached bool             `json:"detached"`
	Mode     string           `json:"mode"`
}

type workerToolStartEvent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type workerToolEndEvent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
}

func decodeEvent[T any](data json.RawMessage) (T, bool) {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return value, false
	}
	return value, true
}

func workerError(message string) error {
	if message == "" {
		return nil
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "context canceled") || strings.Contains(lower, "interrupted") {
		return context.Canceled
	}
	return errors.New(message)
}

func workerStateBusy(state workerwire.State) bool {
	return state == workerwire.StateRunning || state == workerwire.StateWaitingApproval
}

func (m *model) attachWorkerClient(client *workerwire.Client, runtimeFile workerwire.Runtime) uint64 {
	m.workerRuntime = runtimeFile
	m.workerClient = client
	m.workerGeneration++
	generation := m.workerGeneration
	m.pumpWorker(client, generation)
	return generation
}

func (m *model) startWorkerProcess(cautious bool) error {
	if m.store == nil {
		return errors.New("worker requires a session store")
	}
	if m.modelName == "" || m.provName == "" {
		return errors.New(m.degradedProviderNote())
	}
	if m.sessionID == "" {
		m.sessionID = session.NewSessionID()
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	runtimeFile, err := workerwire.NewRuntime(dir, m.sessionID)
	if err != nil {
		return err
	}
	// A detached worker already owns this session (resume after /detach, or
	// --resume while one still runs). Attach to it instead of launching a
	// competitor: the launch would fail its lifetime lock, the monitor would
	// see the failed process and close the valid client, and the live worker
	// would read that as an unacknowledged disconnect and cancel its work.
	if runtimeFile.Live() {
		client, cerr := m.connectWorker(runtimeFile)
		if cerr != nil {
			return cerr
		}
		m.attachWorkerClient(client, runtimeFile)
		return nil
	}
	if err := runtimeFile.WritePrompt(m.sysPrompt); err != nil {
		return err
	}
	workerEnv := map[string]string{
		"GHG_INTERNAL_WORKER": "1",
		workerSessionEnv:      m.sessionID,
		workerBaseEnv:         dir,
		// Captured at launch, which now happens lazily on the first
		// worker-backed turn — after any /cd the user made.
		workerCWDEnv:      mustWorkingDirectory(),
		workerModelEnv:    m.modelName,
		workerProviderEnv: m.provName,
		workerRoleEnv:     m.currentRole(),
		workerEffortEnv:   m.currentEffort(),
		workerModeEnv:     m.uiMode(),
		workerCautiousEnv: strconv.FormatBool(cautious),
	}
	if m.cfg != nil && m.cfg.Execution != nil {
		workerEnv[workerSandboxEnv] = m.cfg.Execution.Sandbox
		workerEnv[workerNetworkEnv] = m.cfg.Execution.Network
		workerEnv[workerApprovalEnv] = m.cfg.Execution.Approval
	}
	proc, err := workerwire.Launch(context.Background(), os.Args[0], workerEnv)
	if err != nil {
		_ = runtimeFile.RemovePrompt()
		return err
	}
	client, err := m.connectWorker(runtimeFile)
	if err != nil {
		_ = proc.Stop()
		waitProcess(proc, time.Second)
		_ = runtimeFile.RemovePrompt()
		return err
	}
	m.workerProcess = proc
	generation := m.attachWorkerClient(client, runtimeFile)
	m.monitorWorker(proc, runtimeFile, generation)
	return nil
}

// connectWorker dials the session socket until the worker serves it (bounded
// by 5s — enough for a fresh process to reach Serve, short enough that a dead
// endpoint fails fast).
func (m *model) connectWorker(runtimeFile workerwire.Runtime) (*workerwire.Client, error) {
	var client *workerwire.Client
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client, err = workerwire.Dial(context.Background(), runtimeFile)
		if err == nil {
			return client, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

func (m *model) attachWorkerProcess(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("worker session id is empty")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	runtimeFile, err := workerwire.NewRuntime(dir, sessionID)
	if err != nil {
		return err
	}
	client, err := m.connectWorker(runtimeFile)
	if err != nil {
		return fmt.Errorf("attach worker: %w", err)
	}
	m.attachWorkerClient(client, runtimeFile)
	return nil
}

func (m *model) monitorWorker(proc *workerwire.Process, runtimeFile workerwire.Runtime, generation uint64) {
	p := m.prog
	go func() {
		err := proc.Wait()
		if state, stateErr := runtimeFile.ReadState(); stateErr == nil {
			switch state.State {
			case workerwire.StateRunning, workerwire.StateWaitingApproval, workerwire.StateStopping:
				_ = runtimeFile.WriteState(workerwire.StateRecord{
					SessionID: runtimeFile.SessionID, State: workerwire.StateInterrupted,
					Role: state.Role, PID: proc.PID(), Detail: "worker exited before clean shutdown",
				})
			}
		}
		if p != nil && err != nil {
			p.Send(workerErrorMsg{err: fmt.Errorf("worker exited: %w", err), process: proc, generation: generation})
		}
	}()
}

func (m *model) ensureWorker() bool {
	if m.workerClient != nil || m.workerStartFailed || m.prog == nil || m.store == nil {
		return m.workerClient != nil
	}
	if err := m.startWorkerProcess(m.cautious); err != nil {
		m.workerStartFailed = true
		m.workerStartError = err.Error()
		config.LogEvent("worker.start", err.Error())
		return false
	}
	m.workerStartError = ""
	return true
}

func (m *model) beginWorkerTransition() error {
	if m.busy || m.workerLiveWork {
		return errors.New("cannot change session while worker work is running")
	}
	m.stopWorker()
	m.workerStartFailed = false
	m.workerStartError = ""
	return nil
}

func (m *model) syncWorkerConfiguration(updateEffort bool) {
	if m.workerClient == nil {
		return
	}
	if err := m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("configure"), workerwire.ConfigureRequest{
		Model: m.modelName, Provider: m.provName, Role: m.currentRole(),
		Effort: m.currentEffort(), UpdateEffort: updateEffort,
		Mode: m.uiMode(),
	}); err != nil {
		m.append(errStyle.Render("worker configuration failed: " + err.Error()))
	}
}

func mustWorkingDirectory() string {
	wd, _ := os.Getwd()
	return wd
}

func (m *model) pumpWorker(client *workerwire.Client, generation uint64) {
	p := m.prog
	go func() {
		frames, errs := client.Frames(), client.Errors()
		for frames != nil || errs != nil {
			select {
			case frame, ok := <-frames:
				if !ok {
					frames = nil
					continue
				}
				if p != nil {
					p.Send(workerFrameMsg{frame: frame, client: client, generation: generation})
				}
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if err != nil && p != nil {
					p.Send(workerErrorMsg{err: err, client: client, generation: generation})
				}
			}
		}
		if p != nil {
			p.Send(workerErrorMsg{err: errors.New("worker connection closed"), client: client, generation: generation})
		}
	}()
}

func (m *model) stopWorker() {
	client, proc := m.workerClient, m.workerProcess
	detached := m.workerDetached
	m.workerGeneration++
	m.workerClient, m.workerProcess = nil, nil
	m.workerRuntime = workerwire.Runtime{}
	m.workerDetached = false
	if client == nil && proc == nil {
		return
	}
	if client != nil {
		if !detached {
			_ = client.Send(workerwire.CommandStop, workerRequestID("stop"), nil)
		}
		_ = client.Close()
	}
	if proc != nil && !detached {
		if !waitProcess(proc, 2*time.Second) {
			_ = proc.Stop()
			waitProcess(proc, time.Second)
		}
	}
}

func waitProcess(proc *workerwire.Process, timeout time.Duration) bool {
	if proc == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func workerRequestID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func (m *model) submitWorkerTurn(text string, authored bool, prepared string, parts []models.ContentPart, at int, snap string, goalCtx *agent.GoalRecord, ask bool) (tea.Model, tea.Cmd) {
	if m.workerClient == nil {
		return m, nil
	}
	message := models.Message{Role: "user", Content: prepared, Parts: parts, Authored: authored}
	if authored {
		now := m.nowFn()
		message.SentAt = &now
	}
	m.messages = append(m.messages, message)
	requestID := workerRequestID("turn")
	systemPrompt := m.sysPrompt
	m.cancel = func() {
		if m.workerClient != nil {
			_ = m.workerClient.Send(workerwire.CommandCancel, requestID+"-cancel", nil)
		}
	}
	if err := m.workerClient.Send(workerwire.CommandInput, requestID, workerwire.Input{
		Input: prepared, Authored: authored, Parts: parts, Goal: goalCtx,
		SystemPrompt: systemPrompt, At: at, Snap: snap,
		PlanMode:   !ask && !m.reviewing && m.uiMode() == uiModePlan,
		ReviewMode: !ask && m.reviewing,
		AskMode:    ask,
	}); err != nil {
		m.cancel = nil
		m.busy = false
		m.append(errStyle.Render("worker turn failed: " + err.Error()))
		return m, nil
	}
	m.append(youStyle.Render("❯ ") + linkifyFilePaths(text, realFileExists))
	if authored {
		for len(m.msgBlock) <= at {
			m.msgBlock = append(m.msgBlock, -1)
		}
		m.msgBlock[at] = len(m.blocks) - 1
	}
	return m, m.spin.Tick
}

func (m *model) handleWorkerFrame(frame workerwire.Frame) (tea.Model, tea.Cmd) {
	switch frame.Type {
	case workerwire.TypeSnapshot:
		var envelope workerwire.SnapshotEnvelope
		if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
			return m, nil
		}
		var snapshot workerwire.Snapshot
		if err := json.Unmarshal(envelope.State, &snapshot); err != nil {
			return m, nil
		}
		m.applyWorkerSnapshot(snapshot)
	case workerwire.TypeAttached:
		return m, nil
	case workerwire.TypeAck:
		if strings.HasPrefix(frame.RequestID, "lsp-") {
			var statuses []workerwire.LSPStatus
			if err := json.Unmarshal(frame.Payload, &statuses); err == nil {
				m.renderLSPStatuses(workerLSPStatuses(statuses))
			}
		}
		if strings.HasPrefix(frame.RequestID, "mcp-") {
			var statuses []workerwire.MCPStatus
			if err := json.Unmarshal(frame.Payload, &statuses); err == nil {
				m.workerMCPStatuses = statuses
				m.append(renderWorkerMCPStatuses(statuses))
			}
		}
		if strings.HasPrefix(frame.RequestID, "doctor-") {
			var result workerwire.ContextDoctorResult
			if err := json.Unmarshal(frame.Payload, &result); err == nil && result.Report != "" {
				m.append(result.Report)
			}
		}
		if frame.RequestID == m.workerHistoryRequest {
			var result workerwire.HistoryResult
			if err := json.Unmarshal(frame.Payload, &result); err == nil {
				m.setMessages(result.Messages)
				m.usage = result.Usage
				m.workerContextTokens = result.ContextTokens
				m.rebuildTranscript()
				if strings.HasPrefix(frame.RequestID, "compact-retry-") {
					m.future = nil
					m.append(dimStyle.Render("⟲ compaction undone — raw history restored; run /compact to re-compact"))
				} else if m.workerRewindRestore != "" {
					m.input.SetValue(m.workerRewindRestore)
					m.input.CursorEnd()
					m.growInput()
					m.workerRewindRestore = ""
				}
			}
			m.workerHistoryRequest = ""
		}
		if frame.RequestID == m.workerChdirRequest {
			var result workerwire.ChdirResult
			if err := json.Unmarshal(frame.Payload, &result); err == nil && result.CWD != "" {
				if err := os.Chdir(result.CWD); err != nil {
					m.append(errStyle.Render("/cd: controller: " + err.Error()))
				} else {
					m.shortCWD = shortCWD()
					m.append(dimStyle.Render("→ " + result.CWD))
				}
			}
			m.workerChdirRequest = ""
		}
		if strings.HasPrefix(frame.RequestID, "fork-") {
			var result workerwire.ForkResult
			if err := json.Unmarshal(frame.Payload, &result); err == nil && result.NewSessionID != "" {
				m.stopWorker()
				if err := m.resumeDisplay(result.NewSessionID); err != nil {
					m.append(errStyle.Render("fork resume failed: " + err.Error()))
				} else {
					m.append(dimStyle.Render(fmt.Sprintf("⑂ forked %q → %q (%s) — the original is under /resume", result.OldTitle, result.Title, result.NewSessionID)))
				}
			}
		}
		if strings.HasPrefix(frame.RequestID, "rename-") {
			var result workerwire.RenameResult
			if err := json.Unmarshal(frame.Payload, &result); err == nil && result.Title != "" {
				m.append(dimStyle.Render("✎ session renamed: " + result.Title))
			}
		}
	case workerwire.TypeDetachAck:
		if frame.RequestID == m.detachRequestID {
			m.workerDetached = true
			m.detachRequestID = ""
			return m, tea.Quit
		}
	case workerwire.TypeAlreadyControlled:
		m.append(errStyle.Render("worker already has a controlling client"))
		return m, nil
	case workerwire.TypeError:
		var payload workerwire.ErrorPayload
		_ = json.Unmarshal(frame.Payload, &payload)
		if frame.RequestID == m.detachRequestID {
			m.detachRequestID = ""
		}
		if frame.RequestID == m.workerHistoryRequest {
			m.workerHistoryRequest = ""
			m.workerRewindRestore = ""
		}
		if frame.RequestID == m.workerChdirRequest {
			m.workerChdirRequest = ""
		}
		if payload.Message != "" {
			m.append(errStyle.Render(payload.Message))
		}
	case workerwire.TypeEvent:
		var event workerEvent
		if err := json.Unmarshal(frame.Payload, &event); err != nil {
			return m, nil
		}
		return m, m.workerEvent(event)
	}
	return m, nil
}

func (m *model) applyWorkerSnapshot(snapshot workerwire.Snapshot) {
	m.usage = snapshot.Usage
	m.contextLimit = snapshot.ContextLimit
	m.modelID = snapshot.Model
	m.modelName = snapshot.ModelName
	m.provName = snapshot.Provider
	m.role = snapshot.Role
	m.protocol = snapshot.Protocol
	m.effort = snapshot.Effort
	if len(snapshot.Messages) > 0 {
		m.setMessages(snapshot.Messages)
		// The snapshot is authoritative: re-render from it so a turn that
		// finished between the initial DB load and this attach still shows
		// its completed blocks (and stale tool rows do not linger).
		m.rebuildTranscript()
	}
	if snapshot.Mode != "" {
		m.mode = snapshot.Mode
	}
	m.workerContextTokens = snapshot.ContextTokens
	m.workerState = snapshot.State
	m.workerDetached = snapshot.Detached
	m.workerTasks = make(map[string]workerwire.TaskState, len(snapshot.Tasks))
	m.workerLiveWork = workerStateBusy(snapshot.State)
	for _, task := range snapshot.Tasks {
		m.workerTasks[task.ID] = task
		m.workerLiveWork = m.workerLiveWork || task.Status == "running"
	}
	m.busy = workerStateBusy(snapshot.State)
	if m.busy {
		if snapshot.LiveText != "" {
			m.current = snapshot.LiveText
		}
		if snapshot.LivePlan != "" {
			m.planCurrent = snapshot.LivePlan
		}
		if snapshot.LiveThink != "" {
			if m.thinkStart.IsZero() && m.showThinking {
				m.thinkStart = m.nowFn()
			}
		}
		// Attaching mid-tool-call: restore the running row the protocol preserves.
		if snapshot.ActiveTool != "" {
			id := "attach-active"
			row := toolStyle.Render("⚒ "+snapshot.ActiveTool+" ") + dimStyle.Render("(resumed mid-call)")
			m.blocks = append(m.blocks, block{kind: blockToolRun, text: row, toolID: id, toolRunning: true})
		}
		m.refreshVP()
	}
	if snapshot.Pending != nil && m.permDialog == nil {
		m.permDialog = &permDialog{
			req:      tools.GateRequest{Tool: snapshot.Pending.Tool, Command: snapshot.Pending.Command, Rule: snapshot.Pending.Rule},
			workerID: snapshot.Pending.ID,
		}
	}
}

func (m *model) workerEvent(event workerEvent) tea.Cmd {
	switch event.Kind {
	case "route":
		if value, ok := decodeEvent[workerwire.ConfigureRequest](event.Data); ok {
			m.modelID = value.Model
			m.modelName, m.provName = value.ModelName, value.Provider
			if m.modelName == "" {
				m.modelName = value.Model
			}
			m.role, m.protocol = value.Role, value.Protocol
			if value.UpdateEffort {
				m.effort = value.Effort
			}
			if value.Mode != "" {
				m.mode = value.Mode
			}
		}
		return nil
	case "state":
		if value, ok := decodeEvent[workerStateEvent](event.Data); ok {
			m.workerState = value.State
			m.workerDetached = value.Detached
			if value.Mode != "" {
				m.mode = value.Mode
			}
			m.busy = workerStateBusy(value.State)
			m.workerLiveWork = m.busy || m.workerHasLiveTask()
		}
		return nil
	case "task":
		search.InvalidateFileIndex(mustWorkingDirectory())
		if value, ok := decodeEvent[workerwire.TaskState](event.Data); ok {
			if m.workerTasks == nil {
				m.workerTasks = make(map[string]workerwire.TaskState)
			}
			m.workerTasks[value.ID] = value
			m.workerLiveWork = m.busy || m.workerHasLiveTask()
			return func() tea.Msg { return taskUpdateMsg{} }
		}
		return nil
	case "mcp":
		if value, ok := decodeEvent[[]workerwire.MCPStatus](event.Data); ok {
			return func() tea.Msg { return mcpStatusMsg{statuses: value} }
		}
		return nil
	case "text":
		if value, ok := decodeEvent[string](event.Data); ok {
			return func() tea.Msg { return textMsg(value) }
		}
	case "think":
		if value, ok := decodeEvent[string](event.Data); ok {
			return func() tea.Msg { return thinkMsg(value) }
		}
	case workerwire.EventPlanDelta:
		if value, ok := decodeEvent[string](event.Data); ok {
			return func() tea.Msg { return planDeltaMsg(value) }
		}
	case workerwire.EventShellDone:
		search.InvalidateFileIndex(mustWorkingDirectory())
		if value, ok := decodeEvent[workerwire.ShellResult](event.Data); ok {
			return func() tea.Msg { return shellDoneMsg{cmd: value.Command, out: value.Output} }
		}
	case "steer":
		if value, ok := decodeEvent[string](event.Data); ok {
			return func() tea.Msg { return steeredMsg(value) }
		}
	case "tool_start":
		if value, ok := decodeEvent[workerToolStartEvent](event.Data); ok {
			return func() tea.Msg { return toolStartMsg{value.ID, value.Name, value.Args} }
		}
	case "tool_end":
		if value, ok := decodeEvent[workerToolEndEvent](event.Data); ok {
			return func() tea.Msg { return toolEndMsg{value.ID, value.Name, value.Result} }
		}
	case "usage":
		if value, ok := decodeEvent[models.Usage](event.Data); ok {
			m.usage.Add(value)
			if total := value.PromptTokens + value.CompletionTokens; total > 0 {
				m.workerContextTokens = total
			}
			return func() tea.Msg { return usageMsg(value) }
		}
	case "goal_update":
		if value, ok := decodeEvent[agent.GoalUpdate](event.Data); ok {
			return func() tea.Msg { return goalUpdateMsg{update: value} }
		}
	case "retry":
		if value, ok := decodeEvent[models.RetryEvent](event.Data); ok {
			return func() tea.Msg {
				return noticeMsg(fmt.Sprintf("⚠ request failed (%s) — retrying in %s (attempt %d/%d)", value.Err, value.Delay.Round(time.Millisecond), value.Attempt+1, value.Max))
			}
		}
	case "goal":
		if value, ok := decodeEvent[agent.GoalRecord](event.Data); ok {
			return func() tea.Msg { return goalUpdateRecordMsg{record: value} }
		}
	case "goal_from_context":
		if value, ok := decodeEvent[workerwire.GoalFromContextResult](event.Data); ok {
			var err error
			if value.Error != "" {
				err = errors.New(value.Error)
			}
			var record *agent.GoalRecord
			if value.Goal != nil {
				copy := *value.Goal
				record = &copy
			}
			goalText := ""
			if record != nil {
				goalText = record.Objective
			}
			return func() tea.Msg {
				return goalFromContextMsg{goal: goalText, record: record, usage: value.Usage, err: err}
			}
		}
	case "schedule":
		if value, ok := decodeEvent[string](event.Data); ok {
			return func() tea.Msg { return noticeMsg(value) }
		}
	case "compact":
		return func() tea.Msg { return noticeMsg("◎ compacted — raw history preserved") }
	case "compact_done":
		if value, ok := decodeEvent[workerwire.CompactResult](event.Data); ok {
			err := workerError(value.Error)
			if len(value.Messages) > 0 {
				m.setMessages(value.Messages)
				m.workerContextTokens = agent.EstimateTokens(value.Messages)
			}
			contextTokens := m.workerContextTokens
			return func() tea.Msg {
				return workerCompactDoneMsg{err: err, usage: value.Usage, contextTokens: contextTokens}
			}
		}
	case "permission_request":
		if value, ok := decodeEvent[workerwire.PermissionRequest](event.Data); ok {
			return func() tea.Msg {
				return workerPermissionMsg{approval: value.Approval}
			}
		}
	case "turn_done":
		search.InvalidateFileIndex(mustWorkingDirectory())
		if value, ok := decodeEvent[workerwire.TurnResult](event.Data); ok {
			err := workerError(value.Error)
			if len(value.Messages) > 0 {
				m.setMessages(value.Messages)
			}
			m.usage = value.Usage
			m.workerContextTokens = value.ContextTokens
			if value.ContextLimit > 0 {
				m.contextLimit = value.ContextLimit
			}
			if value.Model != "" {
				m.modelID = value.Model
			}
			if value.ModelName != "" {
				m.modelName = value.ModelName
			}
			if value.Provider != "" {
				m.provName = value.Provider
			}
			if value.Role != "" {
				m.role = value.Role
			}
			if value.Protocol != "" {
				m.protocol = value.Protocol
			}
			if value.Effort != "" || m.effort != "" {
				m.effort = value.Effort
			}
			var goal *agent.GoalRecord
			if value.Goal != nil {
				copy := *value.Goal
				goal = &copy
			}
			return func() tea.Msg {
				return turnDoneMsg{
					final: value.Final, err: err, at: value.At, snap: value.Snap,
					clean: value.Clean, plan: value.Plan, review: value.Review, reviewMarkdown: value.ReviewMarkdown,
					goal: goal, goalContinue: value.GoalContinue, goalUsage: value.Usage,
				}
			}
		}
	}
	return nil
}

func (m *model) workerHasLiveTask() bool {
	for _, task := range m.workerTasks {
		if task.Status == "running" {
			return true
		}
	}
	return false
}
