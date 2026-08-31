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
	"github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
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
	workerCautiousEnv = "GHG_WORKER_CAUTIOUS"
	workerSandboxEnv  = "GHG_WORKER_SANDBOX"
	workerNetworkEnv  = "GHG_WORKER_NETWORK"
	workerApprovalEnv = "GHG_WORKER_APPROVAL"
)

// Wire payload shapes live in internal/worker (workerwire); these aliases
// keep the historical local names readable.
type (
	workerInput             = workerwire.Input
	workerTurnResult        = workerwire.TurnResult
	workerCompactResult     = workerwire.CompactResult
	workerTaskState         = workerwire.TaskState
	workerApproval          = workerwire.Approval
	workerApprovalAnswer    = workerwire.ApprovalAnswer
	workerConfigureRequest  = workerwire.ConfigureRequest
	workerPlanRequest       = workerwire.PlanRequest
	workerPlanResult        = workerwire.PlanResult
	workerSnapshot          = workerwire.Snapshot
	workerPermissionRequest = workerwire.PermissionRequest
)

type workerEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func (m *model) startWorkerProcess(cautious bool) error {
	if m.agent == nil || m.store == nil {
		return errors.New("worker requires a configured agent and session store")
	}
	if m.sessionID == "" && !m.ensureSession() {
		return errors.New("worker could not reserve a session")
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
		m.workerRuntime = runtimeFile
		m.workerClient = client
		m.pumpWorker(client)
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
		workerRoleEnv:     m.agent.Role,
		workerEffortEnv:   m.agent.Effort,
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
	m.workerRuntime = runtimeFile
	m.workerClient = client
	m.pumpWorker(client)
	m.monitorWorker(proc, runtimeFile)
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
		client, err = workerwire.Dial(context.Background(), runtimeFile, m.workerLastSeq)
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
	m.workerClient = client
	m.workerRuntime = runtimeFile
	m.pumpWorker(client)
	return nil
}

func (m *model) monitorWorker(proc *workerwire.Process, runtimeFile workerwire.Runtime) {
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
			p.Send(workerErrorMsg{err: fmt.Errorf("worker exited: %w", err), process: proc})
		}
	}()
}

func (m *model) ensureWorker() bool {
	if m.workerClient != nil || m.workerStartFailed || m.prog == nil || m.agent == nil || m.store == nil {
		return m.workerClient != nil
	}
	if err := m.startWorkerProcess(m.cautious); err != nil {
		m.workerStartFailed = true
		config.LogEvent("worker.start", err.Error())
		return false
	}
	return true
}

func (m *model) syncWorkerConfiguration(updateEffort bool) {
	if m.workerClient == nil || m.agent == nil {
		return
	}
	if err := m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("configure"), workerConfigureRequest{
		Model: m.modelName, Provider: m.provName, Role: m.agent.Role,
		Effort: m.agent.Effort, UpdateEffort: updateEffort,
	}); err != nil {
		m.append(errStyle.Render("worker configuration failed: " + err.Error()))
	}
}

func mustWorkingDirectory() string {
	wd, _ := os.Getwd()
	return wd
}

func (m *model) pumpWorker(client *workerwire.Client) {
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
					p.Send(workerFrameMsg{frame: frame})
				}
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if err != nil && p != nil {
					p.Send(workerErrorMsg{err: err})
				}
			}
		}
		if p != nil {
			p.Send(workerErrorMsg{err: errors.New("worker connection closed")})
		}
	}()
}

func (m *model) stopWorker() {
	client, proc := m.workerClient, m.workerProcess
	detached := m.workerDetached
	m.workerClient, m.workerProcess = nil, nil
	m.workerRuntime = workerwire.Runtime{}
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

func (m *model) submitWorkerTurn(text string, authored bool, prepared string, parts []llm.ContentPart, at int, snap string, goalCtx *goal.Record) (tea.Model, tea.Cmd) {
	if m.workerClient == nil {
		return m, nil
	}
	if m.agent != nil {
		message := llm.Message{Role: "user", Content: prepared, Parts: parts, Authored: authored}
		if authored {
			now := m.nowFn()
			message.SentAt = &now
		}
		m.agent.Messages = append(m.agent.Messages, message)
	}
	requestID := workerRequestID("turn")
	systemPrompt := m.sysPrompt
	if m.agent != nil && len(m.agent.Messages) > 0 {
		systemPrompt = m.agent.Messages[0].Content
	}
	m.cancel = func() {
		if m.workerClient != nil {
			_ = m.workerClient.Send(workerwire.CommandCancel, requestID+"-cancel", nil)
		}
	}
	if err := m.workerClient.Send(workerwire.CommandInput, requestID, workerInput{
		Input: prepared, Authored: authored, Parts: parts, Goal: goalCtx,
		SystemPrompt: systemPrompt, At: at, Snap: snap,
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
	if frame.Seq > m.workerLastSeq {
		m.workerLastSeq = frame.Seq
	}
	switch frame.Type {
	case workerwire.TypeSnapshot:
		var envelope workerwire.SnapshotEnvelope
		if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
			return m, nil
		}
		var snapshot workerSnapshot
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

func (m *model) applyWorkerSnapshot(snapshot workerSnapshot) {
	if snapshot.ModelName != "" {
		m.modelName = snapshot.ModelName
	}
	if snapshot.Provider != "" {
		m.provName = snapshot.Provider
	}
	if m.agent != nil {
		m.agent.SetUsage(snapshot.Usage)
		m.agent.ContextLimit = snapshot.ContextLimit
		m.agent.Model = snapshot.Model
		m.agent.ModelName = snapshot.ModelName
		m.agent.Provider = snapshot.Provider
		m.agent.Role = snapshot.Role
		m.agent.Protocol = snapshot.Protocol
		m.agent.Effort = snapshot.Effort
		if len(snapshot.Messages) > 0 {
			m.agent.Messages = append([]llm.Message(nil), snapshot.Messages...)
			// The snapshot is authoritative: re-render from it so a turn that
			// finished between the initial DB load and this attach still shows
			// its completed blocks (and stale tool rows do not linger).
			m.rebuildTranscript()
			m.saved = len(m.agent.Messages)
		}
	}
	m.workerContextTokens = snapshot.ContextTokens
	m.workerState = snapshot.State
	m.workerDetached = snapshot.Detached
	m.workerTasks = make(map[string]workerTaskState, len(snapshot.Tasks))
	m.workerLiveWork = snapshot.State == workerwire.StateRunning || snapshot.State == workerwire.StateWaitingApproval
	for _, task := range snapshot.Tasks {
		m.workerTasks[task.ID] = task
		m.workerLiveWork = m.workerLiveWork || task.Status == "running"
	}
	m.busy = snapshot.State == workerwire.StateRunning || snapshot.State == workerwire.StateWaitingApproval
	if m.busy {
		if snapshot.LiveText != "" {
			m.current = snapshot.LiveText
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
		var value workerConfigureRequest
		if json.Unmarshal(event.Data, &value) == nil {
			if m.agent != nil {
				m.agent.Model = value.Model
				m.agent.ModelName = value.ModelName
				if m.agent.ModelName == "" {
					m.agent.ModelName = value.Model
				}
				m.agent.Provider = value.Provider
				m.agent.Role = value.Role
				m.agent.Protocol = value.Protocol
				if value.UpdateEffort {
					m.agent.Effort = value.Effort
				}
			}
			m.modelName, m.provName = value.ModelName, value.Provider
		}
		return nil
	case "plan_done":
		var value workerPlanResult
		if json.Unmarshal(event.Data, &value) == nil {
			var err error
			if value.Error != "" {
				err = errors.New(value.Error)
			}
			return func() tea.Msg { return planProposalMsg{plan: value.Plan, err: err} }
		}
		return nil
	case "state":
		var value struct {
			State    workerwire.State `json:"state"`
			Detached bool             `json:"detached"`
		}
		if json.Unmarshal(event.Data, &value) == nil {
			m.workerState = value.State
			m.workerDetached = value.Detached
			m.busy = value.State == workerwire.StateRunning || value.State == workerwire.StateWaitingApproval
			m.workerLiveWork = m.busy || m.workerHasLiveTask()
		}
		return nil
	case "task":
		var value workerTaskState
		if json.Unmarshal(event.Data, &value) == nil {
			if m.workerTasks == nil {
				m.workerTasks = make(map[string]workerTaskState)
			}
			m.workerTasks[value.ID] = value
			m.workerLiveWork = m.busy || m.workerHasLiveTask()
			return func() tea.Msg { return taskUpdateMsg{} }
		}
		return nil
	case "text":
		var value string
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg { return textMsg(value) }
		}
	case "think":
		var value string
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg { return thinkMsg(value) }
		}
	case "steer":
		var value string
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg { return steeredMsg(value) }
		}
	case "tool_start":
		var value struct{ ID, Name, Args string }
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg { return toolStartMsg{value.ID, value.Name, value.Args} }
		}
	case "tool_end":
		var value struct{ ID, Name, Result string }
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg { return toolEndMsg{value.ID, value.Name, value.Result} }
		}
	case "usage":
		var value llm.Usage
		if json.Unmarshal(event.Data, &value) == nil {
			if m.agent != nil {
				m.agent.AddUsage(value)
			}
			if total := value.PromptTokens + value.CompletionTokens; total > 0 {
				m.workerContextTokens = total
			}
			return func() tea.Msg { return usageMsg(value) }
		}
	case "goal_update":
		var value agent.GoalUpdate
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg { return goalUpdateMsg{update: value} }
		}
	case "retry":
		var value llm.RetryEvent
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg {
				return noticeMsg(fmt.Sprintf("⚠ request failed (%s) — retrying in %s (attempt %d/%d)", value.Err, value.Delay.Round(time.Millisecond), value.Attempt+1, value.Max))
			}
		}
	case "compact":
		return func() tea.Msg { return noticeMsg("◎ compacted — raw history preserved") }
	case "compact_done":
		var value workerCompactResult
		if json.Unmarshal(event.Data, &value) == nil {
			var err error
			if value.Error != "" {
				if strings.Contains(strings.ToLower(value.Error), "context canceled") || strings.Contains(strings.ToLower(value.Error), "interrupted") {
					err = context.Canceled
				} else {
					err = errors.New(value.Error)
				}
			}
			if m.agent != nil && len(value.Messages) > 0 {
				m.agent.Messages = append([]llm.Message(nil), value.Messages...)
				m.workerContextTokens = m.agent.ContextTokens()
			}
			return func() tea.Msg {
				return workerCompactDoneMsg{err: err, usage: value.Usage}
			}
		}
	case "permission_request":
		var value workerPermissionRequest
		if json.Unmarshal(event.Data, &value) == nil {
			return func() tea.Msg {
				return workerPermissionMsg{approval: value.Approval}
			}
		}
	case "turn_done":
		var value workerTurnResult
		if json.Unmarshal(event.Data, &value) == nil {
			var err error
			if value.Error != "" {
				if strings.Contains(strings.ToLower(value.Error), "context canceled") || strings.Contains(strings.ToLower(value.Error), "interrupted") {
					err = context.Canceled
				} else {
					err = errors.New(value.Error)
				}
			}
			if m.agent != nil && len(value.Messages) > 0 {
				m.agent.Messages = append([]llm.Message(nil), value.Messages...)
				m.workerContextTokens = m.agent.ContextTokens()
			}
			return func() tea.Msg {
				return turnDoneMsg{final: value.Final, err: err, at: value.At, snap: value.Snap, clean: workspaceClean(), goalUsage: value.Usage}
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
