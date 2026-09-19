package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
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
	turnDone  bool
	detached  atomic.Bool
	outMu     sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]string
}

func bridgeCLI(args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "session id to resume")
	modelFlag := fs.String("m", "", "model name")
	providerFlag := fs.String("p", "", "provider name")
	roleFlag := fs.String("role", config.RoleFast, "initial model role")
	modeFlag := fs.String("mode", "execute", "initial mode: execute or plan")
	cautiousFlag := fs.Bool("cautious", false, "ask before running commands / writing files")
	sandboxFlag := fs.String("sandbox", "", "execution sandbox override")
	networkFlag := fs.String("network", "", "execution network override")
	approvalFlag := fs.String("approval", "", "execution approval override")
	trustProjectFlag := fs.Bool("trust-project", false, "allow project-local instructions, profiles, and MCP configuration")
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
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	project, err := config.NewProjectContext(wd, *trustProjectFlag)
	if err != nil {
		return err
	}
	profiles, err := loadProviderProfilesForProject(project)
	if err != nil {
		return err
	}
	sessionID := strings.TrimSpace(*sessionFlag)
	modelName, providerName := strings.TrimSpace(*modelFlag), strings.TrimSpace(*providerFlag)
	if sessionID != "" {
		if dir, dirErr := config.Dir(); dirErr == nil {
			if st, openErr := session.Open(filepath.Join(dir, "sessions.db")); openErr == nil {
				if meta, _, loadErr := st.Load(sessionID); loadErr == nil {
					if modelName == "" {
						modelName = meta.Model
					}
					if providerName == "" {
						providerName = meta.Provider
					}
				}
				_ = st.Close()
			}
		}
	} else {
		sessionID = session.NewSessionID()
	}

	sysPrompt := systemPromptForProject(project)
	if modelName == "" && providerName == "" {
		_, modelName, providerName, err = agent.NewConfiguredForRole(cfg, profiles, *roleFlag, sysPrompt, false)
		if err != nil {
			return err
		}
	} else {
		_, modelName, providerName, err = agent.NewConfigured(agent.BuildOptions{
			Config: cfg, Profiles: profiles, Model: modelName, Provider: providerName,
			Role: *roleFlag, SystemPrompt: sysPrompt,
		})
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
		if err != nil {
			return err
		}
	} else {
		if err = runtimeFile.WritePrompt(sysPrompt); err != nil {
			return err
		}
		cwd := project.Root
		env := map[string]string{
			"GHG_INTERNAL_WORKER":            "1",
			workerwire.WorkerSessionEnv:      sessionID,
			workerwire.WorkerBaseEnv:         dir,
			workerwire.WorkerCWDEnv:          cwd,
			workerwire.WorkerModelEnv:        modelName,
			workerwire.WorkerProviderEnv:     providerName,
			workerwire.WorkerRoleEnv:         *roleFlag,
			workerwire.WorkerModeEnv:         *modeFlag,
			workerwire.WorkerCautiousEnv:     strconv.FormatBool(*cautiousFlag),
			workerwire.WorkerTrustProjectEnv: strconv.FormatBool(project.Trusted),
		}
		if *sandboxFlag != "" {
			env[workerwire.WorkerSandboxEnv] = *sandboxFlag
		}
		if *networkFlag != "" {
			env[workerwire.WorkerNetworkEnv] = *networkFlag
		}
		if *approvalFlag != "" {
			env[workerwire.WorkerApprovalEnv] = *approvalFlag
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

	b := &bridge{client: client, process: process, pending: make(map[string]string)}
	defer b.close()
	b.emit(map[string]any{"type": "bridge_ready", "session_id": sessionID, "commands": workerwire.Commands()})
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
	if !workerwire.KnownCommand(request.Name) {
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
		var snapshot workerwire.Snapshot
		if json.Unmarshal(frame.Payload, &snapshot) == nil {
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
		case workerwire.EventText, workerwire.EventThink, workerwire.EventPlanDelta:
			field = "delta"
		case workerwire.EventNotice, workerwire.EventSteer:
			field = "text"
		}
		b.emit(map[string]any{"type": envelope.Kind, field: value})
	}
	if envelope.Kind == workerwire.EventTurnDone {
		// The worker publishes turn_done before its operation wrapper clears
		// activeCancel. Wait for the following idle state before advertising a
		// turn end, otherwise the next command can race cleanup.
		b.turnDone = true
	}
	if envelope.Kind == workerwire.EventState {
		if stateValue, ok := value.(map[string]any); ok {
			state, ok := stateValue["state"].(string)
			if ok && b.turnDone && (state == string(workerwire.StateIdle) || state == string(workerwire.StateInterrupted)) {
				b.turnDone = false
				b.emit(map[string]any{"type": "turn_end"})
			}
		}
	}
}
