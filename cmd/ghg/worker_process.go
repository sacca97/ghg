package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/config"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/provider"
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
	workerCautiousEnv = "GHG_WORKER_CAUTIOUS"
	workerSandboxEnv  = "GHG_WORKER_SANDBOX"
	workerNetworkEnv  = "GHG_WORKER_NETWORK"
	workerApprovalEnv = "GHG_WORKER_APPROVAL"
)

type workerInput struct {
	Input        string            `json:"input"`
	Authored     bool              `json:"authored"`
	Parts        []llm.ContentPart `json:"parts,omitempty"`
	Goal         *goalstate.Record `json:"goal,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	At           int               `json:"at"`
	Snap         string            `json:"snap,omitempty"`
}

type workerTurnResult struct {
	Final    string        `json:"final,omitempty"`
	Error    string        `json:"error,omitempty"`
	Usage    llm.Usage     `json:"usage"`
	At       int           `json:"at"`
	Snap     string        `json:"snap,omitempty"`
	Messages []llm.Message `json:"messages,omitempty"`
}

type workerCompactResult struct {
	Error    string        `json:"error,omitempty"`
	Usage    llm.Usage     `json:"usage"`
	Messages []llm.Message `json:"messages,omitempty"`
}

type workerTaskState struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Prompt      string    `json:"prompt,omitempty"`
	Status      string    `json:"status"`
	Report      string    `json:"report,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
}

type workerApproval struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Command string `json:"command"`
	Rule    string `json:"rule"`
}

type workerApprovalAnswer struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Redirect string `json:"redirect,omitempty"`
}

type workerConfigureRequest struct {
	Model        string `json:"model,omitempty"`
	ModelName    string `json:"model_name,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Role         string `json:"role,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	Effort       string `json:"effort,omitempty"`
	UpdateEffort bool   `json:"update_effort,omitempty"`
}

type workerPlanRequest struct {
	Goal string `json:"goal"`
}

type workerPlanResult struct {
	Plan  agent.Plan `json:"plan"`
	Error string     `json:"error,omitempty"`
}

type workerSnapshot struct {
	SessionID     string            `json:"session_id"`
	State         workerwire.State  `json:"state"`
	Detached      bool              `json:"detached"`
	Model         string            `json:"model"`
	ModelName     string            `json:"model_name"`
	Provider      string            `json:"provider"`
	Role          string            `json:"role,omitempty"`
	Protocol      string            `json:"protocol,omitempty"`
	Effort        string            `json:"effort,omitempty"`
	ContextLimit  int               `json:"context_limit,omitempty"`
	ContextTokens int               `json:"context_tokens"`
	Usage         llm.Usage         `json:"usage"`
	Messages      []llm.Message     `json:"messages,omitempty"`
	Tasks         []workerTaskState `json:"tasks,omitempty"`
	Pending       *workerApproval   `json:"pending_approval,omitempty"`
	ActiveTool    string            `json:"active_tool,omitempty"`
	LiveText      string            `json:"live_text,omitempty"`
	LiveThink     string            `json:"live_think,omitempty"`
	LiveTool      string            `json:"live_tool_output,omitempty"`
}

type workerPermissionRequest struct {
	Approval workerApproval `json:"approval"`
}

type workerProcessState struct {
	mu              sync.Mutex
	cfg             *config.Config
	profiles        provider.Profiles
	definitions     map[string]agent.Definition
	server          *workerwire.Server
	ag              *agent.Agent
	store           *session.Store
	artifacts       *artifact.Store
	runtime         *tools.ToolRuntime
	runtimeClean    func()
	lsp             *lsp.Manager
	mcp             *mcp.Manager
	runtimeFile     workerwire.Runtime
	sessionID       string
	modelName       string
	provider        string
	role            string
	saved           int
	state           workerwire.State
	detached        bool
	activeCancel    context.CancelFunc
	activeTool      string
	stopRequested   bool
	stopInterrupted bool
	stopDetail      string
	stopOnce        sync.Once
	stateWriteMu    sync.Mutex
	disconnect      *time.Timer
	idleTimer       *time.Timer
	pending         map[string]*workerApprovalFlight
	approvalSeq     atomic.Uint64
	done            chan struct{}
	turns           sync.WaitGroup
	liveMu          sync.Mutex
	liveText        string
	liveThink       string
	liveToolOutput  string
}

type workerApprovalFlight struct {
	done     chan struct{}
	request  workerApproval
	decision tools.GateDecision
	redirect string
	once     sync.Once
}

func runWorkerProcess() error {
	sessionID := strings.TrimSpace(os.Getenv(workerSessionEnv))
	baseDir := strings.TrimSpace(os.Getenv(workerBaseEnv))
	if sessionID == "" || baseDir == "" {
		return errors.New("worker session environment is incomplete")
	}
	if cwd := os.Getenv(workerCWDEnv); cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			return fmt.Errorf("worker chdir: %w", err)
		}
	}
	runtimeFile, err := workerwire.NewRuntime(baseDir, sessionID)
	if err != nil {
		return err
	}
	w, err := newWorkerProcess(runtimeFile)
	if err != nil {
		return err
	}
	server, err := workerwire.NewServer(runtimeFile, w)
	if err != nil {
		w.closeResources()
		return err
	}
	w.server = server
	if old, oldErr := runtimeFile.ReadState(); oldErr == nil && old.State != workerwire.StateIdle && old.State != workerwire.StateInterrupted {
		_ = runtimeFile.WriteState(workerwire.StateRecord{
			State: workerwire.StateInterrupted, PID: old.PID,
			Role:   old.Role,
			Detail: "previous worker exited before clean shutdown",
		})
	}
	w.setState(workerwire.StateIdle, false, "worker ready")
	_ = runtimeFile.RemovePrompt()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx) }()
	select {
	case <-w.done:
	case err := <-serveErr:
		if ctx.Err() != nil {
			w.requestStop(true, "worker interrupted")
		} else {
			detail := "worker socket stopped"
			if err != nil {
				detail += ": " + err.Error()
			}
			w.requestStop(true, detail)
		}
	}
	_ = server.Close()
	server.Wait()
	<-w.done
	w.closeResources()
	return nil
}

func newWorkerProcess(runtimeFile workerwire.Runtime) (*workerProcessState, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.ApplyExecutionOverrides(os.Getenv(workerSandboxEnv), os.Getenv(workerNetworkEnv), os.Getenv(workerApprovalEnv)); err != nil {
		return nil, err
	}
	profiles, err := loadProviderProfiles()
	if err != nil {
		return nil, err
	}
	definitions, err := agent.LoadAgentDefinitions(agent.DefinitionLoadOptions{ProjectTrusted: true})
	if err != nil {
		return nil, err
	}
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	store, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return nil, err
	}
	sessionID := runtimeFile.SessionID
	meta, msgs, err := store.Load(sessionID)
	if err != nil {
		store.Close()
		return nil, err
	}
	modelName := os.Getenv(workerModelEnv)
	providerName := os.Getenv(workerProviderEnv)
	if modelName == "" {
		modelName = meta.Model
	}
	if providerName == "" {
		providerName = meta.Provider
	}
	role := os.Getenv(workerRoleEnv)
	if role == "" {
		if previous, stateErr := runtimeFile.ReadState(); stateErr == nil {
			role = previous.Role
		}
	}
	sysPrompt, err := runtimeFile.ReadPrompt()
	if err != nil {
		sysPrompt = systemPromptForProject(true)
	}
	ag, modelName, providerName, err := newWorkerAgent(cfg, profiles, modelName, providerName, role, sysPrompt)
	if err != nil {
		store.Close()
		return nil, err
	}
	configureWorkerCompaction(ag, cfg, profiles, sysPrompt)
	loaded := msgs
	if len(loaded) > 0 && loaded[0].Role == "system" {
		loaded = loaded[1:]
	}
	ag.Messages = append(ag.Messages, loaded...)
	ag.RebuildTouched(ag.MessagesSnapshot())

	configuredRuntime, runtimeCleanup, err := tools.NewConfiguredRuntime(".", cfg.Execution, false, cfg.PostEdit)
	if err != nil {
		store.Close()
		return nil, err
	}
	artifactsDisabled := cfg.Artifacts != nil && cfg.Artifacts.Enabled != nil && !*cfg.Artifacts.Enabled
	var artifactStore *artifact.Store
	if !artifactsDisabled {
		maxBytes := artifact.DefaultMaxBytes
		if cfg.Artifacts != nil && cfg.Artifacts.MaxBytes > 0 {
			maxBytes = cfg.Artifacts.MaxBytes
		}
		artifactStore, err = artifact.NewWithLimit(filepath.Join(dir, "artifacts"), maxBytes)
		if err != nil {
			runtimeCleanup()
			store.Close()
			return nil, err
		}
	}
	w := &workerProcessState{
		cfg: cfg, profiles: profiles, definitions: definitions, ag: ag, store: store, artifacts: artifactStore, runtime: configuredRuntime,
		runtimeClean: runtimeCleanup,
		runtimeFile:  runtimeFile, sessionID: sessionID, modelName: modelName,
		provider: providerName, role: role, saved: len(ag.Messages), state: workerwire.StateIdle,
		pending: make(map[string]*workerApprovalFlight), done: make(chan struct{}),
	}
	ag.Runtime = configuredRuntime
	ag.ArtifactStore = artifactStore
	ag.ArtifactWriter = artifactStore
	ag.ArtifactCatalog = store
	ag.HistoryCatalog = store
	ag.ArtifactsDisabled = artifactsDisabled
	ag.SetObservationStore(store.ObservationRegistryStore())
	ag.SetSearchStore(store.SearchRegistryStore())
	ag.SetSessionID(sessionID)
	ag.LoadTodosJSON(store.Todos(sessionID))
	if err := ag.BindState(context.Background()); err != nil {
		runtimeCleanup()
		store.Close()
		return nil, err
	}
	if meta.Effort != "" {
		ag.Effort = meta.Effort
	} else if effort := os.Getenv(workerEffortEnv); effort != "" {
		ag.Effort = effort
	} else {
		ag.Effort = cfg.DefaultEffort
		if ag.Effort == "" {
			ag.Effort = "medium"
		}
	}
	if tasks, taskErr := store.LoadTasks(sessionID); taskErr == nil {
		for _, task := range tasks {
			status := agent.TaskStatus(task.Status)
			if status == agent.TaskRunning {
				status = agent.TaskError
				task.Report = "interrupted — worker ended before this subagent finished"
			}
			ag.RestoreTask(agent.BackgroundTask{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: status, Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt, Restored: true})
		}
	}
	ag.Tasks().OnRecord = func(id string, task *agent.BackgroundTask) {
		if id != sessionID {
			return
		}
		_ = store.SaveTask(sessionID, sessionTask(task))
		w.publish("task", workerTask(task), true)
		if task.Status != agent.TaskRunning {
			w.mu.Lock()
			detached := w.detached
			w.mu.Unlock()
			if detached && !w.hasLiveWork() {
				w.scheduleIdleExit()
			}
		}
	}
	configuredRuntime.HumanGate = w.humanGate
	if configuredRuntime.ApprovalMode == tools.ApprovalAutoReview {
		configuredRuntime.Reviewer = ag.ApproveForMe
	}
	if cautious, _ := strconv.ParseBool(os.Getenv(workerCautiousEnv)); cautious {
		tools.Gate = w.legacyGate
	}
	w.lsp = lsp.NewManager(lsp.FromConfigMap(cfg.LSPServers))
	w.lsp.SetRuntime(configuredRuntime)
	if wd, wdErr := os.Getwd(); wdErr == nil {
		disc := mcp.LoadMergedFiltered(wd, mcp.FromConfigMap(cfg.MCPServers), mcp.ImportPolicyFrom(cfg.MCPImport))
		if len(disc.Merged) > 0 || len(disc.Blocked) > 0 || len(disc.Errs) > 0 {
			w.mcp = mcp.NewManager(disc.Merged)
			w.mcp.SetRuntime(configuredRuntime)
			w.mcp.SetBlocked(disc.Blocked)
			w.mcp.SetOnChange(func() {
				ag.SetMCPTools(w.mcp.Tools())
				w.publish("mcp", w.mcp.Statuses(), true)
			})
			w.mcp.Start(context.Background())
			ag.SetMCPTools(w.mcp.Tools())
		}
	}
	return w, nil
}

func newWorkerAgent(cfg *config.Config, profiles provider.Profiles, modelName, providerName, role, sysPrompt string) (*agent.Agent, string, string, error) {
	prov, mdl, apiID, err := cfg.Resolve(modelName, providerName)
	if err != nil {
		return nil, "", "", err
	}
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	if providerName == "" {
		providerName = cfg.DefaultProvider
		if providerName == "" && len(mdl.Providers) > 0 {
			providerName = mdl.Providers[0]
		}
	}
	key, err := prov.ResolveKey()
	if err != nil {
		return nil, "", "", err
	}
	resolved, err := profiles.ResolveModel(provider.Instance{
		Name: providerName, Profile: prov.Profile, BaseURL: prov.BaseURL, Protocol: prov.API,
	}, apiID)
	if err != nil {
		return nil, "", "", err
	}
	backend, err := newProviderBackend(profiles, providerName, prov, key, cfg.MaxRetries, apiID, mdl.API)
	if err != nil {
		return nil, "", "", err
	}
	catalogs := config.LoadCatalogs()
	cat, hasCatalog := catalogs[providerName]
	contextLimit := mdl.ContextWindow()
	if hasCatalog {
		if n := cat.ContextLength(apiID); n > 0 {
			contextLimit = n
		}
	}
	if contextLimit <= 0 {
		contextLimit = config.LoadModelsDev().ContextLength(apiID, workerModelsDevProviderIDs(resolved, providerName)...)
	}
	maxOut := mdl.MaxOut
	if maxOut <= 0 && hasCatalog {
		maxOut = cat.MaxCompletionTokens(apiID)
	}
	if maxOut <= 0 {
		maxOut = contextLimit
	}
	ag := agent.New(backend, apiID, maxOut, sysPrompt)
	ag.ModelName, ag.Provider, ag.Role = modelName, providerName, role
	ag.ContextLimit = contextLimit
	if hasCatalog {
		if info := cat.Find(apiID); info != nil {
			ag.ReasoningToggle = info.ReasoningToggle
		}
	}
	if !ag.ReasoningToggle {
		if info, ok := config.LoadModelsDev().ReasoningFor(apiID, workerModelsDevProviderIDs(resolved, providerName)...); ok {
			ag.ReasoningToggle = info.Toggle
		}
	}
	configureSubagentFactory(ag, cfg, profiles)
	return ag, modelName, providerName, nil
}

func workerModelsDevProviderIDs(resolved provider.Resolved, instanceName string) []string {
	ids := make([]string, 0, 3)
	for _, id := range []string{resolved.Catalog.ModelsDev, resolved.Profile.ID, instanceName} {
		if id == "" {
			continue
		}
		seen := false
		for _, existing := range ids {
			if existing == id {
				seen = true
				break
			}
		}
		if !seen {
			ids = append(ids, id)
		}
	}
	return ids
}

// configureWorkerCompaction keeps the worker's compaction route aligned with
// the interactive role policy. A configured tiny role wins; legacy configs use
// the built-in compact model and otherwise fall back to the active backend.
func configureWorkerCompaction(ag *agent.Agent, cfg *config.Config, profiles provider.Profiles, systemPrompt string) {
	if ag == nil || cfg == nil {
		return
	}
	modelName, providerName := cfg.CompactModel, cfg.CompactProvider
	if modelName == "" && providerName == "" && len(cfg.Roles) > 0 {
		target, err := cfg.ResolveRole(config.RoleTiny)
		if err != nil {
			return
		}
		modelName, providerName = target.Model, target.Provider
	}
	if modelName == "" {
		modelName = config.DefaultCompactModel
	}
	compact, _, _, err := newWorkerAgent(cfg, profiles, modelName, providerName, config.RoleTiny, systemPrompt)
	if err != nil || compact == nil || compact.Backend == nil {
		return
	}
	ag.CompactBackend = compact.Backend
	ag.CompactModel = compact.Model
	ag.CompactProvider = compact.Provider
	ag.CompactProtocol = compact.Protocol
}

func (w *workerProcessState) Snapshot(context.Context) (any, error) {
	w.mu.Lock()
	state, detached, activeTool := w.state, w.detached, w.activeTool
	modelName, providerName, role := w.modelName, w.provider, w.role
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
		return workerSnapshot{SessionID: w.sessionID, State: state, Detached: detached}, nil
	}
	return workerSnapshot{
		SessionID: w.sessionID, State: state, Detached: detached,
		Model: modelID, ModelName: modelName, Provider: providerName,
		Role: role, Protocol: protocol, Effort: effort,
		ContextLimit: contextLimit, ContextTokens: ag.ContextTokens(),
		Usage: ag.Usage(), Messages: boundedWorkerMessages(ag.MessagesSnapshot()),
		Tasks: w.taskStates(), Pending: w.pendingState(), ActiveTool: activeTool,
		LiveText: live.text, LiveThink: live.think, LiveTool: live.tool,
	}, nil
}

func (w *workerProcessState) Command(_ context.Context, command workerwire.Command) (workerwire.CommandResult, error) {
	switch command.Name {
	case workerwire.CommandInput:
		var input workerInput
		if err := json.Unmarshal(command.Payload, &input); err != nil || strings.TrimSpace(input.Input) == "" {
			return workerwire.CommandResult{}, errors.New("worker input is invalid")
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
	case workerwire.CommandPlan:
		var request workerPlanRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil || strings.TrimSpace(request.Goal) == "" {
			return workerwire.CommandResult{}, errors.New("worker plan request is invalid")
		}
		if !w.startPlan(strings.TrimSpace(request.Goal)) {
			return workerwire.CommandResult{}, errors.New("worker is busy or stopping")
		}
		return workerwire.CommandResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
	case workerwire.CommandDetach:
		w.mu.Lock()
		allowed := w.state == workerwire.StateRunning || w.state == workerwire.StateWaitingApproval
		state := w.state
		w.mu.Unlock()
		allowed = allowed || w.hasLiveWork()
		if !allowed {
			return workerwire.CommandResult{}, errors.New("nothing running to detach")
		}
		return workerwire.CommandResult{
			Payload: json.RawMessage(`{"detached":true}`), Detach: true,
			AfterAck: func() { w.setState(state, true, "clientless continuation authorized") },
		}, nil
	case workerwire.CommandStop:
		w.requestStop(false, "stop requested by client")
		return workerwire.CommandResult{Payload: json.RawMessage(`{"stopping":true}`)}, nil
	case workerwire.CommandPing:
		return workerwire.CommandResult{Payload: json.RawMessage(`{"ok":true}`)}, nil
	default:
		return workerwire.CommandResult{}, fmt.Errorf("worker command %q is not implemented", command.Name)
	}
}

func (w *workerProcessState) startCompact() bool {
	return w.startOperation("compaction", func(ctx context.Context) { w.runCompact(ctx) })
}

func (w *workerProcessState) startPlan(goal string) bool {
	return w.startOperation("planning", func(ctx context.Context) { w.runPlan(ctx, goal) })
}

func (w *workerProcessState) startOperation(detail string, run func(context.Context)) bool {
	w.mu.Lock()
	if w.activeCancel != nil || w.stopRequested {
		w.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.activeCancel = cancel
	w.state = workerwire.StateRunning
	detached := w.detached
	w.turns.Add(1)
	w.mu.Unlock()
	w.clearLive()
	w.setState(workerwire.StateRunning, detached, detail+" started")
	go func() {
		defer w.turns.Done()
		run(ctx)
		w.mu.Lock()
		w.activeCancel = nil
		w.activeTool = ""
		detached := w.detached
		stopping := w.stopRequested
		w.mu.Unlock()
		if !stopping {
			w.setState(workerwire.StateIdle, detached, detail+" finished")
		}
		w.clearLive()
	}()
	return true
}

func (w *workerProcessState) runPlan(ctx context.Context, goal string) {
	var plan agent.Plan
	var err error
	if w.cfg == nil {
		err = errors.New("worker configuration is unavailable")
	} else {
		target, targetErr := w.cfg.ResolveRole(config.RoleSmart)
		if targetErr != nil {
			err = targetErr
		} else if target.Model == "" {
			err = errors.New("smart role has no configured model")
		} else {
			systemPrompt := ""
			if messages := w.ag.MessagesSnapshot(); len(messages) > 0 {
				systemPrompt = messages[0].Content
			}
			planner, _, _, buildErr := newWorkerAgent(w.cfg, w.profiles, target.Model, target.Provider, config.RoleSmart, systemPrompt)
			if buildErr != nil {
				err = buildErr
			} else {
				planner.Runtime = w.runtime
				planner.ArtifactWriter = w.artifacts
				planner.ArtifactCatalog = w.store
				planner.ArtifactStore = w.artifacts
				planner.HistoryCatalog = w.store
				planner.SetSessionID(w.sessionID)
				definition := agent.BuiltInPlannerDefinition()
				if loaded, ok := w.definitions[definition.Name]; ok {
					definition = loaded
				}
				events := agent.Events{
					OnUsage: func(usage llm.Usage) {
						w.ag.AddUsage(usage)
						w.publish("usage", usage, true)
					},
					OnRetry: func(retry llm.RetryEvent) { w.publish("retry", retry, true) },
				}
				plan, err = agent.ProposePlanWithDefinition(ctx, planner, goal, definition, events)
			}
		}
	}
	w.persist()
	result := workerPlanResult{Plan: plan}
	if err != nil {
		result.Error = err.Error()
	}
	w.publish("plan_done", result, true)
}

func (w *workerProcessState) runCompact(ctx context.Context) {
	before := len(w.ag.MessagesSnapshot())
	var summary string
	var cutoff int
	err := w.ag.ManualCompact(ctx, agent.Events{
		OnCompactionReady: func(messages []llm.Message, summary string, cutoff int) error {
			w.mu.Lock()
			saved, modelName, providerName := w.saved, w.modelName, w.provider
			w.mu.Unlock()
			if len(messages) > saved {
				if err := w.store.Save(w.sessionID, saved, messages, modelName, providerName); err != nil {
					return err
				}
			}
			return w.store.RecordCompaction(w.sessionID, cutoff, summary)
		},
		OnCompacted: func(value string, at int) { summary, cutoff = value, at },
		OnUsage:     func(usage llm.Usage) { w.publish("usage", usage, true) },
		OnRetry:     func(retry llm.RetryEvent) { w.publish("retry", retry, true) },
	})
	if err == nil {
		kept := len(w.ag.MessagesSnapshot())
		w.mu.Lock()
		w.saved = kept
		w.mu.Unlock()
		w.publish("compact", map[string]any{
			"summary": summary, "cutoff": cutoff,
			"took": before - kept, "kept": kept,
		}, true)
	}
	w.persist()
	result := workerCompactResult{Usage: w.ag.Usage(), Messages: boundedWorkerMessages(w.ag.MessagesSnapshot())}
	if err != nil {
		result.Error = err.Error()
	}
	w.publish("compact_done", result, true)
}

// configure changes only the idle worker's route. Keeping the existing Agent
// preserves its task registry, observations, history, and session resources;
// the replacement agent is used only as a route builder.
func (w *workerProcessState) configure(request workerConfigureRequest) error {
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
	candidate, resolvedModel, resolvedProvider, err := newWorkerAgent(w.cfg, w.profiles, modelName, providerName, role, systemPrompt)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	w.ag.Backend = candidate.Backend
	w.ag.Model = candidate.Model
	w.ag.ModelName = resolvedModel
	w.ag.Provider = resolvedProvider
	w.ag.Protocol = candidate.Protocol
	w.ag.MaxTokens = candidate.MaxTokens
	w.ag.ContextLimit = candidate.ContextLimit
	w.ag.ReasoningToggle = candidate.ReasoningToggle
	w.ag.SubagentFactory = candidate.SubagentFactory
	w.ag.Role = role
	if request.UpdateEffort {
		w.ag.Effort = request.Effort
	}
	w.modelName, w.provider, w.role = resolvedModel, resolvedProvider, role
	configureWorkerCompaction(w.ag, w.cfg, w.profiles, systemPrompt)
	effort := w.ag.Effort
	protocol := w.ag.Protocol
	modelID := w.ag.Model
	state, detached := w.state, w.detached
	w.mu.Unlock()
	if w.store != nil {
		_ = w.store.SetRoute(w.sessionID, resolvedModel, resolvedProvider)
		if request.UpdateEffort {
			_ = w.store.SetEffort(w.sessionID, effort)
		}
	}
	w.publish("route", workerConfigureRequest{
		Model: modelID, ModelName: resolvedModel, Provider: resolvedProvider,
		Role: role, Protocol: protocol, Effort: effort, UpdateEffort: true,
	}, true)
	w.setState(state, detached, "route changed")
	return nil
}

func (w *workerProcessState) Attached(context.Context) {
	w.mu.Lock()
	if w.disconnect != nil {
		w.disconnect.Stop()
		w.disconnect = nil
	}
	if w.idleTimer != nil {
		w.idleTimer.Stop()
		w.idleTimer = nil
	}
	w.detached = false
	state := w.state
	w.mu.Unlock()
	w.setState(state, false, "controller attached")
}

func (w *workerProcessState) Disconnected(_ context.Context, detached bool) {
	w.mu.Lock()
	if detached || w.detached {
		w.mu.Unlock()
		return
	}
	if w.disconnect != nil {
		w.disconnect.Stop()
	}
	w.disconnect = time.AfterFunc(2*time.Second, func() {
		if w.server != nil && w.server.ControllerPresent() {
			return
		}
		w.requestStop(true, "client disconnected")
	})
	w.mu.Unlock()
}

func (w *workerProcessState) startTurn(input workerInput) bool {
	w.mu.Lock()
	if w.activeCancel != nil || w.stopRequested {
		w.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.activeCancel = cancel
	w.state = workerwire.StateRunning
	detached := w.detached
	w.turns.Add(1)
	w.mu.Unlock()
	w.clearLive()
	w.setState(workerwire.StateRunning, detached, "turn started")
	go w.runTurn(ctx, input)
	return true
}

func (w *workerProcessState) runTurn(ctx context.Context, input workerInput) {
	defer w.turns.Done()
	if input.SystemPrompt != "" {
		w.ag.SetSystemPrompt(input.SystemPrompt)
	}
	var turnUsage llm.Usage
	addUsage := func(u llm.Usage) {
		turnUsage.PromptTokens += u.PromptTokens
		turnUsage.CompletionTokens += u.CompletionTokens
		turnUsage.CacheCreationTokens += u.CacheCreationTokens
		if cached := u.Cached(); cached > 0 {
			if turnUsage.PromptTokensDetails == nil {
				turnUsage.PromptTokensDetails = &struct {
					CachedTokens int `json:"cached_tokens"`
				}{}
			}
			turnUsage.PromptTokensDetails.CachedTokens += cached
		}
	}
	ev := agent.Events{
		OnText: func(s string) {
			w.appendLive("text", s)
			w.publish("text", s, false)
		},
		OnThink: func(s string) {
			w.appendLive("think", s)
			w.publish("think", s, false)
		},
		OnToolStart: func(id, name, args string) {
			w.mu.Lock()
			w.activeTool = name
			w.mu.Unlock()
			w.publish("tool_start", map[string]string{"id": id, "name": name, "args": args}, true)
		},
		OnToolOutput: func(id, output string) {
			w.appendLive("tool_output", output)
			w.publish("tool_output", map[string]string{"id": id, "output": output}, false)
		},
		OnToolEnd: func(id, name, result string) {
			w.mu.Lock()
			w.activeTool = ""
			w.mu.Unlock()
			w.publish("tool_end", map[string]string{"id": id, "name": name, "result": result}, true)
		},
		OnSteer: func(s string) { w.publish("steer", s, true) },
		OnUsage: func(u llm.Usage) { addUsage(u); w.publish("usage", u, true) },
		OnRetry: func(ev llm.RetryEvent) { w.publish("retry", ev, true) },
		OnGoalUpdate: func(update agent.GoalUpdate) {
			w.persistGoalUpdate(update)
			w.publish("goal_update", update, true)
		},
		OnCompactionReady: func(messages []llm.Message, summary string, cutoff int) error {
			w.mu.Lock()
			saved, modelName, providerName := w.saved, w.modelName, w.provider
			w.mu.Unlock()
			if len(messages) > saved {
				if err := w.store.Save(w.sessionID, saved, messages, modelName, providerName); err != nil {
					return err
				}
			}
			return w.store.RecordCompaction(w.sessionID, cutoff, summary)
		},
		OnCompacted: func(summary string, cutoff int) {
			w.mu.Lock()
			w.saved = len(w.ag.MessagesSnapshot())
			w.mu.Unlock()
			w.publish("compact", map[string]any{"summary": summary, "cutoff": cutoff}, true)
		},
	}
	var final string
	var err error
	switch {
	case len(input.Parts) > 0 && input.Goal != nil:
		final, err = w.ag.TurnWithImagesAndGoal(ctx, input.Input, input.Parts, *input.Goal, ev)
	case len(input.Parts) > 0:
		final, err = w.ag.TurnWithImages(ctx, input.Input, input.Parts, ev)
	case input.Goal != nil:
		final, err = w.ag.TurnWithGoal(ctx, input.Input, *input.Goal, ev)
	case input.Authored:
		final, err = w.ag.TurnAuthored(ctx, input.Input, ev)
	default:
		final, err = w.ag.Turn(ctx, input.Input, ev)
	}
	w.persist()
	w.persistGoalTurn(input.Goal, turnUsage, err)
	w.mu.Lock()
	w.activeCancel = nil
	w.activeTool = ""
	detached := w.detached
	if w.stopRequested {
		w.mu.Unlock()
	} else {
		w.state = workerwire.StateIdle
		w.mu.Unlock()
		w.setState(workerwire.StateIdle, detached, "turn finished")
	}
	result := workerTurnResult{
		Final: final, Usage: turnUsage, At: input.At, Snap: input.Snap,
		Messages: boundedWorkerMessages(w.ag.MessagesSnapshot()),
	}
	if err != nil {
		result.Error = err.Error()
	}
	w.publish("turn_done", result, true)
	w.clearLive()
	if detached && !w.hasLiveWork() {
		w.scheduleIdleExit()
	}
}

func (w *workerProcessState) persist() {
	if w.store == nil || w.ag == nil {
		return
	}
	msgs := w.ag.MessagesSnapshot()
	w.mu.Lock()
	saved, modelName, providerName, effort := w.saved, w.modelName, w.provider, w.ag.Effort
	w.mu.Unlock()
	_ = w.store.SetEffort(w.sessionID, effort)
	_ = w.store.SetTodos(w.sessionID, w.ag.TodosJSON())
	usage := w.ag.Usage()
	_ = w.store.SetUsage(w.sessionID, usage.PromptTokens, usage.Cached(), usage.CompletionTokens)
	if len(msgs) <= saved {
		return
	}
	if err := w.store.Save(w.sessionID, saved, msgs, modelName, providerName); err == nil {
		w.mu.Lock()
		w.saved = len(msgs)
		w.mu.Unlock()
	}
}

func (w *workerProcessState) persistGoalUpdate(update agent.GoalUpdate) {
	if w.store == nil || w.sessionID == "" {
		return
	}
	record, ok, err := w.store.LoadGoal(w.sessionID)
	if err != nil || !ok || record.Status != goalstate.StatusActive {
		return
	}
	if err := update.Validate(record.ID); err != nil {
		return
	}
	record.Status = update.Status
	record.Progress = truncateWorkerGoalNote(update.Progress)
	record.Blocker = truncateWorkerGoalNote(update.Blocker)
	record.UpdatedAt = time.Now().UTC()
	_ = w.store.CheckpointGoal(w.sessionID, record)
}

func (w *workerProcessState) persistGoalTurn(input *goalstate.Record, usage llm.Usage, turnErr error) {
	if w.store == nil || w.sessionID == "" || input == nil {
		return
	}
	record, ok, err := w.store.LoadGoal(w.sessionID)
	if err != nil || !ok || record.ID != input.ID {
		return
	}
	// An explicit clear/pause made while the turn was running wins over the
	// stale request-scoped goal supplied at turn start.
	if record.Status != goalstate.StatusActive && record.Status != goalstate.StatusBlocked && record.Status != goalstate.StatusComplete {
		return
	}
	record.PromptTokens += usage.PromptTokens
	record.CachedTokens += usage.Cached()
	record.CompletionTokens += usage.CompletionTokens
	record.Rounds++
	if turnErr != nil && record.Status == goalstate.StatusActive {
		record.Status = goalstate.StatusPaused
		record.Blocker = truncateWorkerGoalNote(turnErr.Error())
	}
	if record.Status == goalstate.StatusActive && record.Rounds >= w.goalMaxRounds() {
		record.Status = goalstate.StatusBudgetLimited
		record.Blocker = fmt.Sprintf("goal round circuit breaker reached (%d rounds)", record.Rounds)
	}
	record.UpdatedAt = time.Now().UTC()
	if record.Status == goalstate.StatusActive {
		_ = w.store.SaveGoal(w.sessionID, record)
	} else {
		_ = w.store.CheckpointGoal(w.sessionID, record)
	}
}

func (w *workerProcessState) goalMaxRounds() int {
	if wd, err := os.Getwd(); err == nil {
		if n := config.ProjectGoalMaxRounds(wd); n > 0 {
			return n
		}
	}
	if w.cfg != nil && w.cfg.GoalMaxRounds > 0 {
		return w.cfg.GoalMaxRounds
	}
	return config.DefaultGoalMaxRounds
}

func truncateWorkerGoalNote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= goalstate.MaxNoteBytes {
		return value
	}
	return value[:goalstate.MaxNoteBytes]
}

func (w *workerProcessState) publish(kind string, data any, important bool) {
	if w.server != nil {
		_, _ = w.server.Publish(kind, data, important)
	}
}

func (w *workerProcessState) setState(state workerwire.State, detached bool, detail string) {
	w.mu.Lock()
	w.state, w.detached = state, detached
	role := w.role
	w.mu.Unlock()
	w.stateWriteMu.Lock()
	defer w.stateWriteMu.Unlock()
	_ = w.runtimeFile.WriteState(workerwire.StateRecord{SessionID: w.sessionID, State: state, Detached: detached, Role: role, PID: os.Getpid(), Detail: detail})
	w.publish("state", map[string]any{"state": state, "detached": detached}, true)
}

func (w *workerProcessState) requestStop(interrupted bool, detail string) {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopRequested = true
		w.stopInterrupted = interrupted
		w.stopDetail = detail
		cancel := w.activeCancel
		detached := w.detached
		if w.disconnect != nil {
			w.disconnect.Stop()
			w.disconnect = nil
		}
		if w.idleTimer != nil {
			w.idleTimer.Stop()
			w.idleTimer = nil
		}
		w.state = workerwire.StateStopping
		w.mu.Unlock()
		w.setState(workerwire.StateStopping, detached, detail)
		if cancel != nil {
			cancel()
		}
		if w.ag != nil {
			for _, task := range w.ag.Tasks().List() {
				if task.Status == agent.TaskRunning {
					w.ag.Tasks().Cancel(task.ID)
				}
			}
		}
		w.rejectApprovals("worker stopped")
		go func() {
			w.turns.Wait()
			w.waitTasks()
			w.mu.Lock()
			interrupted := w.stopInterrupted
			detail := w.stopDetail
			w.mu.Unlock()
			state := workerwire.StateIdle
			if interrupted {
				state = workerwire.StateInterrupted
			}
			w.setState(state, false, detail)
			close(w.done)
		}()
	})
}

func (w *workerProcessState) waitTasks() {
	for _, task := range w.ag.Tasks().List() {
		if task.Status == agent.TaskRunning && task.Done != nil {
			<-task.Done
		}
	}
}

func (w *workerProcessState) scheduleIdleExit() {
	w.mu.Lock()
	if w.idleTimer != nil || !w.detached || w.stopRequested {
		w.mu.Unlock()
		return
	}
	w.idleTimer = time.AfterFunc(30*time.Second, func() {
		w.mu.Lock()
		w.idleTimer = nil
		detached := w.detached
		stopping := w.stopRequested
		w.mu.Unlock()
		if !detached || stopping {
			return
		}
		if w.server != nil && w.server.ControllerPresent() || w.hasLiveWork() {
			return
		}
		w.requestStop(false, "detached worker idle grace elapsed")
	})
	w.mu.Unlock()
}

func (w *workerProcessState) hasLiveWork() bool {
	if w.ag == nil {
		return false
	}
	w.mu.Lock()
	active := w.activeCancel != nil
	w.mu.Unlock()
	if active {
		return true
	}
	for _, task := range w.ag.Tasks().List() {
		if task.Status == agent.TaskRunning {
			return true
		}
	}
	return false
}

func (w *workerProcessState) humanGate(req tools.GateRequest) (tools.GateDecision, string) {
	id := fmt.Sprintf("approval-%d", w.approvalSeq.Add(1))
	pending := &workerApproval{ID: id, Tool: req.Tool, Command: req.Command, Rule: req.Rule}
	flight := &workerApprovalFlight{done: make(chan struct{}), request: *pending}
	w.mu.Lock()
	w.pending[id] = flight
	w.state = workerwire.StateWaitingApproval
	detached := w.detached
	w.mu.Unlock()
	w.setState(workerwire.StateWaitingApproval, detached, "approval requested")
	w.publish("permission_request", workerPermissionRequest{Approval: *pending}, true)
	<-flight.done
	w.mu.Lock()
	delete(w.pending, id)
	if !w.stopRequested {
		w.state = workerwire.StateRunning
	}
	decision, redirect := flight.decision, flight.redirect
	detached = w.detached
	w.mu.Unlock()
	if !w.stopRequested {
		w.setState(workerwire.StateRunning, detached, "approval answered")
	}
	return decision, redirect
}

func (w *workerProcessState) legacyGate(req tools.GateRequest) (tools.GateDecision, string) {
	return w.humanGate(req)
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

func (w *workerProcessState) pendingState() *workerApproval {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, request := range w.pending {
		return &request.request
	}
	return nil
}

const workerLiveTailBytes = 128 << 10

type workerLiveSnapshot struct {
	text, think, tool string
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
	}
	w.liveMu.Unlock()
}

func (w *workerProcessState) liveSnapshot() workerLiveSnapshot {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	return workerLiveSnapshot{text: w.liveText, think: w.liveThink, tool: w.liveToolOutput}
}

func (w *workerProcessState) clearLive() {
	w.liveMu.Lock()
	w.liveText, w.liveThink, w.liveToolOutput = "", "", ""
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
		out = append(out, workerTaskState{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: string(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt})
	}
	return out
}

func (w *workerProcessState) closeResources() {
	if w.mcp != nil {
		w.mcp.Close()
	}
	if w.lsp != nil {
		w.lsp.Close()
	}
	if w.runtime != nil {
		if w.runtimeClean != nil {
			w.runtimeClean()
			w.runtimeClean = nil
		}
	}
	tools.Gate = nil
	if w.store != nil {
		_ = w.store.Close()
	}
}

func workerTask(task *agent.BackgroundTask) workerTaskState {
	return workerTaskState{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: string(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt}
}

func sessionTask(task *agent.BackgroundTask) session.Task {
	return session.Task{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: string(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt}
}

func boundedWorkerMessages(messages []llm.Message) []llm.Message {
	const maxBytes = 512 << 10
	if len(messages) == 0 {
		return nil
	}
	start := len(messages)
	bytes := 0
	for start > 0 {
		data, err := json.Marshal(messages[start-1])
		if err != nil || bytes+len(data) > maxBytes {
			break
		}
		bytes += len(data)
		start--
	}
	if start == len(messages) {
		return nil
	}
	if start > 0 && messages[0].Role == "system" {
		return append([]llm.Message{messages[0]}, messages[start:]...)
	}
	return messages[start:]
}
