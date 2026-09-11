package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

type bridgeRequest struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type bridge struct {
	client    *workerwire.Client
	process   *workerwire.Process
	cfg       *config.Config
	profiles  models.Profiles
	role      string
	turnDone  bool
	detached  atomic.Bool
	outMu     sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]string
}

func bridgeCLI(args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "session id to resume")
	roleFlag := fs.String("role", config.RoleFast, "initial model role")
	modeFlag := fs.String("mode", "execute", "initial mode: execute or plan")
	sandboxFlag := fs.String("sandbox", "", "execution sandbox override")
	networkFlag := fs.String("network", "", "execution network override")
	approvalFlag := fs.String("approval", "", "execution approval override")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !config.IsRole(*roleFlag) {
		return fmt.Errorf("unknown role %q", *roleFlag)
	}
	if *modeFlag != "execute" && *modeFlag != "plan" {
		return fmt.Errorf("unknown bridge mode %q", *modeFlag)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	validationCfg := *cfg
	if err := validationCfg.ApplyExecutionOverrides(*sandboxFlag, *networkFlag, *approvalFlag); err != nil {
		return err
	}
	profiles, err := loadProviderProfiles()
	if err != nil {
		return err
	}
	sessionID := strings.TrimSpace(*sessionFlag)
	modelName, providerName := "", ""
	if sessionID != "" {
		if dir, dirErr := config.Dir(); dirErr == nil {
			if st, openErr := session.Open(filepath.Join(dir, "sessions.db")); openErr == nil {
				if meta, _, loadErr := st.Load(sessionID); loadErr == nil {
					modelName, providerName = meta.Model, meta.Provider
				}
				_ = st.Close()
			}
		}
	} else {
		sessionID = session.NewSessionID()
	}

	sysPrompt := systemPrompt()
	if modelName == "" || providerName == "" {
		_, modelName, providerName, err = agent.NewConfiguredForRole(cfg, profiles, *roleFlag, sysPrompt, false)
		if err != nil {
			return err
		}
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	runtimeFile, err := workerwire.NewRuntime(dir, sessionID)
	if err != nil {
		return err
	}

	var client *workerwire.Client
	var process *workerwire.Process
	if runtimeFile.Live() {
		client, err = bridgeConnect(runtimeFile)
	} else {
		if err = runtimeFile.WritePrompt(sysPrompt); err != nil {
			return err
		}
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return cwdErr
		}
		env := map[string]string{
			"GHG_INTERNAL_WORKER": "1",
			workerSessionEnv:      sessionID,
			workerBaseEnv:         dir,
			workerCWDEnv:          cwd,
			workerModelEnv:        modelName,
			workerProviderEnv:     providerName,
			workerRoleEnv:         *roleFlag,
			workerModeEnv:         *modeFlag,
			workerCautiousEnv:     "false",
		}
		if *sandboxFlag != "" {
			env[workerSandboxEnv] = *sandboxFlag
		}
		if *networkFlag != "" {
			env[workerNetworkEnv] = *networkFlag
		}
		if *approvalFlag != "" {
			env[workerApprovalEnv] = *approvalFlag
		}
		process, err = workerwire.Launch(context.Background(), os.Args[0], env)
		if err == nil {
			client, err = bridgeConnect(runtimeFile)
		}
		if err != nil {
			if process != nil {
				_ = process.Stop()
				_ = process.Wait()
			}
			return err
		}
	}

	b := &bridge{client: client, process: process, cfg: cfg, profiles: profiles, role: *roleFlag, pending: make(map[string]string)}
	defer b.close()
	b.emit(map[string]any{"type": "bridge_ready", "session_id": sessionID})
	ctx, stop := signalContext()
	defer stop()
	go b.forward(ctx)
	return b.readInput(ctx)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	// The bridge is a short-lived controller; the worker owns the durable state.
	// A signal only ends this controller; close() stops the worker cleanly.
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

func bridgeConnect(runtimeFile workerwire.Runtime) (*workerwire.Client, error) {
	var client *workerwire.Client
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		client, err = workerwire.Dial(ctx, runtimeFile)
		cancel()
		if err == nil {
			return client, nil
		}
		if strings.Contains(err.Error(), "worker already has a controlling client") {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

func (b *bridge) close() {
	if b.client != nil {
		if !b.detached.Load() {
			_ = b.client.Send(workerwire.CommandStop, "bridge-close", nil)
		}
		_ = b.client.Close()
	}
	if b.process != nil {
		if !waitProcessForBridge(b.process, 2*time.Second) {
			_ = b.process.Stop()
			_ = b.process.Wait()
		}
	}
}

func waitProcessForBridge(process *workerwire.Process, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_ = process.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (b *bridge) emit(value any) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func (b *bridge) readInput(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), workerwire.MaxFrameBytes)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		var request bridgeRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			b.emit(map[string]any{"type": "error", "error": "invalid bridge request: " + err.Error()})
			continue
		}
		if err := b.handle(request); err != nil {
			b.emit(map[string]any{"type": "error", "request_id": request.RequestID, "error": err.Error()})
		}
	}
	return scanner.Err()
}

func (b *bridge) handle(request bridgeRequest) error {
	if request.Type != "command" {
		return fmt.Errorf("unknown bridge message %q", request.Type)
	}
	requestID := request.RequestID
	if requestID == "" {
		requestID = fmt.Sprintf("bridge-%d", time.Now().UnixNano())
	}
	if request.Name == "set_role_model" {
		var payload struct {
			Role     string `json:"role"`
			Model    string `json:"model"`
			Provider string `json:"provider"`
			Mode     string `json:"mode,omitempty"`
		}
		if err := json.Unmarshal(request.Payload, &payload); err != nil || !config.IsRole(payload.Role) || strings.TrimSpace(payload.Model) == "" {
			return errors.New("role model configuration is invalid")
		}
		payload.Model = strings.TrimSpace(payload.Model)
		payload.Provider = strings.TrimSpace(payload.Provider)
		if _, _, _, err := agent.NewConfigured(agent.BuildOptions{
			Config: b.cfg, Profiles: b.profiles, Model: payload.Model, Provider: payload.Provider,
			Role: payload.Role, SystemPrompt: systemPrompt(),
		}); err != nil {
			return err
		}
		if b.cfg.Roles == nil {
			b.cfg.Roles = make(map[string]config.RoleConfig)
		}
		b.cfg.Roles[payload.Role] = config.RoleConfig{Model: payload.Model, Provider: payload.Provider}
		if err := b.cfg.Save(); err != nil {
			return err
		}
		if payload.Mode != "plan" {
			payload.Mode = "execute"
		}
		active, err := b.cfg.ResolveRole(payload.Role)
		if err != nil {
			return err
		}
		request.Name = workerwire.CommandConfigure
		request.Payload, err = json.Marshal(workerwire.ConfigureRequest{
			Model: active.Model, Provider: active.Provider, Role: payload.Role, Mode: payload.Mode,
		})
		if err != nil {
			return err
		}
		b.role = payload.Role
	}
	if request.Name == "configure_role" {
		var payload struct {
			Role             string `json:"role"`
			Mode             string `json:"mode,omitempty"`
			Effort           string `json:"effort,omitempty"`
			UpdateEffort     bool   `json:"update_effort,omitempty"`
			DynamicReasoning *bool  `json:"dynamic_reasoning,omitempty"`
		}
		if err := json.Unmarshal(request.Payload, &payload); err != nil || !config.IsRole(payload.Role) {
			return errors.New("bridge role configuration is invalid")
		}
		if payload.DynamicReasoning != nil {
			b.cfg.DynamicReasoning = payload.DynamicReasoning
			if err := b.cfg.Save(); err != nil {
				return err
			}
		}
		_, modelName, providerName, err := agent.NewConfiguredForRole(b.cfg, b.profiles, payload.Role, systemPrompt(), false)
		if err != nil {
			return err
		}
		request.Name = workerwire.CommandConfigure
		request.Payload, err = json.Marshal(workerwire.ConfigureRequest{
			Model: modelName, Provider: providerName, Role: payload.Role,
			Effort: strings.TrimSpace(payload.Effort), UpdateEffort: payload.UpdateEffort || payload.Effort != "", DynamicReasoning: payload.DynamicReasoning, Mode: payload.Mode,
		})
		if err != nil {
			return err
		}
		b.role = payload.Role
	}
	if request.Name == "stop" {
		request.Name = workerwire.CommandStop
	}
	if !workerCommandName(request.Name) {
		return fmt.Errorf("unsupported bridge command %q", request.Name)
	}
	b.pendingMu.Lock()
	b.pending[requestID] = request.Name
	b.pendingMu.Unlock()
	if err := b.client.Send(request.Name, requestID, request.Payload); err != nil {
		b.pendingMu.Lock()
		delete(b.pending, requestID)
		b.pendingMu.Unlock()
		return err
	}
	return nil
}

func workerCommandName(name string) bool {
	return name == workerwire.CommandDetach || name == workerwire.CommandCancel || name == workerwire.CommandInput ||
		name == workerwire.CommandApprove || name == workerwire.CommandAnswerQuestion || name == workerwire.CommandConfigure ||
		name == workerwire.CommandCompact || name == workerwire.CommandStop || name == workerwire.CommandPing ||
		name == workerwire.CommandLSPStatus || name == workerwire.CommandMCPStatus || name == workerwire.CommandMCPReconnect ||
		name == workerwire.CommandMCPEnable || name == workerwire.CommandMCPDisable || name == workerwire.CommandContextDoctor ||
		name == workerwire.CommandRewind || name == workerwire.CommandCompactRetry || name == workerwire.CommandGoal ||
		name == workerwire.CommandGoalFromContext || name == workerwire.CommandChdir || name == workerwire.CommandAppend ||
		name == workerwire.CommandShell || name == workerwire.CommandFork || name == workerwire.CommandRename ||
		name == workerwire.CommandNotify
}

func (b *bridge) forward(ctx context.Context) {
	frames, errs := b.client.Frames(), b.client.Errors()
	for frames != nil || errs != nil {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			b.forwardFrame(frame)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				b.emit(map[string]any{"type": "error", "error": err.Error()})
			}
		}
	}
}

func (b *bridge) forwardFrame(frame workerwire.Frame) {
	switch frame.Type {
	case workerwire.TypeAttached:
		return
	case workerwire.TypeSnapshot:
		var envelope workerwire.SnapshotEnvelope
		var snapshot workerwire.Snapshot
		if json.Unmarshal(frame.Payload, &envelope) == nil && json.Unmarshal(envelope.State, &snapshot) == nil {
			b.emit(map[string]any{"type": "snapshot", "snapshot": snapshot})
		}
	case workerwire.TypeEvent:
		var envelope workerwire.EventEnvelope
		if json.Unmarshal(frame.Payload, &envelope) == nil {
			b.forwardEvent(envelope)
		}
	case workerwire.TypeAck:
		b.pendingMu.Lock()
		name := b.pending[frame.RequestID]
		delete(b.pending, frame.RequestID)
		b.pendingMu.Unlock()
		b.emit(map[string]any{"type": "ack", "request_id": frame.RequestID, "name": name, "payload": json.RawMessage(frame.Payload)})
	case workerwire.TypeDetachAck:
		b.detached.Store(true)
		b.emit(map[string]any{"type": "detach_ack", "request_id": frame.RequestID})
	case workerwire.TypeAlreadyControlled, workerwire.TypeError:
		b.pendingMu.Lock()
		delete(b.pending, frame.RequestID)
		b.pendingMu.Unlock()
		var payload workerwire.ErrorPayload
		_ = json.Unmarshal(frame.Payload, &payload)
		if payload.Message == "" {
			payload.Message = "worker rejected the request"
		}
		b.emit(map[string]any{"type": "error", "request_id": frame.RequestID, "error": payload.Message})
	}
}

func (b *bridge) forwardEvent(envelope workerwire.EventEnvelope) {
	var value any
	if json.Unmarshal(envelope.Data, &value) != nil {
		return
	}
	if object, ok := value.(map[string]any); ok {
		object["type"] = envelope.Kind
		b.emit(object)
	} else {
		field := "data"
		switch envelope.Kind {
		case "text", "think", workerwire.EventPlanDelta:
			field = "delta"
		case "notice", "steer":
			field = "text"
		}
		b.emit(map[string]any{"type": envelope.Kind, field: value})
	}
	if envelope.Kind == "turn_done" {
		// The worker publishes turn_done before its operation wrapper clears
		// activeCancel. Wait for the following idle state before advertising a
		// turn end, otherwise the next command can race cleanup.
		b.turnDone = true
	}
	if envelope.Kind == "state" {
		if stateValue, ok := value.(map[string]any); ok {
			state, ok := stateValue["state"].(string)
			if ok && b.turnDone && (state == string(workerwire.StateIdle) || state == string(workerwire.StateInterrupted)) {
				b.turnDone = false
				b.emit(map[string]any{"type": "turn_end"})
			}
		}
	}
}
