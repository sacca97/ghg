package tui

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/tools"
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

// tuiPresentation carries the TUI-only attributes of a catalogue command:
// which help group it belongs to, its keybinding, and whether it must run
// immediately while a turn is in flight. Names and hints come from the shared
// worker catalogue so both clients render identical text.
var tuiPresentation = map[string]registryEntry{
	"!cmd":               {Category: "App"},
	"/ask":               {Category: "Agent", Immediate: true},
	"/approval":          {Category: "Session", Immediate: true},
	"/auth":              {Category: "Agent", Immediate: true},
	"/cd":                {Category: "Session", Immediate: true},
	"/clear":             {Category: "Session", Immediate: true},
	"/compact":           {Category: "Session", Immediate: true},
	"/context-doctor":    {Category: "Session", Immediate: true},
	"/continue":          {Category: "Session", Immediate: true},
	"/detach":            {Keybind: "ctrl+d", Category: "Session", Immediate: true},
	"/dynamic-reasoning": {Category: "Agent", Immediate: true},
	"/effort":            {Category: "Agent", Immediate: true},
	"/execute":           {Category: "Agent", Immediate: true},
	"/export":            {Category: "Session"},
	"/fork":              {Category: "Session"},
	"/goal":              {Category: "Session", Immediate: true},
	"/goal-from-context": {Category: "Session", Immediate: true},
	"/help":              {Category: "App", Immediate: true},
	"/lsp":               {Category: "Session", Immediate: true},
	"/mcp":               {Category: "Session", Immediate: true},
	"/me":                {Category: "Agent", Immediate: true},
	"/memory":            {Category: "Session"},
	"/model":             {Category: "Agent"},
	"/notify":            {Category: "Session", Immediate: true},
	"/plan":              {Category: "Agent", Immediate: true},
	"/pwd":               {Category: "Session", Immediate: true},
	"/quit":              {Keybind: "ctrl+c ctrl+c", Category: "App"},
	"/rename":            {Category: "Session", Immediate: true},
	"/report":            {Category: "App", Immediate: true},
	"/resume":            {Category: "Session"},
	"/review":            {Category: "Agent", Immediate: true},
	"/schedule":          {Category: "Session"},
	"/search-providers":  {Category: "Session", Immediate: true},
	"/tasks":             {Keybind: "ctrl+t", Category: "Session", Immediate: true},
}

// registry lists every user-facing command, derived from the canonical worker
// catalogue plus the TUI-only presentation attributes above. Help, completion,
// and the palette all read this table, so a command can never be dispatchable
// but undocumented (or documented but dead).
var registry = buildRegistry()

func buildRegistry() []registryEntry {
	specs := workerwire.Commands()
	out := make([]registryEntry, 0, len(specs))
	for _, spec := range specs {
		entry := registryEntry{Name: spec.Name, Hint: spec.Hint}
		if extra, ok := tuiPresentation[spec.Name]; ok {
			entry.Keybind, entry.Category, entry.Immediate = extra.Keybind, extra.Category, extra.Immediate
		}
		out = append(out, entry)
	}
	return out
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
	name = workerwire.CommandName(name)
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
		b.WriteString(e.Name)
		b.WriteByte(' ')
		b.WriteString(e.Hint)
		b.WriteByte('\n')
	}
	b.WriteString(palHintRewind)
	b.WriteString(" — ")
	b.WriteString(palDescRewind)
	b.WriteByte('\n')
	b.WriteString("!cmd ")
	b.WriteString(registryFind("!cmd").Hint)
	b.WriteByte('\n')
	b.WriteString("tab — complete")
	for _, hint := range []string{
		"ctrl+k — clear the conversation",
		"ctrl+t — focus the subagents dock (↑/↓ select, enter opens, esc backs out)",
		"ctrl+d — stop the worker and exit (resume later)",
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
		b.WriteString(" · ")
		b.WriteString(hint)
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
	name := workerwire.CommandName(fields[0])
	if name == "/goal" { // status and clear are settings; resume/<text> submit turns
		return len(fields) == 1 || fields[1] == "clear" || fields[1] == "rounds"
	}
	return registryImmediate(name)
}

func (m *model) currentApprovalMode() string {
	if m.approval != "" {
		return m.approval
	}
	if m.cfg != nil && m.cfg.Execution != nil && strings.TrimSpace(m.cfg.Execution.Approval) != "" {
		return strings.TrimSpace(m.cfg.Execution.Approval)
	}
	return string(tools.ApprovalAsk)
}

func (m *model) command(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return m, nil
	}
	command := workerwire.CommandName(fields[0])
	switch command {
	case "/quit":
		return m, tea.Quit
	case "/detach":
		live := m.busy || m.workerState == workerwire.StateRunning || m.workerState == workerwire.StateWaitingApproval || m.workerState == workerwire.StateWaitingQuestion || m.workerLiveWork
		if m.workerClient == nil || !live {
			m.append(dimStyle.Render("(nothing running to stop)"))
			return m, nil
		}
		if m.workerStopRequestID != "" {
			return m, nil
		}
		requestID := workerRequestID("stop")
		if err := m.workerClient.Send(workerwire.CommandStop, requestID, nil); err != nil {
			m.append(errStyle.Render("stop failed: " + err.Error()))
			return m, nil
		}
		m.workerStopRequestID = requestID
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
	case "/continue":
		if m.busy {
			m.append(dimStyle.Render("(busy — /continue after this turn)"))
			return m, nil
		}
		extra := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
		return m.submitContinue(extra)
	case "/approval":
		if len(fields) == 1 {
			m.append(dimStyle.Render("approval mode: " + m.currentApprovalMode()))
			return m, nil
		}
		if len(fields) != 2 {
			m.append(errStyle.Render("usage: /approval [ask|auto|never]"))
			return m, nil
		}
		mode, err := tools.ParseApprovalMode(fields[1])
		if err != nil {
			m.append(errStyle.Render(err.Error()))
			return m, nil
		}
		if m.cfg == nil {
			m.append(errStyle.Render("approval: config unavailable"))
			return m, nil
		}
		if err := m.cfg.ApplyExecutionOverrides("", "", string(mode)); err != nil {
			m.append(errStyle.Render("approval: " + err.Error()))
			return m, nil
		}
		if err := m.saveConfig(); err != nil {
			return m, nil
		}
		m.approval = string(mode)
		if m.workerClient == nil {
			m.append(dimStyle.Render("approval mode saved: " + string(mode) + " (applies to the next worker)"))
			return m, nil
		}
		if err := m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("approval"), workerwire.ConfigureRequest{Approval: string(mode)}); err != nil {
			m.append(errStyle.Render("approval update failed: " + err.Error()))
			return m, nil
		}
		m.append(dimStyle.Render("approval mode: " + string(mode)))
		return m, nil
	case "/memory":
		m.memoryCommand(fields[1:])
	case "/schedule":
		m.scheduleCommand(fields[1:])
	case "/me":
		return m, m.openMe()
	case "/compact":
		if len(fields) == 1 {
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
			m.append(errStyle.Render("/compact does not accept a model; it always uses tiny → fast → default → smart"))
			return m, nil
		}
	case "/notify":
		if len(fields) > 2 || (len(fields) == 2 && fields[1] != "on" && fields[1] != "off" && fields[1] != "config") {
			m.append(errStyle.Render("usage: /notify [config|on|off]"))
			return m, nil
		}
		if len(fields) == 2 && fields[1] == "config" {
			m.notifyConfigCommand()
			return m, nil
		}
		action := "status"
		if len(fields) == 2 {
			action = fields[1]
		}
		m.sendWorkerCommand("/notify", "notify", workerwire.CommandNotify, workerwire.NotifyRequest{Action: action})
		return m, nil
	case "/search-providers":
		m.searchProvidersCommand(fields[1:])
		return m, nil
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
			m.clampTaskSel()
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
	case "/dynamic-reasoning":
		if len(fields) != 1 {
			m.append(errStyle.Render("usage: /dynamic-reasoning"))
			return m, nil
		}
		m.setDynamicReasoning(!m.dynamicReasoningEnabled())
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
	case "/plan":
		return m.planCommand(text)
	case "/execute":
		return m.executeCommand(text)
	case "/review":
		return m.reviewCommand(text)
	case "/export":
		if fields[0] == "/export-chat" || fields[0] == "/export-log" {
			text = "/export chat " + strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		}
		return m.exportResultCommand(text)
	case "/goal":
		switch {
		case len(fields) == 1:
			record, ok := m.goalRecordForSession()
			if !ok {
				m.append(dimStyle.Render("no goal set — /goal <text> to set one"))
			} else {
				m.append(dimStyle.Render(fmt.Sprintf("◎ goal %s (%s, round %d, unbounded): %s", record.ID, record.Status, record.Rounds, record.Objective)))
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
			m.append(dimStyle.Render("goal runs are unbounded; /goal rounds is no longer used"))
		case fields[1] == "resume":
			if !m.requireAgent() {
				break
			}
			if !m.resumeGoal() {
				break
			}
			record, _ := m.goalRecordForSession()
			return m.submitGoal(agent.ContinuePrompt(record.Objective))
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
			if m.prog != nil {
				return m, m.resumeCmd(fields[1])
			}
			if err := m.resume(fields[1]); err != nil {
				m.append(errStyle.Render(err.Error()))
			}
			break
		}
		if m.prog != nil {
			return m, m.openPickerCmd()
		}
		m.openPicker()
	case "/context-doctor":
		m.sendWorkerCommand("/context-doctor", "doctor", workerwire.CommandContextDoctor, nil)
		return m, nil
	case "/report":
		m.append(m.reportBlock())
	case "/help":
		m.append(dimStyle.Render(helpText()))
	case "/auth":
		m.authCommand(fields[1:])
	case "/ask":
		return m.askCommand(text)
	case "/model":
		if len(fields) < 2 {
			m.openModelPicker()
			break
		}
		if fields[1] == "refresh" {
			m.append(dimStyle.Render("refreshing model catalogs…"))
			cfg := *m.cfg
			cfg.Providers = maps.Clone(m.cfg.Providers)
			profiles := m.profiles
			p := m.prog
			go func() {
				sendProg(p, fetchCatalogs(true, cfg, profiles))
				sendProg(p, noticeMsg("model catalogs refreshed — /model shows newly announced models"))
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
	if m.modelName == "" {
		return "No model has been configured — choose one with /model"
	}
	return "No provider has been configured — run /auth"
}

// requireAgent keeps agent-dependent commands harmless during the cold TUI
// state. The note is deliberately the same onboarding hint shown at startup.
func (m *model) requireAgent() bool {
	if m.modelName != "" && m.provName != "" {
		return true
	}
	m.append(m.degradedProviderNote())
	return false
}

// lspCommand handles "/lsp" — requests status from the worker.
func (m *model) lspCommand([]string) (tea.Model, tea.Cmd) {
	m.sendWorkerCommand("/lsp", "lsp", workerwire.CommandLSPStatus, nil)
	return m, nil
}

func (m *model) renderLSPStatuses(servers []lsp.Status) {
	if len(servers) == 0 {
		m.append(dimStyle.Render("no LSP servers"))
		return
	}
	var b strings.Builder
	b.WriteString("LSP servers:\n")
	for _, s := range servers {
		icon := "○"
		detail := "idle — starts on first matching file"
		switch s.State {
		case "connected":
			icon = "●"
			detail = "connected"
			if s.Root != "" {
				detail += " (root: " + s.Root + ")"
			}
		case "failed":
			icon = "✗"
			detail = s.Err
		}
		line := fmt.Sprintf("  %s %-16s %s", icon, s.Name, detail)
		switch s.State {
		case "failed":
			b.WriteString(errStyle.Render(line))
			b.WriteByte('\n')
		case "not started":
			b.WriteString(dimStyle.Render(line))
			b.WriteByte('\n')
		default:
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	m.append(strings.TrimRight(b.String(), "\n"))
}

func workerLSPStatuses(statuses []workerwire.LSPStatus) []lsp.Status {
	converted := make([]lsp.Status, len(statuses))
	for i, status := range statuses {
		converted[i] = lsp.Status{Name: status.Name, Root: status.Root, State: status.State, Err: status.Error}
	}
	return converted
}

// mcpCommand handles "/mcp [name] [reconnect|enable|disable]".
func (m *model) mcpCommand(fields []string) (tea.Model, tea.Cmd) {
	if len(fields) == 1 {
		m.sendWorkerCommand("/mcp", "mcp", workerwire.CommandMCPStatus, nil)
		return m, nil
	}
	name := fields[1]
	action := workerwire.CommandMCPReconnect
	if len(fields) > 2 {
		switch fields[2] {
		case "reconnect":
			action = workerwire.CommandMCPReconnect
		case "enable":
			action = workerwire.CommandMCPEnable
		case "disable":
			action = workerwire.CommandMCPDisable
		default:
			m.append(errStyle.Render("usage: /mcp [name] [reconnect|enable|disable]"))
			return m, nil
		}
	}
	m.sendWorkerCommand("/mcp", "mcp", action, workerwire.MCPRequest{Name: name})
	return m, nil
}

func renderMCPStatuses(servers []mcp.Server) string {
	if len(servers) == 0 {
		return dimStyle.Render("no MCP servers")
	}
	var b strings.Builder
	b.WriteString("MCP servers:\n")
	for _, s := range servers {
		icon := "◌"
		detail := ""
		switch s.Status {
		case mcp.StatusReady:
			icon = "●"
			detail = fmt.Sprintf("%d tools", s.Tools)
		case mcp.StatusFailed:
			icon = "✗"
			detail = s.Err
			if s.Source != "" {
				detail += " (" + s.Source + ")"
			}
		case mcp.StatusDisabled:
			icon = "○"
			detail = "disabled"
			if s.Note != "" {
				detail = "disabled — " + s.Note
			}
		case mcp.StatusConnecting:
			icon = "◌"
			detail = "connecting…"
		}
		line := fmt.Sprintf("  %s %-20s %s", icon, s.Name, detail)
		switch s.Status {
		case mcp.StatusReady:
			b.WriteString(line)
			b.WriteByte('\n')
		case mcp.StatusFailed:
			b.WriteString(errStyle.Render(line))
			b.WriteByte('\n')
		default:
			b.WriteString(dimStyle.Render(line))
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderWorkerMCPStatuses(statuses []workerwire.MCPStatus) string {
	servers := make([]mcp.Server, len(statuses))
	for i, status := range statuses {
		servers[i] = mcp.Server{Name: status.Name, Note: status.Note, Err: status.Error, Tools: status.Tools, Source: status.Source}
		switch status.State {
		case "ready":
			servers[i].Status = mcp.StatusReady
		case "failed":
			servers[i].Status = mcp.StatusFailed
		case "disabled":
			servers[i].Status = mcp.StatusDisabled
		default:
			servers[i].Status = mcp.StatusConnecting
		}
	}
	return renderMCPStatuses(servers)
}
