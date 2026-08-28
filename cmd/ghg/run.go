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

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
)

func runCLI(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text (stream the reply) or json (newline-delimited event stream)")
	modelFlag := fs.String("m", "", "model name from ~/.ghg/config.json (default: defaultModel)")
	providerFlag := fs.String("p", "", "provider to route the model through (default: model's first provider)")
	roleFlag := fs.String("role", "", "model role: default, smart, fast, or tiny (default: fast when -m/-p are omitted)")
	planFlag := fs.Bool("plan", false, "plan the prompt with smart, then execute the plan with fast")
	planOnlyFlag := fs.Bool("plan-only", false, "plan the prompt with smart and exit without executing it")
	resumeFlag := fs.String("resume", "", "continue this session id (see `ghg sessions`) instead of starting fresh")
	systemFlag := fs.String("system", "", "override the system prompt for this run")
	systemFileFlag := fs.String("system-file", "", "read the system prompt from this file (wins over -system)")
	maxTurnsFlag := fs.Int("max-turns", 0, "cap the tool-call loop at N rounds (0 = uncapped); a capped run exits non-zero")
	timeoutFlag := fs.Duration("timeout", 0, "wall-clock cap on the whole run (e.g. 30s, 5m); 0 = no timeout")
	quietFlag := fs.Bool("quiet", false, "suppress the stderr tool/session notes (clean stdout for -format json piping)")
	noSessionFlag := fs.Bool("no-session", false, "run without persisting a session (one-off jobs don't clutter ghg sessions)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ghg run [--plan | --plan-only] [--format text|json] [--role role] [-m model] [-p provider] [-resume id] [-system text | -system-file path] [-max-turns N] [-timeout dur] [-quiet] [-no-session] \"prompt\"")
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
	profiles, err := loadProviderProfiles()
	if err != nil {
		return err
	}
	definitions, err := agent.LoadAgentDefinitions(agent.DefinitionLoadOptions{ProjectTrusted: true})
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
		ev.OnText = func(d string) { emit(map[string]string{"type": "text", "delta": d}) }
		ev.OnToolStart = func(_, name, args string) {
			emit(map[string]string{"type": "tool_start", "name": name, "args": args})
		}
		ev.OnToolOutput = func(id, output string) {
			emit(map[string]string{"type": "tool_output", "id": id, "output": output})
		}
		ev.OnToolEnd = func(_, name, result string) {
			emit(map[string]string{"type": "tool_end", "name": name, "result": result})
		}
		ev.OnToolTelemetry = func(telemetry agent.ToolTelemetry) {
			emit(map[string]any{
				"type": "tool_telemetry", "id": telemetry.ID, "name": telemetry.Name,
				"preview_bytes": telemetry.PreviewBytes, "retained_bytes": telemetry.RetainedBytes,
				"original_bytes": telemetry.OriginalBytes, "truncated": telemetry.Truncated,
				"bash_redirect": telemetry.BashRedirect, "metadata": telemetry.Metadata,
			})
		}
		ev.OnModelCallStart = func(call agent.ModelCallStart) {
			emit(map[string]any{
				"type": "model_call_start", "role": call.Role, "provider": call.Provider,
				"model": call.Model, "protocol": call.Protocol,
			})
		}
		ev.OnModelCallEnd = func(call agent.ModelCallEnd) {
			emit(map[string]any{
				"type": "model_call_end", "role": call.Role, "provider": call.Provider,
				"model": call.Model, "protocol": call.Protocol, "latency_ms": call.LatencyMS,
				"finish_reason": call.FinishReason, "usage": call.Usage, "error": call.Error,
			})
		}
	} else {
		ev.OnText = func(d string) { fmt.Fprint(os.Stdout, d) }
		ev.OnToolStart = func(_, name, args string) { note("⚒ %s", name) }
	}

	var ag *agent.Agent
	var modelName, provName string
	if *planFlag || *planOnlyFlag {
		planner, _, _, planErr := newRoleAgent(cfg, profiles, config.RoleSmart, sys)
		if planErr != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				planErr = fmt.Errorf("run timed out after %s", *timeoutFlag)
			}
			if emit != nil {
				emit(map[string]string{"type": "error", "error": planErr.Error()})
			}
			return planErr
		}
		planner.Effort = cfg.DefaultEffort
		if planner.Effort == "" {
			planner.Effort = "medium"
		}
		definition := agent.BuiltInPlannerDefinition()
		if loaded, ok := definitions[definition.Name]; ok {
			definition = loaded
		}
		planned, planErr := agent.ProposePlanWithDefinition(ctx, planner, prompt, definition, ev)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			planErr = fmt.Errorf("run timed out after %s", *timeoutFlag)
		}
		if planErr != nil {
			if emit != nil {
				emit(map[string]string{"type": "error", "error": planErr.Error()})
			}
			return planErr
		}
		if emit != nil {
			emit(map[string]any{"type": "plan", "plan": planned})
		} else {
			fmt.Fprintln(os.Stdout, headlessPlanText(planned))
		}
		if *planOnlyFlag {
			if emit != nil {
				emit(map[string]any{"type": "done", "plan": planned})
			}
			return nil
		}
		ag, modelName, provName, err = newModeAgent(cfg, profiles, config.ModeActing, sys)
		if err == nil {
			err = ag.SetTodos(planned.Todos())
			if err == nil {
				prompt = executionPrompt(planned)
			}
		}
	} else if *modelFlag == "" && *providerFlag == "" {
		if *roleFlag == "" {
			ag, modelName, provName, err = newModeAgent(cfg, profiles, config.ModeActing, sys)
		} else {
			ag, modelName, provName, err = newRoleAgent(cfg, profiles, *roleFlag, sys)
		}
	} else {
		prov, mdl, apiID, resolveErr := cfg.Resolve(*modelFlag, *providerFlag)
		if resolveErr != nil {
			return resolveErr
		}
		modelName, provName = *modelFlag, *providerFlag
		if modelName == "" {
			modelName = cfg.DefaultModel
		}
		if provName == "" {
			provName = cfg.DefaultProvider
			if provName == "" && len(mdl.Providers) > 0 {
				provName = mdl.Providers[0]
			}
		}
		key, keyErr := prov.ResolveKey()
		if keyErr != nil {
			return keyErr
		}
		backend, backendErr := newProviderBackend(profiles, provName, prov, key, cfg.MaxRetries, apiID, mdl.API)
		if backendErr != nil {
			return backendErr
		}
		ag = agent.New(backend, apiID, mdl.MaxTokens, sys)
		ag.ModelName, ag.Provider, ag.Role = modelName, provName, config.RoleDefault
		ag.ContextLimit = mdl.ContextWindow()
		configureSubagentFactory(ag, cfg, profiles)
	}
	if err != nil {
		return err
	}
	ag.Effort = cfg.DefaultEffort
	if ag.Effort == "" {
		ag.Effort = "medium"
	}
	ag.MaxTurns = *maxTurnsFlag

	// Artifact payloads are durable for session runs and private temporary
	// files for --no-session runs. The latter are cleaned up when this process
	// exits; the agent's live message slice still makes them readable during
	// the run.
	artifactsDisabled := cfg.Artifacts != nil && cfg.Artifacts.Enabled != nil && !*cfg.Artifacts.Enabled
	var artifactStore *artifact.Store
	if !artifactsDisabled {
		maxBytes := artifact.DefaultMaxBytes
		if cfg.Artifacts != nil && cfg.Artifacts.MaxBytes > 0 {
			maxBytes = cfg.Artifacts.MaxBytes
		}
		if *noSessionFlag {
			artifactStore, err = artifact.NewTempWithLimit(maxBytes)
		} else if dir, derr := config.Dir(); derr == nil {
			artifactStore, err = artifact.NewWithLimit(filepath.Join(dir, "artifacts"), maxBytes)
		} else {
			err = derr
		}
		if err != nil {
			config.LogEvent("artifact.open", "FAILED: "+err.Error())
			artifactStore = nil
		}
	}
	if artifactStore != nil {
		defer func() {
			if *noSessionFlag {
				_ = artifactStore.Cleanup()
			}
		}()
	}
	ag.ArtifactStore = artifactStore
	ag.ArtifactWriter = artifactStore
	ag.ArtifactsDisabled = artifactsDisabled

	// Session: resume an existing one, or create a fresh one — unless
	// -no-session (a one-off cron job shouldn't clutter ghg sessions).
	var store *session.Store
	var sessionID string
	if !*noSessionFlag {
		if dir, derr := config.Dir(); derr == nil {
			if st, serr := session.Open(dir + "/sessions.db"); serr == nil {
				store = st
				defer func() { _ = st.Close() }()
			}
		}
	}
	if store != nil {
		if *resumeFlag != "" {
			meta, msgs, lerr := store.Load(*resumeFlag)
			if lerr != nil {
				return fmt.Errorf("-resume: %w", lerr)
			}
			sessionID = meta.ID
			ag.Messages = append(ag.Messages[:1], msgs[1:]...) // keep our system prompt, replay the rest
		} else if cwd, cerr := os.Getwd(); cerr == nil {
			if id, ierr := store.Create(cwd, modelName, provName); ierr == nil {
				sessionID = id
			}
		}
	}
	if store != nil {
		ag.ArtifactCatalog = store
		ag.SetObservationStore(store.ObservationRegistryStore())
		ag.SetSearchStore(store.SearchRegistryStore())
	}
	ag.SetSessionID(sessionID)
	if err := ag.BindState(ctx); err != nil {
		return fmt.Errorf("bind session tool state: %w", err)
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
	// Save from index 0: Load re-derives the system-prompt slot, so a resumed
	// conversation must not skip it (saving from 1 shifts everything off).
	if store != nil && sessionID != "" {
		if serr := store.Save(sessionID, 0, ag.MessagesSnapshot(), modelName, provName); serr != nil {
			config.LogEvent("session.save", "run FAILED id="+sessionID+": "+serr.Error())
		}
		note("session %s — resume with: ghg run -resume %s \"…\" · or interactively: ghg --resume %s", sessionID, sessionID, sessionID)
	}
	return err
}

func executionPrompt(p agent.Plan) string {
	var b strings.Builder
	b.WriteString("Execute this validated plan now. Use the available tools to make the changes and verify the acceptance checks; do not merely describe what should be done. Keep the todowrite checklist updated.\n\n")
	b.WriteString("Goal: " + p.Goal + "\n\nOrdered steps:\n")
	for i, step := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	b.WriteString("\nAcceptance checks:\n")
	for _, check := range p.AcceptanceChecks {
		b.WriteString("- " + check + "\n")
	}
	return b.String()
}

func headlessPlanText(p agent.Plan) string {
	var b strings.Builder
	b.WriteString("Plan\nGoal: " + p.Goal + "\n\nSteps:\n")
	for i, step := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	b.WriteString("\nAcceptance checks:\n")
	for _, check := range p.AcceptanceChecks {
		b.WriteString("- " + check + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
