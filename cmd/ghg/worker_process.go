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
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/schedule"
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
	workerSnapshot          = workerwire.Snapshot
	workerPermissionRequest = workerwire.PermissionRequest
)

type workerProcessState struct {
	mu              sync.Mutex
	cfg             *config.Config
	profiles        models.Profiles
	server          *workerwire.Server
	ag              *agent.Agent
	store           *session.Store
	runtime         *tools.ToolRuntime
	runtimeClean    func()
	lsp             *lsp.Manager
	mcp             *mcp.Manager
	runtimeFile     workerwire.Runtime
	sessionID       string
	modelName       string
	provider        string
	role            string
	mode            string
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
	livePlan        string
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
	go w.scheduleLoop(ctx)
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
	mode := os.Getenv(workerModeEnv)
	if mode == "" {
		if previous, stateErr := runtimeFile.ReadState(); stateErr == nil && previous.Mode != "" {
			mode = previous.Mode
		} else {
			mode = "execute"
		}
	}
	sysPrompt, err := runtimeFile.ReadPrompt()
	if err != nil {
		sysPrompt = systemPromptForProject(true)
	}
	ag, modelName, providerName, err := agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: modelName, Provider: providerName,
		Role: role, SystemPrompt: sysPrompt,
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	ag.PlanMode = (mode == "plan")
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
	outputConfig := cfg.Outputs
	if outputConfig == nil {
		outputConfig = cfg.Artifacts
	}
	outputsDisabled := outputConfig != nil && outputConfig.Enabled != nil && !*outputConfig.Enabled
	var outputStore *session.OutputStore
	if !outputsDisabled {
		maxBytes := session.DefaultMaxBytes
		if outputConfig != nil && outputConfig.MaxBytes > 0 {
			maxBytes = outputConfig.MaxBytes
		}
		outputStore, err = session.NewOutputStoreWithLimit(filepath.Join(dir, "outputs"), maxBytes)
		if err != nil {
			runtimeCleanup()
			store.Close()
			return nil, err
		}
	}
	w := &workerProcessState{
		cfg: cfg, profiles: profiles, ag: ag, store: store, runtime: configuredRuntime,
		runtimeClean: runtimeCleanup,
		runtimeFile:  runtimeFile, sessionID: sessionID, modelName: modelName,
		provider: providerName, role: role, mode: mode, saved: len(ag.Messages), state: workerwire.StateIdle,
		pending: make(map[string]*workerApprovalFlight), done: make(chan struct{}),
	}
	store.Outputs = outputStore
	ag.Runtime = configuredRuntime
	ag.Outputs = outputStore
	ag.OutputCatalog = store
	ag.HistoryCatalog = store
	ag.SubagentsDisabled = !config.SubagentsEnabled(cfg)
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
		ag.Effort = defaultEffort(cfg)
	}
	var usage models.Usage
	usage.PromptTokens, usage.CompletionTokens = meta.UsageIn, meta.UsageOut
	usage.AddCached(meta.UsageCached)
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.Cached() == 0 {
		for _, message := range msgs {
			if message.Usage == nil {
				continue
			}
			usage.PromptTokens += message.Usage.PromptTokens
			usage.CompletionTokens += message.Usage.CompletionTokens
			usage.AddCached(message.Usage.Cached())
		}
	}
	ag.SetUsage(usage)
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
		if id != sessionID || task == nil {
			return
		}
		_ = store.SaveTask(sessionID, sessionTask(*task))
		w.publish("task", workerTask(*task), true)
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
		configuredRuntime.Cautious = true
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
				w.publish("mcp", w.mcpStatuses(), true)
			})
			w.mcp.Start(context.Background())
			ag.SetMCPTools(w.mcp.Tools())
		}
	}
	return w, nil
}

// configureWorkerCompaction keeps the worker's compaction route aligned with
// the interactive role policy. A configured tiny role wins; legacy configs use
// the built-in compact model and otherwise fall back to the active backend.
func configureWorkerCompaction(ag *agent.Agent, cfg *config.Config, profiles models.Profiles, systemPrompt string) {
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
	compact, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: modelName, Provider: providerName,
		Role: config.RoleTiny, SystemPrompt: systemPrompt,
	})
	if err != nil || compact == nil || compact.Backend == nil {
		return
	}
	ag.CompactBackend = compact.Backend
	ag.CompactModel = compact.Model
	ag.CompactProvider = compact.Provider
	ag.CompactProtocol = compact.Protocol
}

func (w *workerProcessState) transition(mutate func() (newState workerwire.State, newDetached bool, detail string, ok bool)) bool {
	w.stateWriteMu.Lock()
	defer w.stateWriteMu.Unlock()

	w.mu.Lock()
	state, detached, detail, ok := mutate()
	if !ok {
		w.mu.Unlock()
		return false
	}
	w.state = state
	w.detached = detached
	role := w.role
	mode := w.mode
	sessionID := w.sessionID
	w.mu.Unlock()

	_ = w.runtimeFile.WriteState(workerwire.StateRecord{
		SessionID: sessionID,
		State:     state,
		Detached:  detached,
		Role:      role,
		Mode:      mode,
		PID:       os.Getpid(),
		Detail:    detail,
	})
	w.publish("state", map[string]any{"state": state, "detached": detached, "mode": mode}, true)
	return true
}

func (w *workerProcessState) setState(state workerwire.State, detached bool, detail string) {
	w.transition(func() (workerwire.State, bool, string, bool) {
		return state, detached, detail, true
	})
}

func (w *workerProcessState) requestStop(interrupted bool, detail string) {
	w.stopOnce.Do(func() {
		var cancel context.CancelFunc
		w.transition(func() (workerwire.State, bool, string, bool) {
			w.stopRequested = true
			w.stopInterrupted = interrupted
			w.stopDetail = detail
			cancel = w.activeCancel
			if w.disconnect != nil {
				w.disconnect.Stop()
				w.disconnect = nil
			}
			if w.idleTimer != nil {
				w.idleTimer.Stop()
				w.idleTimer = nil
			}
			return workerwire.StateStopping, w.detached, detail, true
		})
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
		if w.server != nil && w.server.ControllerPresent() || w.hasLiveWork() || w.hasActiveSchedules() {
			return
		}
		w.requestStop(false, "detached worker idle grace elapsed")
	})
	w.mu.Unlock()
}

func (w *workerProcessState) scheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case now := <-ticker.C:
			w.fireDueSchedule(now)
		}
	}
}

func (w *workerProcessState) fireDueSchedule(now time.Time) {
	if w.store == nil || w.sessionID == "" || w.hasLiveWork() {
		return
	}
	task, ok := schedule.NextDue(w.store.Schedules(w.sessionID), now)
	if !ok {
		return
	}
	prompt := fmt.Sprintf("⏰ Scheduled task #%d fired (%s). Work on it now:\n\n%s", task.ID, task.Schedule, task.Prompt)
	if !w.startTurn(workerInput{Input: prompt}) {
		return
	}
	if err := w.store.MarkFired(w.sessionID, task.ID, task.Slot); err != nil {
		w.publish("schedule", fmt.Sprintf("scheduled task #%d started, but could not record its fire: %v", task.ID, err), true)
		return
	}
	w.publish("schedule", fmt.Sprintf("⏰ scheduled task #%d fired — %s", task.ID, task.Prompt), true)
}

func (w *workerProcessState) hasActiveSchedules() bool {
	if w.store == nil || w.sessionID == "" {
		return false
	}
	for _, task := range w.store.Schedules(w.sessionID) {
		parsed, err := schedule.Parse(task.Schedule)
		if err != nil {
			continue
		}
		if parsed.Every > 0 || task.LastFire.IsZero() {
			return true
		}
	}
	return false
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
	if w.store != nil {
		_ = w.store.Close()
	}
}

func workerTask(task agent.BackgroundTask) workerTaskState {
	return workerTaskState{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: string(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt, Restored: task.Restored}
}

func sessionTask(task agent.BackgroundTask) session.Task {
	return session.Task{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: string(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt}
}

func boundedWorkerMessages(messages []models.Message) []models.Message {
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
		return append([]models.Message{messages[0]}, messages[start:]...)
	}
	return messages[start:]
}
