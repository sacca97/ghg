// `ghg run` — non-interactive (headless) mode: one turn of the agent with
// no TUI and no trust prompt, for trusted automation and scripting. Piped
// stdin is appended to the prompt. --format json emits the raw event stream
// as newline-delimited JSON; the final event is {"type":"done",...} or
// {"type":"error",...}. Exit code 0 on success, 1 on error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/export"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"github.com/sacca97/ghg/internal/tools"
)

func runCLI(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text (stream the reply) or json (newline-delimited event stream)")
	modelFlag := fs.String("m", "", "model name from ~/.ghg/config.json (default: defaultModel)")
	providerFlag := fs.String("p", "", "provider to route the model through (default: model's first provider)")
	roleFlag := fs.String("role", "", "model role: default, smart, fast, or tiny (default: fast when -m/-p are omitted)")
	planFlag := fs.Bool("plan", false, "plan the prompt with smart, then execute the plan with fast")
	planOnlyFlag := fs.Bool("plan-only", false, "plan the prompt with smart and exit without executing it")
	exportResultFlag := fs.String("export-result", "", "export structured result (e.g. plan) to this file path")
	exportFormatFlag := fs.String("export-format", "markdown", "export format when -export-result is set (markdown or json)")
	resumeFlag := fs.String("resume", "", "continue this session id (see `ghg sessions`) instead of starting fresh")
	systemFlag := fs.String("system", "", "override the system prompt for this run")
	systemFileFlag := fs.String("system-file", "", "read the system prompt from this file (wins over -system)")
	maxTurnsFlag := fs.Int("max-turns", 0, "cap the tool-call loop at N rounds (0 = uncapped); a capped run exits non-zero")
	timeoutFlag := fs.Duration("timeout", 0, "wall-clock cap on the whole run (e.g. 30s, 5m); 0 = no timeout")
	sandboxFlag := fs.String("sandbox", "", "execution sandbox: read-only, workspace-write, or danger-full-access")
	networkFlag := fs.String("network", "", "execution network: deny or host")
	approvalFlag := fs.String("approval", "", "exceptional capability approval: ask, auto-review, or never")
	quietFlag := fs.Bool("quiet", false, "suppress the stderr tool/session notes (clean stdout for -format json piping)")
	noSessionFlag := fs.Bool("no-session", false, "run without persisting a session (one-off jobs don't clutter ghg sessions)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ghg run [--plan | --plan-only] [--export-result path] [--export-format markdown|json] [--format text|json] [--role role] [-m model] [-p provider] [-resume id] [-system text | -system-file path] [-max-turns N] [-timeout dur] [--sandbox mode] [--network mode] [--approval mode] [-quiet] [-no-session] \"prompt\"")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format %q (want text|json)", *format)
	}

	prompt := strings.Join(fs.Args(), " ")
	// Piped stdin is appended to the prompt (both matter: e.g.
	// `git diff | ghg run "review this"`). Read only when stdin is not a
	// TTY, so interactive `ghg run "…"` never blocks on it.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		if data, err := io.ReadAll(os.Stdin); err == nil {
			if piped := strings.TrimSpace(string(data)); piped != "" {
				if prompt != "" {
					prompt += "\n\n"
				}
				prompt += piped
			}
		}
	}
	if prompt == "" {
		fs.Usage()
		return fmt.Errorf("no prompt given (pass one as an argument or pipe it on stdin)")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ApplyExecutionOverrides(*sandboxFlag, *networkFlag, *approvalFlag); err != nil {
		return err
	}
	profiles, err := loadProviderProfiles()
	if err != nil {
		return err
	}
	// System prompt: -system-file wins over -system (a file is the deliberate
	// choice; a stray -system alongside it is almost certainly stale).
	// Headless mode is explicitly trusted automation, so it receives the same
	// project-local AGENTS.md block without an interactive trust prompt.
	sys := systemPromptForProject(true)
	if *systemFlag != "" {
		sys = *systemFlag
	}
	if *systemFileFlag != "" {
		data, err := os.ReadFile(*systemFileFlag)
		if err != nil {
			return fmt.Errorf("-system-file: %w", err)
		}
		sys = string(data)
	}
	sys = agent.CompileSystemPrompt(sys,
		skills.PromptBlock(skills.Scan(skills.DefaultDirs()...)),
		memory.PromptBlock(memory.Installation(), memory.Session(*resumeFlag)),
	)

	if *roleFlag != "" && !config.IsRole(*roleFlag) {
		return fmt.Errorf("unknown role %q (roles: %s)", *roleFlag, strings.Join(config.SupportedRoles(), ", "))
	}
	if *roleFlag != "" && (*modelFlag != "" || *providerFlag != "") {
		return errors.New("--role cannot be combined with -m or -p")
	}
	if *planFlag && *planOnlyFlag {
		return errors.New("--plan cannot be combined with --plan-only")
	}
	if (*planFlag || *planOnlyFlag) && (*roleFlag != "" || *modelFlag != "" || *providerFlag != "" || *resumeFlag != "") {
		return errors.New("--plan/--plan-only use the configured smart and fast roles and cannot be combined with --role, -m, -p, or --resume")
	}

	// ctrl+c cancels planning or execution; -timeout caps the whole operation.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	ev := agent.Events{}
	var emit func(any) // set only for --format json
	planDeltaSeen := false
	note := func(format string, a ...any) {
		if !*quietFlag {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		}
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		var emitMu sync.Mutex
		emit = func(v any) {
			emitMu.Lock()
			defer emitMu.Unlock()
			if err := enc.Encode(v); err != nil {
				fmt.Fprintln(os.Stderr, "ghg: json encode:", err)
			}
		}
		setupWireEvents(&ev, emit)
	} else {
		ev.OnText = func(d string) { fmt.Fprint(os.Stdout, d) }
		ev.OnToolStart = func(_, name, args string) { note("⚒ %s", name) }
	}
	if emit == nil {
		ev.OnPlanDelta = func(delta string) {
			planDeltaSeen = true
			fmt.Fprint(os.Stdout, delta)
		}
	}
	runtime, runtimeCleanup, err := tools.NewConfiguredRuntime(".", cfg.Execution, true, cfg.PostEdit)
	if err != nil {
		return err
	}
	defer runtimeCleanup()
	lspMgr := lsp.NewManager(lsp.FromConfigMap(cfg.LSPServers))
	lspMgr.SetRuntime(runtime)
	defer lspMgr.Close()
	if emit != nil {
		status := runtime.Policy.Status()
		emit(map[string]any{
			"type": "execution_policy", "mode": status.Mode, "backend": status.Backend,
			"network": status.Network, "workspace": status.Workspace,
			"read_roots": status.ReadRoots, "write_roots": status.WriteRoots,
			"cache_roots": status.CacheRoots, "immutable_roots": status.ImmutableRoots,
			"temp_roots":      status.TempRoots,
			"protected_roots": status.ProtectedRoots, "degraded": status.Degraded,
			"reason": status.Reason,
		})
		runtime.OnAudit = func(audit tools.ExecutionAudit) {
			emit(map[string]any{
				"type": "execution_audit", "disposition": audit.Disposition,
				"fingerprint": audit.Request.Fingerprint, "granted": audit.Granted,
				"error": audit.Error,
			})
		}
		runtime.OnReviewerCall = func(call tools.ReviewerCall) {
			emit(map[string]any{
				"type": "model_call_end", "role": call.Role, "provider": call.Provider,
				"model": call.Model, "protocol": call.Protocol, "purpose": call.Purpose,
				"latency_ms": call.LatencyMS, "usage": call.Usage, "error": call.Error,
			})
		}
	}

	var ag *agent.Agent
	var modelName, provName string
	if *planFlag || *planOnlyFlag {
		var planner *agent.Agent
		var planErr error
		if *modelFlag == "" && *providerFlag == "" {
			planner, _, _, planErr = agent.NewConfiguredForRole(cfg, profiles, config.RoleSmart, sys, false)
		} else {
			planner, _, _, planErr = agent.NewConfigured(agent.BuildOptions{
				Config: cfg, Profiles: profiles, Model: *modelFlag, Provider: *providerFlag,
				Role: config.RoleDefault, SystemPrompt: sys,
			})
		}
		if planErr != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				planErr = fmt.Errorf("run timed out after %s", *timeoutFlag)
			}
			if emit != nil {
				emit(map[string]string{"type": "error", "error": planErr.Error()})
			}
			return planErr
		}
		planner.Runtime = runtime
		planner.Effort = defaultEffort(cfg)
		planner.PlanMode = true
		planner.MaxTurns = *maxTurnsFlag
		final, planErr := planner.Turn(ctx, prompt, ev)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			planErr = fmt.Errorf("run timed out after %s", *timeoutFlag)
		}
		if planErr != nil {
			if emit != nil {
				emit(map[string]string{"type": "error", "error": planErr.Error()})
			}
			return planErr
		}
		planMD, ok := agent.ExtractProposedPlan(final)
		if !ok {
			planErr = fmt.Errorf("planner finished without a <proposed_plan> block; response preview: %q", boundedPlanPreview(final))
			if emit != nil {
				emit(map[string]string{"type": "error", "error": planErr.Error()})
			}
			return planErr
		}
		if emit != nil {
			emit(map[string]string{"type": "plan", "markdown": planMD})
		} else if !planDeltaSeen {
			fmt.Fprintln(os.Stdout, planMD)
		}
		if *exportResultFlag != "" {
			planJSON, _ := json.Marshal(map[string]string{"markdown": planMD})
			record := session.WorkflowResultRecord{
				ResultID:  fmt.Sprintf("plan-%x", time.Now().UnixNano()),
				Kind:      "plan",
				Version:   2,
				Payload:   string(planJSON),
				Role:      planner.Role,
				Provider:  planner.Provider,
				Model:     planner.Model,
				CreatedAt: time.Now().UTC(),
			}
			exportData, exportErr := export.RenderResult(record, *exportFormatFlag)
			if exportErr == nil {
				cwd, _ := os.Getwd()
				_, exportErr = export.WriteExportFile(*exportResultFlag, exportData, true, cwd)
			}
			if exportErr != nil {
				if emit != nil {
					emit(map[string]string{"type": "error", "error": "export failed: " + exportErr.Error()})
				}
				return fmt.Errorf("export result: %w", exportErr)
			}
		}
		if *planOnlyFlag {
			if emit != nil {
				emit(map[string]string{"type": "done", "markdown": planMD})
			}
			return nil
		}
		ag, modelName, provName, err = agent.NewConfiguredForRole(cfg, profiles, config.RoleForMode(config.ModeActing), sys, false)
		if err == nil {
			prompt = fmt.Sprintf("Execute the following approved plan. Create and maintain a todowrite\nchecklist while implementing it.\n\n%s", planMD)
		}
	} else if *modelFlag == "" && *providerFlag == "" {
		if *roleFlag == "" {
			ag, modelName, provName, err = agent.NewConfiguredForRole(cfg, profiles, config.RoleForMode(config.ModeActing), sys, false)
		} else {
			ag, modelName, provName, err = agent.NewConfiguredForRole(cfg, profiles, *roleFlag, sys, false)
		}
	} else {
		ag, modelName, provName, err = agent.NewConfigured(agent.BuildOptions{
			Config: cfg, Profiles: profiles, Model: *modelFlag, Provider: *providerFlag,
			Role: config.RoleDefault, SystemPrompt: sys,
		})
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	ag.Runtime = runtime
	if runtime.ApprovalMode == tools.ApprovalAutoReview {
		runtime.Reviewer = ag.ApproveForMe
	}
	ag.Effort = defaultEffort(cfg)
	ag.MaxTurns = *maxTurnsFlag

	// Output payloads are durable for session runs and private temporary
	// files for --no-session runs. The latter are cleaned up when this process
	// exits; the agent's live message slice still makes them readable during
	// the run.
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
		if *noSessionFlag {
			outputStore, err = session.NewTempOutputStoreWithLimit(maxBytes)
		} else if dir, derr := config.Dir(); derr == nil {
			outputStore, err = session.NewOutputStoreWithLimit(filepath.Join(dir, "outputs"), maxBytes)
		} else {
			err = derr
		}
		if err != nil {
			config.LogEvent("output.open", "FAILED: "+err.Error())
			outputStore = nil
		}
	}
	if outputStore != nil {
		defer func() {
			if *noSessionFlag {
				_ = outputStore.Cleanup()
			}
		}()
	}
	ag.Outputs = outputStore
	ag.SubagentsDisabled = !config.SubagentsEnabled(cfg)

	// Session: resume an existing one, or create a fresh one — unless
	// -no-session (a one-off cron job shouldn't clutter ghg sessions).
	var store *session.Store
	var sessionID string
	saved := 0 // headless sessions persist the system message at sequence zero
	if !*noSessionFlag {
		dir, derr := config.Dir()
		if derr != nil {
			return fmt.Errorf("session directory: %w", derr)
		}
		st, serr := session.Open(filepath.Join(dir, "sessions.db"))
		if serr != nil {
			return fmt.Errorf("open session database: %w", serr)
		}
		store = st
		store.Outputs = outputStore
		defer func() { _ = st.Close() }()

		if *resumeFlag != "" {
			meta, msgs, lerr := store.Load(*resumeFlag)
			if lerr != nil {
				return fmt.Errorf("-resume: %w", lerr)
			}
			sessionID = meta.ID
			loaded := msgs
			if len(loaded) > 0 && loaded[0].Role == "system" {
				loaded = loaded[1:]
			}
			ag.Messages = append(ag.Messages[:1], loaded...) // keep our system prompt, replay the rest
			saved = len(ag.Messages)
		} else {
			cwd, cerr := os.Getwd()
			if cerr != nil {
				return fmt.Errorf("session working directory: %w", cerr)
			}
			id, ierr := store.Create(cwd, modelName, provName)
			if ierr != nil {
				return fmt.Errorf("create session: %w", ierr)
			}
			sessionID = id
		}
	}
	if store != nil {
		ag.OutputCatalog = store
		ag.HistoryCatalog = store
		ag.SetObservationStore(store.ObservationRegistryStore())
		ag.SetSearchStore(store.SearchRegistryStore())
	}
	ag.SetSessionID(sessionID)
	if err := ag.BindState(ctx); err != nil {
		return fmt.Errorf("bind session tool state: %w", err)
	}
	if store != nil && sessionID != "" {
		ev.OnCompactionReady = func(raw []models.Message, summary string, cutoff int) error {
			return store.PersistCompaction(sessionID, saved, raw, modelName, provName, summary, cutoff)
		}
		ev.OnCompacted = func(_ string, _ int) {
			saved = len(ag.MessagesSnapshot())
		}
	}
	if *resumeFlag != "" {
		ag.RebuildTouched(ag.MessagesSnapshot())
	}

	final, err := ag.Turn(ctx, prompt, ev)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("run timed out after %s", *timeoutFlag)
	}
	if emit != nil {
		if err != nil {
			emit(map[string]string{"type": "error", "error": err.Error()})
		} else {
			emit(map[string]string{"type": "done", "text": final})
		}
	} else {
		fmt.Fprintln(os.Stdout) // end the streamed reply's line
	}

	// Best-effort persistence (the TUI's persist does the same each turn).
	// Before the first compaction, saved is zero so the headless session keeps
	// its system prompt at sequence zero. After a cutover it tracks the derived
	// view and Save appends only the new raw tail.
	if store != nil && sessionID != "" {
		if serr := store.Save(sessionID, saved, ag.MessagesSnapshot(), modelName, provName); serr != nil {
			config.LogEvent("session.save", "run FAILED id="+sessionID+": "+serr.Error())
		}
		note("session %s — resume with: ghg run -resume %s \"…\" · or interactively: ghg --resume %s", sessionID, sessionID, sessionID)
	}
	return err
}

func boundedPlanPreview(text string) string {
	const maxPreviewRunes = 512
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > maxPreviewRunes {
		return string(runes[:maxPreviewRunes]) + "…"
	}
	return string(runes)
}

func setupWireEvents(ev *agent.Events, emit func(any)) {
	if ev == nil || emit == nil {
		return
	}
	ev.OnText = func(delta string) {
		emit(map[string]any{"type": "text", "delta": delta})
	}
	ev.OnToolStart = func(id, name, args string) {
		emit(map[string]any{"type": "tool_start", "id": id, "name": name, "args": args})
	}
	ev.OnToolOutput = func(id, output string) {
		emit(map[string]any{"type": "tool_output", "id": id, "output": output})
	}
	ev.OnToolEnd = func(id, name, result string) {
		emit(map[string]any{"type": "tool_end", "id": id, "name": name, "result": result})
	}
	ev.OnToolTelemetry = func(telemetry agent.ToolTelemetry) {
		emit(map[string]any{
			"type": "tool_telemetry", "id": telemetry.ID, "name": telemetry.Name,
			"preview_bytes": telemetry.PreviewBytes, "retained_bytes": telemetry.RetainedBytes,
			"original_bytes": telemetry.OriginalBytes, "truncated": telemetry.Truncated,
			"bash_redirect": telemetry.BashRedirect, "fingerprint": telemetry.Fingerprint,
			"duplicate": telemetry.Duplicate, "metadata": telemetry.Metadata,
		})
	}
	ev.OnModelCallStart = func(call agent.ModelCallStart) {
		emit(map[string]any{
			"type": "model_call_start", "role": call.Role, "provider": call.Provider,
			"model": call.Model, "protocol": call.Protocol, "purpose": call.Purpose,
		})
	}
	ev.OnModelCallEnd = func(call agent.ModelCallEnd) {
		emit(map[string]any{
			"type": "model_call_end", "role": call.Role, "provider": call.Provider,
			"model": call.Model, "protocol": call.Protocol, "latency_ms": call.LatencyMS,
			"purpose": call.Purpose, "finish_reason": call.FinishReason, "usage": call.Usage, "error": call.Error,
		})
	}
	ev.OnPromptView = func(view agent.PromptView) {
		emit(map[string]any{
			"type": "prompt_view", "role": view.Role, "provider": view.Provider,
			"model": view.Model, "protocol": view.Protocol, "purpose": view.Purpose,
			"message_count": view.MessageCount, "estimated_tokens": view.EstimatedTokens,
			"serialized_bytes": view.SerializedBytes, "context_limit": view.ContextLimit,
		})
	}
	ev.OnPlanDelta = func(delta string) {
		emit(map[string]any{"type": "plan_delta", "delta": delta})
	}
}
