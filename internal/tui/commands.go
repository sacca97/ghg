package tui

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/models"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// registryEntry describes one user-facing command.
type registryEntry struct {
	Name      string
	Hint      string
	Keybind   string
	Category  string
	Immediate bool
}

// registry lists every user-facing slash command.
var registry = []registryEntry{
	{Name: "/auth", Hint: "[provider] [key] — connect any profile (bare lists profiles; provider-only opens a masked prompt; also: ghg auth <provider>)", Category: "Agent"},
	{Name: "/cd", Hint: "[dir] — change working directory (bare prints it)", Category: "Session"},
	{Name: "/clear", Hint: "— reset conversation", Category: "Session", Immediate: true},
	{Name: "/compact", Hint: "[model] [provider]|off — compact now, or pick the compaction model (off restores the default); retry undoes the last compaction, log lists them; compaction level: ctrl+p › Compaction level", Category: "Session", Immediate: true},
	{Name: "/context-doctor", Hint: "— audit what a fresh session injects (skills, MCP, tool schemas) and its token cost", Category: "Session", Immediate: true},
	{Name: "/detach", Hint: "— leave a running worker in the background (ctrl+d)", Keybind: "ctrl+d", Category: "Session", Immediate: true},
	{Name: "/effort", Hint: "[level] — reasoning effort: off·low·medium·high (bare opens selector)", Category: "Agent", Immediate: true},
	{Name: "/export", Hint: "[chat|plan|review|last] [path] [--format json|markdown] [--force] — export chat log or structured result to a file", Category: "Session"},
	{Name: "/export-result", Hint: "[chat|plan|review|last] [path] [--format json|markdown] [--force] — export chat log, structured result, or last message to a file", Category: "Session"},
	{Name: "/fork", Hint: "[name] — copy the conversation into a new session (pick a point in the rewind picker with f)", Category: "Session"},
	{Name: "/goal", Hint: "<text> — keep working until the goal is met (resume | clear | rounds <n>|default [--global])", Category: "Session", Immediate: true},
	{Name: "/goal-from-context", Hint: "[n] — formulate a goal from the last n messages (default 8) and work until it's met", Category: "Session", Immediate: true},
	{Name: "/help", Hint: "— show all commands and keybindings", Category: "App", Immediate: true},
	{Name: "/mcp", Hint: "[name] [reconnect|enable|disable] — MCP servers: status, reconnect, toggle", Category: "Session", Immediate: true},
	{Name: "/me", Hint: "— edit your standing instructions (~/.ghg/me.md) in $EDITOR", Category: "Agent"},
	{Name: "/memory", Hint: "[n] [session] — saved memories: list what's injected each turn, mark entry n done", Category: "Session"},
	{Name: "/model", Hint: "<name> [provider] — switch model (any provider-catalog model works; refresh pulls new announcements)", Category: "Agent", Immediate: true},
	{Name: "/plan", Hint: "[goal] — enter read-only Plan mode or explore a goal with the smart model (run it with /execute)", Category: "Agent"},
	{Name: "/pwd", Hint: "— print working directory", Category: "Session", Immediate: true},
	{Name: "/quit", Hint: "— exit", Keybind: "ctrl+c ctrl+c", Category: "App", Immediate: true},
	{Name: "/rename", Hint: "[title] — retitle this session", Category: "Session"},
	{Name: "/report", Hint: "— bug-report bundle: prefilled GitHub-issue link + copy-pastable environment snippet (terminal, versions)", Category: "App", Immediate: true},
	{Name: "/resume", Hint: "[id] — resume a previous session", Category: "Session", Immediate: true},
	{Name: "/review", Hint: "<target> — run a one-shot read-only review with structured findings using the smart model", Category: "Agent"},
	{Name: "/schedule", Hint: "@every 10m|<@at time> <prompt> — schedule a wakeup turn; list | cancel <n>", Category: "Session"},
	{Name: "/tasks", Hint: "[id] — background subagents: focus the dock, or open one subagent's live view", Keybind: "ctrl+t", Category: "Session", Immediate: true},
	{Name: "/execute", Hint: "[plan] — execute the latest proposal or supplied plan with the fast model", Category: "Agent", Immediate: true},
	{Name: "!cmd", Hint: "— run a shell command locally; output lands in the transcript and the conversation", Category: "App"},
}

// slashRegistry returns the registry entries that name a slash command,
// sorted by name (the canonical order for help and completion).
func slashRegistry() []registryEntry {
	var out []registryEntry
	for _, e := range registry {
		if strings.HasPrefix(e.Name, "/") {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registryFind returns the entry for a slash command name (nil for "!cmd"
// and unknown names).
func registryFind(name string) *registryEntry {
	for i := range registry {
		if registry[i].Name == name {
			return &registry[i]
		}
	}
	return nil
}

func registryImmediate(name string) bool {
	e := registryFind(name)
	return e != nil && e.Immediate
}

// helpText renders /help from the registry plus the settings's keybind hints:
// slash commands first (sorted), then the keybindings roster. Nothing here is
// hand-maintained anymore — every line comes from one of the two tables.
func helpText() string {
	var b strings.Builder
	for _, e := range slashRegistry() {
		b.WriteString(e.Name + " " + e.Hint + "\n")
	}
	b.WriteString(palHintRewind + " — " + palDescRewind + "\n")
	b.WriteString("!cmd " + registryFind("!cmd").Hint + "\n")
	b.WriteString("tab — complete")
	for _, hint := range []string{
		"ctrl+k — clear the conversation",
		"ctrl+t — focus the subagents dock (↑/↓ select, enter opens, esc backs out)",
		"ctrl+d — detach a running turn",
		palHintThinking + " — toggle thinking timer",
		"ctrl+e — expand the last tool result",
		"ctrl+j / shift+enter — newline",
		"ctrl+v — paste image",
		"esc — interrupt the agent",
		"esc esc (idle) — " + palDescRewind + " (↑/↓ browse, enter rewinds, f forks)",
		"while busy with queued messages: ↑/↓ select, del removes",
		"PgUp/PgDn — scroll · wheel — scroll · drag — select/copy text",
		palHintQuit + " — quit",
	} {
		b.WriteString(" · " + hint)
	}
	return b.String()
}

// busyCmd reports whether a slash command should be handled immediately while
// a turn is in flight. Settings/views are safe; /plan and /execute also need
// to report their busy state themselves rather than being queued as literal
// chat text (queued text is submitted to the model verbatim after the turn).
func busyCmd(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/help", "/effort", "/tasks", "/cd", "/pwd", "/report", "/detach":
		return true
	case "/plan", "/execute", "/review": // handled immediately so a slash command is not sent as chat text
		return true
	case "/auth": // must run now even while busy: an inline key queued as a chat message would be sent to the model
		return true
	case "/goal": // status, clear, and rounds are settings; resume/<text> submit turns
		return len(fields) == 1 || fields[1] == "clear" || fields[1] == "rounds"
	}
	return false
}

func (m *model) command(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return m, nil
	}
	switch fields[0] {
	case "/quit", "/exit", "/q":
		return m, tea.Quit
	case "/detach":
		live := m.busy || m.workerState == workerwire.StateRunning || m.workerState == workerwire.StateWaitingApproval || m.workerLiveWork
		if m.workerClient == nil || !live {
			m.append(dimStyle.Render("(nothing running to detach)"))
			return m, nil
		}
		if m.detachRequestID != "" {
			return m, nil
		}
		requestID := workerRequestID("detach")
		if err := m.workerClient.Send(workerwire.CommandDetach, requestID, nil); err != nil {
			m.append(errStyle.Render("detach failed: " + err.Error()))
			return m, nil
		}
		m.detachRequestID = requestID
		return m, nil
	case "/clear":
		if m.busy {
			m.append(dimStyle.Render("(busy — /clear after this turn)"))
			return m, nil
		}
		if !m.requireAgent() {
			return m, nil
		}
		m.resetSessionState()
		m.append(dimStyle.Render("(conversation cleared)"))
	case "/memory":
		m.memoryCommand(fields[1:])
	case "/schedule":
		m.scheduleCommand(fields[1:])
	case "/me":
		return m, m.openMe()
	case "/compact":
		if len(fields) == 1 && (m.workerClient != nil || m.workerOnly) {
			if m.busy {
				m.append(dimStyle.Render("(busy — /compact after this turn)"))
				return m, nil
			}
			if !m.requireAgent() {
				return m, nil
			}
			if m.workerClient == nil && !m.ensureWorker() {
				m.append(errStyle.Render("compact failed: worker unavailable: " + m.workerStartError))
				return m, nil
			}
			requestID := workerRequestID("compact")
			m.busy = true
			m.turnStart = m.nowFn()
			m.append(dimStyle.Render("◎ compacting…"))
			m.cancel = func() {
				if m.workerClient != nil {
					_ = m.workerClient.Send(workerwire.CommandCancel, requestID+"-cancel", nil)
				}
			}
			if err := m.workerClient.Send(workerwire.CommandCompact, requestID, nil); err != nil {
				m.busy = false
				m.cancel = nil
				m.append(errStyle.Render("compact failed: " + err.Error()))
			}
			return m, m.spin.Tick
		}
		if len(fields) > 1 {
			switch fields[1] {
			case "retry":
				m.compactRetry()
				return m, nil
			case "log":
				m.compactLog()
				return m, nil
			}
			m.compactCommand(fields[1:])
			return m, nil
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /compact will land after this turn)"))
			return m, nil
		}
		if !m.requireAgent() {
			return m, nil
		}
		if m.store != nil && m.sessionID == "" {
			m.ensureSession()
		}
		m.busy = true
		m.append(dimStyle.Render("◎ compacting…"))
		p := m.prog
		ag := m.agent // capture the current conversation for the summary call
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		go func() {
			took := len(ag.Messages)
			var summary string
			err := ag.ManualCompact(ctx, agent.Events{
				OnCompactionReady: func(messages []models.Message, summary string, cutoff int) error {
					return m.store.PersistCompaction(m.sessionID, m.saved, messages, m.modelName, m.provName, summary, cutoff)
				},
				OnCompacted: func(s string, _ int) { summary = s },
			})
			if p != nil { // nil in headless tests; compaction still ran
				p.Send(compactMsg{took: took - len(ag.Messages), kept: len(ag.Messages), summary: summary, err: err})
				p.Send(turnDoneMsg{}) // clear busy state
			}
		}()
		return m, m.spin.Tick
	case "/mcp":
		return m.mcpCommand(fields)
	case "/lsp":
		return m.lspCommand(fields)
	case "/cd":
		m.cdCommand(strings.TrimSpace(strings.TrimPrefix(text, "/cd")))
		return m, nil
	case "/pwd":
		m.append(dimStyle.Render(cwd()))
		return m, nil
	case "/tasks":
		if len(fields) > 1 { // /tasks <id>: jump straight into the detail view
			m.openTask(fields[1])
			return m, nil
		}
		// bare /tasks focuses the dock if it exists, else prints the list
		if len(m.dockTasks()) > 0 {
			m.tasksFocus = true
			m.clampTaskSel(-1)
			return m, nil
		}
		m.append(m.tasksView())
		return m, nil
	case "/effort":
		if len(fields) > 1 {
			if !m.requireAgent() {
				return m, nil
			}
			levels := m.effortsFor()
			lv, ok := parseEffort(levels, fields[1])
			if !ok {
				names := make([]string, len(levels))
				for i, e := range levels {
					names[i] = effortLabel(e)
				}
				m.append(errStyle.Render("unknown effort level; " + m.currentModelID() + " supports: " + strings.Join(names, ", ")))
				break
			}
			m.setEffort(lv)
		} else {
			m.openPaletteOn("reasoning effort") // bare: open the level selector
		}
	case "/goal-from-context":
		if !m.requireAgent() {
			return m, nil
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /goal-from-context after this turn)"))
			return m, nil
		}
		window := agent.GoalFromContextDefaultWindow
		if len(fields) > 1 {
			n, err := strconv.Atoi(fields[1])
			if err != nil || n < 2 {
				m.append(errStyle.Render("usage: /goal-from-context [n] — n ≥ 2 messages of context (default " + strconv.Itoa(agent.GoalFromContextDefaultWindow) + ")"))
				return m, nil
			}
			window = n
		}
		if m.workerOnly {
			if m.workerClient == nil && !m.ensureWorker() {
				m.append(errStyle.Render("goal-from-context: worker unavailable: " + m.workerStartError))
				return m, nil
			}
			m.busy = true
			m.turnStart = m.nowFn()
			m.append(dimStyle.Render(fmt.Sprintf("◎ formulating goal from the last %d messages…", window)))
			requestID := workerRequestID("goal-from-context")
			m.cancel = func() {
				if m.workerClient != nil {
					_ = m.workerClient.Send(workerwire.CommandCancel, requestID+"-cancel", nil)
				}
			}
			if err := m.workerClient.Send(workerwire.CommandGoalFromContext, requestID, workerwire.GoalFromContextRequest{Window: window}); err != nil {
				m.busy = false
				m.cancel = nil
				m.append(errStyle.Render("goal-from-context failed: " + err.Error()))
			}
			return m, m.spin.Tick
		}
		tail, err := agent.GoalFromContextMessages(m.agent.Messages, window)
		if err != nil {
			m.append(errStyle.Render(err.Error()))
			return m, nil
		}
		// one non-streaming call on the CURRENT model (the compact-model
		// override is deliberately ignored) distills the tail into a goal
		m.busy = true
		m.append(dimStyle.Render(fmt.Sprintf("◎ formulating goal from the last %d messages…", len(tail))))
		p := m.prog
		// ag may drift from m.agent if the user /model-switches mid-formulation:
		// usage lands on the old agent, the goal submits on the new one. The
		// call itself is safe (Complete touches no Agent state, AddUsage is
		// mutex-protected) and the window is seconds — not worth a guard.
		ag := m.agent
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		prompt := agent.BuildGoalFromContextPrompt(tail)
		formulate := func() (string, error) {
			reasoningEffort, reasoningEnabled := ag.ReasoningRequest()
			message, usage, err := ag.CompleteWithRoute(ctx, ag.Backend, ag.Role, ag.Provider, ag.Protocol, models.Request{
				Model:            ag.Model,
				MaxTokens:        8192,
				Messages:         []models.Message{{Role: "user", Content: prompt}},
				ReasoningEffort:  reasoningEffort,
				ReasoningEnabled: reasoningEnabled,
			}, agent.Events{})
			ag.AddUsage(usage) // the formulation call is session spend too
			return message.TextContent(), err
		}
		if p == nil {
			// headless (tests): run inline on the caller's goroutine — with
			// no program to pump messages the Update handler can't run, so
			// apply the same notes/goal here; the goal loop itself never
			// starts without a running program
			goal, err := formulate()
			m.busy = false
			m.cancel = nil
			switch {
			case err != nil && err != context.Canceled:
				m.append(errStyle.Render("goal-from-context failed: " + err.Error()))
			case err == nil && strings.TrimSpace(goal) == "":
				m.append(errStyle.Render("goal-from-context: model returned an empty goal"))
			case err == nil:
				m.setGoal(strings.TrimSpace(goal))
				m.append(dimStyle.Render("◎ goal set: " + m.goal))
			}
			return m, nil
		}
		go func() {
			goal, err := formulate()
			// the msg handler owns busy/cancel: on success it submits (busy
			// belongs to the new turn), on failure it clears them directly —
			// a turnDoneMsg{} here would either cancel-proof the fresh turn
			// (success) or re-engage a paused goal's loop (failure)
			p.Send(goalFromContextMsg{goal: goal, err: err})
		}()
		return m, m.spin.Tick
	case "/plan":
		return m.planCommand(text)
	case "/execute":
		return m.executeCommand(text)
	case "/review":
		return m.reviewCommand(text)
	case "/export", "/export-result":
		return m.exportResultCommand(text)
	case "/export-chat", "/export-log":
		args := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "/export-chat"), "/export-log"))
		return m.exportResultCommand("/export-result chat " + args)
	case "/goal":
		switch {
		case len(fields) == 1:
			record, ok := m.goalRecordForSession()
			if !ok {
				m.append(dimStyle.Render("no goal set — /goal <text> to set one"))
			} else {
				m.append(dimStyle.Render(fmt.Sprintf("◎ goal %s (%s, round %d/%d): %s", record.ID, record.Status, record.Rounds, m.goalMaxRounds(), record.Objective)))
				if record.Progress != "" {
					m.append(dimStyle.Render("  progress: " + record.Progress))
				}
				if record.Blocker != "" {
					m.append(dimStyle.Render("  blocker: " + record.Blocker))
				}
			}
		case fields[1] == "clear":
			m.setGoal("")
			m.append(dimStyle.Render("(goal cleared)"))
		case fields[1] == "rounds":
			m.goalRoundsCommand(fields[2:])
		case fields[1] == "resume":
			if !m.requireAgent() {
				break
			}
			if !m.resumeGoal() {
				break
			}
			record, _ := m.goalRecordForSession()
			return m.submitGoal(goal.ContinuePrompt(record.Objective))
		default:
			if !m.requireAgent() {
				break
			}
			goal := strings.TrimSpace(strings.TrimPrefix(text, "/goal"))
			m.setGoal(goal)
			m.append(dimStyle.Render("◎ goal set: " + goal))
			return m.submit(goal)
		}
	case "/fork":
		if m.busy {
			m.append(dimStyle.Render("(busy — /fork after this turn)"))
			return m, nil
		}
		m.forkCommand(strings.TrimSpace(strings.TrimPrefix(text, "/fork")))
		return m, nil
	case "/rename":
		if m.busy {
			m.append(dimStyle.Render("(busy — /rename after this turn)"))
			return m, nil
		}
		m.renameCommand(strings.TrimSpace(strings.TrimPrefix(text, "/rename")))
		return m, nil
	case "/resume":
		if !m.requireAgent() {
			break
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /resume after this turn)"))
			return m, nil
		}
		if len(fields) > 1 {
			if err := m.resume(fields[1]); err != nil {
				m.append(errStyle.Render(err.Error()))
			}
			break
		}
		m.openPicker()
	case "/context-doctor":
		if m.workerOnly {
			if m.workerClient == nil && !m.ensureWorker() {
				m.append(errStyle.Render("context doctor: worker unavailable: " + m.workerStartError))
				return m, nil
			}
			if err := m.workerClient.Send(workerwire.CommandContextDoctor, workerRequestID("doctor"), nil); err != nil {
				m.append(errStyle.Render("context doctor failed: " + err.Error()))
			}
			return m, nil
		}
		m.append(m.doctorReport())
	case "/report":
		m.append(m.reportBlock())
	case "/help":
		m.append(dimStyle.Render(helpText()))
	case "/auth":
		m.authCommand(fields[1:])
	case "/model":
		if len(fields) < 2 {
			m.openModelPicker()
			break
		}
		if fields[1] == "refresh" {
			m.append(dimStyle.Render("refreshing model catalogs…"))
			providers := maps.Clone(m.cfg.Providers)
			go func() {
				m.fetchCatalogs(true, providers)
				if m.prog != nil {
					m.prog.Send(noticeMsg("model catalogs refreshed — /model shows newly announced models"))
				}
			}()
			break
		}
		prov := ""
		if len(fields) > 2 {
			prov = fields[2]
		}
		name := fields[1]
		resolved, ok, alts := resolveModelFuzzy(m.cfg, name)
		if !ok {
			if len(alts) > 0 {
				m.append(errStyle.Render(fmt.Sprintf("ambiguous model %q — did you mean: %s?", name, strings.Join(alts, ", "))))
				return m, nil
			}
			m.append(errStyle.Render("unknown model " + name))
			return m, nil
		}
		m.switchModel(resolved, prov)
	default:
		m.append(errStyle.Render("unknown command " + fields[0]))
	}
	return m, nil
}

// degradedProviderNote is the short actionable message shown when the TUI can
// open but no usable provider credential is available.
func (m *model) degradedProviderNote() string {
	return "No provider has been configured — run /auth"
}

// requireAgent keeps agent-dependent commands harmless during the cold TUI
// state. The note is deliberately the same onboarding hint shown at startup.
func (m *model) requireAgent() bool {
	if m.workerOnly {
		if m.modelName != "" && m.provName != "" {
			return true
		}
		m.append(m.degradedProviderNote())
		return false
	}
	if m.agent != nil {
		return true
	}
	m.append(m.degradedProviderNote())
	return false
}
