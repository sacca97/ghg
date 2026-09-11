package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

type workerApprovalFlight struct {
	done     chan struct{}
	request  workerApproval
	decision tools.GateDecision
	redirect string
	once     sync.Once
}

type workerQuestionFlight struct {
	done    chan struct{}
	request workerQuestionRequest
	answers []workerQuestionAnswer
	err     error
	once    sync.Once
}

func (w *workerProcessState) Snapshot(context.Context) (any, error) {
	w.mu.Lock()
	state, detached, activeTool := w.state, w.detached, w.activeTool
	modelName, providerName, role, mode := w.modelName, w.provider, w.role, w.mode
	var approval string
	if w.runtime != nil {
		approval = string(w.runtime.CurrentApprovalMode())
	}
	ag := w.ag
	var modelID, protocol, effort string
	var contextLimit int
	if ag != nil {
		modelID, protocol, effort = ag.Model, ag.Protocol, ag.Effort
		contextLimit = ag.ContextLimit
	}
	w.mu.Unlock()
	live := w.liveSnapshot()
	if ag == nil {
		return workerSnapshot{SessionID: w.sessionID, State: state, Detached: detached, Mode: mode, Approval: approval}, nil
	}
	return workerSnapshot{
		SessionID: w.sessionID, State: state, Detached: detached,
		Model: modelID, ModelName: modelName, Provider: providerName,
		Role: role, Protocol: protocol, Effort: effort, Approval: approval, Mode: mode,
		ContextLimit: contextLimit, ContextTokens: ag.ContextTokens(),
		Usage: ag.Usage(), Messages: boundedWorkerMessages(ag.MessagesSnapshot()),
		Tasks: w.taskStates(), Pending: w.pendingState(), PendingQuestion: w.pendingQuestionState(), ActiveTool: activeTool,
		LiveText: live.text, LiveThink: live.think, LiveTool: live.tool, LivePlan: live.plan,
	}, nil
}

func (w *workerProcessState) Command(ctx context.Context, command workerwire.Command) (workerwire.CommandResult, error) {
	switch command.Name {
	case workerwire.CommandInput:
		var input workerInput
		if err := json.Unmarshal(command.Payload, &input); err != nil || strings.TrimSpace(input.Input) == "" {
			return workerwire.CommandResult{}, errors.New("worker input is invalid")
		}
		if w.ag != nil && w.ag.ReviewPending() && !input.Continue && !input.ReviewMode {
			return workerwire.CommandResult{}, errors.New("review is interrupted; use continue to resume it or review to start a new review")
		}
		if !w.startTurn(input) {
			return workerwire.CommandResult{}, errors.New("worker is busy or stopping")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandCancel:
		w.mu.Lock()
		cancel := w.activeCancel
		w.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"cancelled":true}`)}, nil
	case workerwire.CommandApprove:
		var answer workerApprovalAnswer
		if err := json.Unmarshal(command.Payload, &answer); err != nil {
			return workerwire.CommandResult{}, errors.New("approval answer is invalid")
		}
		if !w.answerApproval(answer) {
			return workerwire.CommandResult{}, errors.New("approval request is no longer pending")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandAnswerQuestion:
		var answer workerwire.QuestionAnswerRequest
		if err := json.Unmarshal(command.Payload, &answer); err != nil {
			return workerwire.CommandResult{}, errors.New("question answer is invalid")
		}
		if !w.answerQuestion(answer) {
			return workerwire.CommandResult{}, errors.New("question request is no longer pending")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandConfigure:
		var request workerConfigureRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("worker configuration is invalid")
		}
		if err := w.configure(request); err != nil {
			return workerwire.CommandResult{}, err
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandCompact:
		if !w.startCompact() {
			return workerwire.CommandResult{}, errors.New("worker is busy or stopping")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandCompactRetry:
		result, err := w.compactRetry()
		if err != nil {
			return workerwire.CommandResult{}, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal compact retry: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandRewind:
		var request workerwire.RewindRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("rewind payload is invalid")
		}
		result, err := w.rewind(request)
		if err != nil {
			return workerwire.CommandResult{}, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal rewind: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandGoal:
		var request workerwire.GoalRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("goal payload is invalid")
		}
		record, err := w.updateGoal(request)
		if err != nil {
			return workerwire.CommandResult{}, err
		}
		data, err := json.Marshal(record)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal goal: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandGoalFromContext:
		var request workerwire.GoalFromContextRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("goal-from-context payload is invalid")
		}
		if !w.startGoalFromContext(request.Window) {
			return workerwire.CommandResult{}, errors.New("worker is busy or stopping")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandShell:
		var request workerwire.ShellRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil || strings.TrimSpace(request.Command) == "" {
			return workerwire.CommandResult{}, errors.New("worker shell payload is invalid")
		}
		if !w.startShell(strings.TrimSpace(request.Command)) {
			return workerwire.CommandResult{}, errors.New("worker shell is busy or stopping")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandChdir:
		if err := w.requireIdleHistory(); err != nil {
			return workerwire.CommandResult{}, err
		}
		var dir string
		if err := json.Unmarshal(command.Payload, &dir); err != nil || dir == "" {
			return workerwire.CommandResult{}, errors.New("worker chdir target is invalid")
		}
		canonical, err := canonicalDir(dir)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("worker chdir: %w", err)
		}
		if err := os.Chdir(canonical); err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("worker chdir: %w", err)
		}
		data, err := json.Marshal(workerwire.ChdirResult{CWD: canonical})
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal worker chdir: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandAppend:
		var request workerwire.AppendRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil || strings.TrimSpace(request.Content) == "" {
			return workerwire.CommandResult{}, errors.New("worker append payload is invalid")
		}
		w.appendContent(request.Content)
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandFork:
		var request workerwire.ForkRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("fork payload is invalid")
		}
		result, err := w.fork(request)
		if err != nil {
			return workerwire.CommandResult{}, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal fork: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandRename:
		var request workerwire.RenameRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("rename payload is invalid")
		}
		result, err := w.rename(request)
		if err != nil {
			return workerwire.CommandResult{}, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal rename: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandNotify:
		var request workerwire.NotifyRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return workerwire.CommandResult{}, errors.New("notify payload is invalid")
		}
		result, message, err := w.notifyCommand(ctx, request)
		if err != nil {
			return workerwire.CommandResult{}, err
		}
		w.publish("notice", message, true)
		data, err := json.Marshal(result)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal notify result: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandDetach:
		w.mu.Lock()
		allowed := w.state == workerwire.StateRunning || w.state == workerwire.StateWaitingApproval || w.state == workerwire.StateWaitingQuestion || w.hasLiveWork()
		w.mu.Unlock()
		if !allowed {
			return workerwire.CommandResult{}, errors.New("nothing running to detach")
		}
		return workerwire.CommandResult{
			Payload: json.RawMessage(`{"detached":true}`), Detach: true,
			AfterAck: func() {
				w.transition(func() (workerwire.State, bool, string, bool) {
					return w.state, true, "clientless continuation authorized", true
				})
			},
		}, nil
	case workerwire.CommandStop:
		w.requestStop(false, "stop requested by client")
		return workerwire.CommandResult{Payload: json.RawMessage(`{"stopping":true}`)}, nil
	case workerwire.CommandPing:
		return workerwire.CommandResult{Payload: json.RawMessage(`{"ok":true}`)}, nil
	case workerwire.CommandLSPStatus:
		if w.lsp == nil {
			return workerwire.CommandResult{}, errors.New("worker LSP manager is unavailable")
		}
		statuses := w.lsp.Statuses()
		payload := make([]workerwire.LSPStatus, len(statuses))
		for i, status := range statuses {
			payload[i] = workerwire.LSPStatus{
				Name: status.Name, Root: status.Root, State: status.State, Error: status.Err,
			}
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal worker LSP status: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandMCPStatus:
		data, err := json.Marshal(w.mcpStatuses())
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal worker MCP status: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandMCPReconnect, workerwire.CommandMCPEnable, workerwire.CommandMCPDisable:
		if err := w.requireIdleHistory(); err != nil {
			return workerwire.CommandResult{}, err
		}
		if w.mcp == nil {
			return workerwire.CommandResult{}, errors.New("worker MCP manager is unavailable")
		}
		var request workerwire.MCPRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil || strings.TrimSpace(request.Name) == "" {
			return workerwire.CommandResult{}, errors.New("MCP server name is invalid")
		}
		var ok bool
		switch command.Name {
		case workerwire.CommandMCPReconnect:
			ok = w.mcp.Reconnect(request.Name)
		case workerwire.CommandMCPEnable:
			ok = w.mcp.Enable(request.Name)
		case workerwire.CommandMCPDisable:
			ok = w.mcp.Disable(request.Name)
		}
		if !ok {
			return workerwire.CommandResult{}, fmt.Errorf("no MCP server named %s", request.Name)
		}
		if command.Name == workerwire.CommandMCPEnable || command.Name == workerwire.CommandMCPDisable {
			if err := w.persistMCPConfig(request.Name); err != nil {
				return workerwire.CommandResult{}, err
			}
		}
		data, err := json.Marshal(w.mcpStatuses())
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal worker MCP status: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	case workerwire.CommandContextDoctor:
		data, err := json.Marshal(w.contextDoctorReport())
		if err != nil {
			return workerwire.CommandResult{}, fmt.Errorf("marshal context doctor: %w", err)
		}
		return workerwire.CommandResult{Payload: data}, nil
	default:
		return workerwire.CommandResult{}, fmt.Errorf("worker command %q is not implemented", command.Name)
	}
}

func (w *workerProcessState) mcpStatuses() []workerwire.MCPStatus {
	if w.mcp == nil {
		return []workerwire.MCPStatus{}
	}
	servers := append(w.mcp.Statuses(), w.mcp.Blocked()...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	statuses := make([]workerwire.MCPStatus, len(servers))
	for i, server := range servers {
		statuses[i] = workerwire.MCPStatus{
			Name: server.Name, State: server.Status.String(), Note: server.Note,
			Error: server.Err, Tools: server.Tools, Source: server.Source,
		}
	}
	return statuses
}

func (w *workerProcessState) persistMCPConfig(name string) error {
	live, ok := w.mcp.Config(name)
	if !ok {
		return fmt.Errorf("no MCP server named %s", name)
	}
	if w.cfg.MCPServers == nil {
		w.cfg.MCPServers = map[string]config.MCPServer{}
	}
	enabled := !live.Disabled()
	w.cfg.MCPServers[name] = config.MCPServer{
		Command: live.Command, Env: live.Env, Cwd: live.Cwd,
		URL: live.URL, Headers: live.Headers, Enabled: &enabled,
		Note: live.Note, StartupTimeout: live.StartupTimeout, ToolTimeout: live.ToolTimeout,
	}
	if err := w.cfg.Save(); err != nil {
		return fmt.Errorf("MCP config save failed: %w", err)
	}
	return nil
}

func canonicalDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("target is not a directory")
	}
	return resolved, nil
}

// configure changes the live approval setting or the idle worker's route. Keeping the existing Agent
// preserves its task registry, observations, history, and session resources;
// the replacement agent is used only as a route builder.
func (w *workerProcessState) configure(request workerConfigureRequest) error {
	if strings.TrimSpace(request.Approval) != "" {
		return w.configureApproval(request.Approval)
	}
	w.mu.Lock()
	if w.activeCancel != nil || w.stopRequested || w.state == workerwire.StateStopping {
		w.mu.Unlock()
		return errors.New("worker is busy or stopping")
	}
	if w.ag == nil {
		w.mu.Unlock()
		return errors.New("worker agent is unavailable")
	}
	modelName, providerName := strings.TrimSpace(request.Model), strings.TrimSpace(request.Provider)
	role := strings.TrimSpace(request.Role)
	if role == "" {
		role = w.role
	}
	systemPrompt := ""
	if messages := w.ag.MessagesSnapshot(); len(messages) > 0 {
		systemPrompt = messages[0].Content
	}
	candidate, resolvedModel, resolvedProvider, err := agent.NewConfigured(agent.BuildOptions{
		Config: w.cfg, Profiles: w.profiles, Model: modelName, Provider: providerName,
		Role: role, SystemPrompt: systemPrompt,
	})
	if err != nil {
		w.mu.Unlock()
		return err
	}
	effort := w.ag.Effort
	if request.UpdateEffort {
		effort = request.Effort
	}
	effort = workerEffortForModel(resolvedProvider, candidate.Model, effort, candidate.ReasoningToggle)
	effortChanged := effort != w.ag.Effort
	w.ag.Backend = candidate.Backend
	w.ag.Model = candidate.Model
	w.ag.ModelName = resolvedModel
	w.ag.Provider = resolvedProvider
	w.ag.Protocol = candidate.Protocol
	w.ag.MaxTokens = candidate.MaxTokens
	w.ag.ContextLimit = candidate.ContextLimit
	w.ag.ReasoningToggle = candidate.ReasoningToggle
	w.ag.ReasoningEfforts = append([]string(nil), candidate.ReasoningEfforts...)
	w.ag.SubagentFactory = candidate.SubagentFactory
	w.ag.Role = role
	if request.DynamicReasoning != nil {
		dynamicReasoning := *request.DynamicReasoning
		w.ag.DynamicReasoning = &dynamicReasoning
	}
	w.ag.Effort = effort
	if request.Mode != "" {
		w.mode = request.Mode
		w.ag.PlanMode = (request.Mode == "plan")
	}
	if request.UpdateCompactThreshold && request.CompactThreshold > 0 {
		w.ag.CompactThreshold = request.CompactThreshold
	}
	w.modelName, w.provider, w.role = resolvedModel, resolvedProvider, role
	configureWorkerCompaction(w.ag, w.cfg, w.profiles, systemPrompt)
	protocol := w.ag.Protocol
	modelID := w.ag.Model
	mode := w.mode
	state, detached := w.state, w.detached
	w.mu.Unlock()
	if w.store != nil {
		_ = w.store.SetRoute(w.sessionID, resolvedModel, resolvedProvider)
		if request.UpdateEffort || effortChanged {
			_ = w.store.SetEffort(w.sessionID, effort)
		}
	}
	w.publish("route", workerConfigureRequest{
		Model: modelID, ModelName: resolvedModel, Provider: resolvedProvider,
		Role: role, Protocol: protocol, Effort: effort, UpdateEffort: true,
		Mode: mode,
	}, true)
	w.setState(state, detached, "route changed")
	return nil
}

func (w *workerProcessState) configureApproval(value string) error {
	mode, err := tools.ParseApprovalMode(value)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if w.runtime == nil || w.cfg == nil {
		w.mu.Unlock()
		return errors.New("worker approval runtime is unavailable")
	}
	if w.cfg.Execution == nil {
		w.cfg.Execution = &config.ExecutionConfig{}
	}
	w.cfg.Execution.Approval = string(mode)
	runtime := w.runtime
	cfg := w.cfg
	w.mu.Unlock()

	runtime.SetApprovalMode(mode)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save approval mode: %w", err)
	}
	w.publish("route", workerConfigureRequest{Approval: string(mode)}, true)
	return nil
}

func workerEffortForModel(provider, model, current string, toggle bool) string {
	current = strings.TrimSpace(current)
	if strings.EqualFold(current, "off") || strings.EqualFold(current, "none") {
		current = ""
	}
	info := config.LoadCatalogs()[provider].Find(model)
	if info == nil || (!info.ReasoningKnown && len(info.ReasoningEfforts) == 0 && !info.ReasoningToggle && !toggle) {
		return current
	}
	levels := []string{""}
	if info.ReasoningToggle && len(info.ReasoningEfforts) == 0 {
		levels = append(levels, "on")
	} else {
		for _, effort := range info.ReasoningEfforts {
			effort = strings.TrimSpace(effort)
			if effort == "" || strings.EqualFold(effort, "off") || strings.EqualFold(effort, "none") {
				continue
			}
			seen := false
			for _, level := range levels {
				if level == effort {
					seen = true
					break
				}
			}
			if !seen {
				levels = append(levels, effort)
			}
		}
	}
	for _, level := range levels {
		if current != "" && strings.EqualFold(level, current) {
			return level
		}
	}
	// An explicit off setting stays off. For a configured level the model does
	// not advertise, use its least expensive advertised level; never silently
	// escalate a session to the maximum.
	if current == "" || len(levels) == 1 {
		return ""
	}
	return levels[1]
}

func (w *workerProcessState) Attached(context.Context) {
	w.transition(func() (workerwire.State, bool, string, bool) {
		return w.state, false, "controller attached", true
	})
}

// A lost controller is recoverable; intentional shutdown sends CommandStop.
func (w *workerProcessState) Disconnected(context.Context, bool) {}

func (w *workerProcessState) publish(kind string, data any, important bool) {
	if w.server != nil {
		_, _ = w.server.Publish(kind, data, important)
	}
}

func (w *workerProcessState) humanGate(ctx context.Context, req tools.GateRequest) (tools.GateDecision, string) {
	if w.perms != nil && w.perms.CoveredBy(req) {
		return tools.GateAllowOnce, ""
	}
	id := fmt.Sprintf("approval-%d", w.approvalSeq.Add(1))
	pending := &workerApproval{ID: id, Tool: req.Tool, Command: req.Command, Rule: req.Rule}
	flight := &workerApprovalFlight{done: make(chan struct{}), request: *pending}

	w.transition(func() (workerwire.State, bool, string, bool) {
		w.pending[id] = flight
		return workerwire.StateWaitingApproval, w.detached, "approval requested", true
	})
	w.publish("permission_request", workerPermissionRequest{Approval: *pending}, true)
	select {
	case <-flight.done:
	case <-ctx.Done():
		flight.once.Do(func() {
			flight.decision = tools.GateReject
			flight.redirect = "worker operation canceled"
			close(flight.done)
		})
	}

	var decision tools.GateDecision
	var redirect string
	w.transition(func() (workerwire.State, bool, string, bool) {
		delete(w.pending, id)
		decision, redirect = flight.decision, flight.redirect
		if w.stopRequested {
			return w.state, w.detached, "", false
		}
		return workerwire.StateRunning, w.detached, "approval answered", true
	})
	return decision, redirect
}

func (w *workerProcessState) questionGate(ctx context.Context, request agent.QuestionRequest) (agent.QuestionResult, error) {
	questions := make([]workerwire.Question, len(request.Questions))
	for i, question := range request.Questions {
		options := make([]workerwire.QuestionOption, len(question.Options))
		for j, option := range question.Options {
			options[j] = workerwire.QuestionOption{Label: option.Label, Description: option.Description}
		}
		questions[i] = workerwire.Question{ID: question.ID, Question: question.Question, Options: options}
	}
	id := fmt.Sprintf("question-%d", w.questionSeq.Add(1))
	flight := &workerQuestionFlight{done: make(chan struct{}), request: workerQuestionRequest{ID: id, Questions: questions}}
	w.mu.Lock()
	if w.pendingQuestion != nil {
		w.mu.Unlock()
		return agent.QuestionResult{}, errors.New("another question is already pending")
	}
	w.pendingQuestion = flight
	w.mu.Unlock()
	w.transition(func() (workerwire.State, bool, string, bool) {
		return workerwire.StateWaitingQuestion, w.detached, "question requested", true
	})
	w.publish(workerwire.EventQuestionRequest, flight.request, true)
	select {
	case <-flight.done:
	case <-ctx.Done():
		flight.once.Do(func() {
			flight.err = ctx.Err()
			close(flight.done)
		})
	}
	w.transition(func() (workerwire.State, bool, string, bool) {
		if w.pendingQuestion == flight {
			w.pendingQuestion = nil
		}
		if w.stopRequested {
			return w.state, w.detached, "", false
		}
		return workerwire.StateRunning, w.detached, "question answered", true
	})
	if flight.err != nil {
		return agent.QuestionResult{}, flight.err
	}
	answers := make([]agent.QuestionAnswer, len(flight.answers))
	for i, answer := range flight.answers {
		answers[i] = agent.QuestionAnswer{ID: answer.ID, Value: answer.Value}
	}
	return agent.QuestionResult{Answers: answers}, nil
}

func (w *workerProcessState) answerQuestion(answer workerwire.QuestionAnswerRequest) bool {
	w.mu.Lock()
	flight := w.pendingQuestion
	w.mu.Unlock()
	if flight == nil || flight.request.ID != answer.ID {
		return false
	}
	if !answer.Cancelled {
		if len(answer.Answers) != len(flight.request.Questions) {
			return false
		}
		for i, value := range answer.Answers {
			if value.ID != flight.request.Questions[i].ID || strings.TrimSpace(value.Value) == "" || len(value.Value) > 4096 {
				return false
			}
		}
	}
	flight.once.Do(func() {
		if answer.Cancelled {
			flight.err = context.Canceled
		} else {
			flight.answers = append([]workerwire.QuestionAnswer(nil), answer.Answers...)
		}
		close(flight.done)
	})
	return true
}

func (w *workerProcessState) answerApproval(answer workerApprovalAnswer) bool {
	w.mu.Lock()
	flight := w.pending[answer.ID]
	w.mu.Unlock()
	if flight == nil {
		return false
	}
	decision := tools.GateReject
	switch answer.Decision {
	case "allow_once":
		decision = tools.GateAllowOnce
	case "allow_always":
		decision = tools.GateAllowAlways
		if w.perms != nil {
			rule := flight.request.Rule
			if flight.request.Tool != "bash" {
				rule = flight.request.Command
			}
			w.perms.AllowAlways(flight.request.Tool, rule)
		}
	case "reject":
	default:
		return false
	}
	flight.once.Do(func() {
		flight.decision, flight.redirect = decision, answer.Redirect
		close(flight.done)
	})
	return true
}

func (w *workerProcessState) rejectApprovals(reason string) {
	w.mu.Lock()
	flights := make([]*workerApprovalFlight, 0, len(w.pending))
	for _, flight := range w.pending {
		flights = append(flights, flight)
	}
	w.mu.Unlock()
	for _, flight := range flights {
		flight.once.Do(func() {
			flight.decision = tools.GateReject
			flight.redirect = reason
			close(flight.done)
		})
	}
}

func (w *workerProcessState) rejectQuestion(reason string) {
	w.mu.Lock()
	flight := w.pendingQuestion
	w.mu.Unlock()
	if flight != nil {
		flight.once.Do(func() {
			flight.err = errors.New(reason)
			close(flight.done)
		})
	}
}

func (w *workerProcessState) pendingState() *workerApproval {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, request := range w.pending {
		return &request.request
	}
	return nil
}

func (w *workerProcessState) pendingQuestionState() *workerQuestionRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pendingQuestion == nil {
		return nil
	}
	request := w.pendingQuestion.request
	request.Questions = append([]workerwire.Question(nil), request.Questions...)
	return &request
}

const workerLiveTailBytes = 128 << 10

type workerLiveSnapshot struct {
	text, think, tool, plan string
}

func (w *workerProcessState) appendLive(kind, value string) {
	if value == "" {
		return
	}
	w.liveMu.Lock()
	switch kind {
	case "text":
		w.liveText = appendWorkerTail(w.liveText, value)
	case "think":
		w.liveThink = appendWorkerTail(w.liveThink, value)
	case "tool_output":
		w.liveToolOutput = appendWorkerTail(w.liveToolOutput, value)
	case "plan":
		w.livePlan = appendWorkerTail(w.livePlan, value)
	}
	w.liveMu.Unlock()
}

func (w *workerProcessState) liveSnapshot() workerLiveSnapshot {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	return workerLiveSnapshot{text: w.liveText, think: w.liveThink, tool: w.liveToolOutput, plan: w.livePlan}
}

func (w *workerProcessState) clearLive() {
	w.liveMu.Lock()
	w.liveText, w.liveThink, w.liveToolOutput, w.livePlan = "", "", "", ""
	w.liveMu.Unlock()
}

func appendWorkerTail(current, value string) string {
	if len(value) >= workerLiveTailBytes {
		return value[len(value)-workerLiveTailBytes:]
	}
	current += value
	if len(current) > workerLiveTailBytes {
		current = current[len(current)-workerLiveTailBytes:]
	}
	return current
}

func (w *workerProcessState) taskStates() []workerTaskState {
	if w.ag == nil {
		return nil
	}
	tasks := w.ag.Tasks().List()
	out := make([]workerTaskState, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, workerTask(task))
	}
	return out
}
