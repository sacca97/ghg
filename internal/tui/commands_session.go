package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/export"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/schedule"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (m *model) cdCommand(arg string) {
	if arg == "" {
		m.append(dimStyle.Render(cwd()))
		return
	}
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			m.append(errStyle.Render("/cd: " + err.Error()))
			return
		}
		arg = home + arg[1:]
	}
	if m.workerOnly {
		if m.busy || m.workerLiveWork {
			m.append(dimStyle.Render("(worker is busy — /cd after this turn)"))
			return
		}
		if m.workerClient == nil && !m.ensureWorker() {
			m.append(errStyle.Render("/cd: worker unavailable: " + m.workerStartError))
			return
		}
		requestID := workerRequestID("chdir")
		if err := m.workerClient.Send(workerwire.CommandChdir, requestID, arg); err != nil {
			m.append(errStyle.Render("/cd: worker: " + err.Error()))
			return
		}
		m.workerChdirRequest = requestID
		return
	}
	if m.workerClient != nil {
		if m.busy || m.workerLiveWork {
			m.append(dimStyle.Render("(worker is busy — /cd after this turn)"))
			return
		}
		requestID := workerRequestID("chdir")
		if err := m.workerClient.Send(workerwire.CommandChdir, requestID, arg); err != nil {
			m.append(errStyle.Render("/cd: worker: " + err.Error()))
			return
		}
		m.workerChdirRequest = requestID
		return
	}
	if err := os.Chdir(arg); err != nil {
		m.append(errStyle.Render("/cd: " + err.Error()))
		return
	}
	m.append(dimStyle.Render("→ " + cwd()))
}

// Compaction events are recorded in raw-log coordinates (session.Store.RawCutoff
// does the translation) so Load never double-folds a summary. The inspection
// surface below is what makes a bad summary erasable.

// /compact retry — drop the latest compaction event and re-compact from the
// raw log. This is the whole point of recording compactions as events: a bad
// summary is inspectable (/compact log) and erasable without losing history.
func (m *model) compactRetry() {
	if m.workerOnly {
		if m.store == nil || m.sessionID == "" {
			m.append(dimStyle.Render("(no session to retry a compaction in)"))
			return
		}
		if m.workerClient == nil && !m.ensureWorker() {
			m.append(errStyle.Render("compact retry: worker unavailable: " + m.workerStartError))
			return
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /compact retry after this turn)"))
			return
		}
		requestID := workerRequestID("compact-retry")
		m.workerHistoryRequest = requestID
		if err := m.workerClient.Send(workerwire.CommandCompactRetry, requestID, nil); err != nil {
			m.workerHistoryRequest = ""
			m.append(errStyle.Render("/compact retry: " + err.Error()))
			return
		}
		m.append(dimStyle.Render("⟲ retrying compaction…"))
		return
	}
	if m.agent == nil {
		m.append(m.degradedProviderNote())
		return
	}
	if m.store == nil || m.sessionID == "" {
		m.append(dimStyle.Render("(no session to retry a compaction in)"))
		return
	}
	events := m.store.Compactions(m.sessionID)
	if len(events) == 0 {
		m.append(dimStyle.Render("(no compaction to retry)"))
		return
	}
	last := events[len(events)-1]
	if err := m.store.DeleteCompaction(m.sessionID, last.Seq); err != nil {
		m.append(errStyle.Render("/compact retry: " + err.Error()))
		return
	}
	m.append(dimStyle.Render("⟲ compaction " + strconv.Itoa(last.Seq) + " undone — raw history restored; run /compact to re-compact"))
	// rebuild the in-memory conversation from the raw log so the next
	// compaction (or turn) starts from the unfolded history
	_, msgs, err := m.store.Load(m.sessionID)
	if err != nil {
		m.append(errStyle.Render("/compact retry: reload failed: " + err.Error()))
		return
	}
	m.agent.Messages = append(m.agent.Messages[:1], msgs[1:]...)
	m.saved = 1 // re-save from scratch next persist
	m.rebuildTranscript()
}

// /compact log — the recorded compaction events (the inspection surface).
func (m *model) compactLog() {
	if m.store == nil || m.sessionID == "" {
		m.append(dimStyle.Render("(no session)"))
		return
	}
	events := m.store.Compactions(m.sessionID)
	if len(events) == 0 {
		m.append(dimStyle.Render("(no compactions recorded)"))
		return
	}
	var b strings.Builder
	b.WriteString(dimStyle.Render("compactions — raw history preserved; /compact retry undoes the latest:"))
	for _, c := range events {
		summary := strings.Join(strings.Fields(c.Summary), " ")
		if len(summary) > 80 {
			summary = summary[:80] + "…"
		}
		b.WriteString("\n  " + dimStyle.Render("#"+strconv.Itoa(c.Seq)+" folded through message "+strconv.Itoa(c.Cutoff)+": ") + summary)
	}
	m.append(b.String())
}

// /context-doctor — audit what a FRESH session injects before
// the user types anything, and what each piece costs in estimated tokens.
// The audience is someone arriving from claude/codex whose first call carries
// tens of thousands of tokens of skill/MCP/tool-schema bloat they never asked
// for; the doctor names every source and its cost so it can be audited (and
// trimmed) instead of silently paid.

// ctxRow is one line of the audit.
type ctxRow struct {
	label string
	bytes int
	note  string
}

func (r ctxRow) tokens() int { return (r.bytes + 3) / 4 }

// doctorReport builds the audit as pure data (testable), then renders.
func (m *model) doctorReport() string {
	var rows []ctxRow

	// Base system prompt; skills/MCP blocks are appended per turn in
	// prepareTurn. Project instructions are called out separately so the audit
	// identifies the trusted repository input instead of hiding it in the base
	// total.
	baseBytes := len(m.sysPrompt)
	if wd, err := os.Getwd(); err == nil {
		if project := config.ProjectInstructions(wd, config.Trusted(wd)); project != "" {
			if strings.Contains(m.sysPrompt, project) && baseBytes >= len(project)+2 {
				baseBytes -= len(project) + 2 // systemPrompt joins blocks with two newlines
			}
			rows = append(rows, ctxRow{"project instructions (AGENTS.md)", len(project), "trusted project"})
		}
	}
	rows = append(rows, ctxRow{"system prompt (base)", baseBytes, ""})

	// Skills: block total + the worst offenders, each named with the directory
	// it was discovered from — "where does this skill come from?" should be
	// answerable here, not by hunting ~/.ghg/skills vs .agents/skills.
	scan := m.skillScan
	if scan == nil { // headless tests build models without the seam
		scan = func() []skills.Skill { return skills.Scan(skills.DefaultDirs()...) }
	}
	sk := scan()
	block := skills.PromptBlock(sk)
	row := ctxRow{fmt.Sprintf("skills (%d loaded)", len(sk)), len(block), ""}
	// Per-skill line cost in the block: "- name: desc (path)\n".
	type sc struct {
		name string
		dir  string // the skills dir the SKILL.md lives under
		n    int
	}
	var per []sc
	for _, s := range sk {
		n := len(s.Name) + min(len(s.Description), 300) + len(s.Path) + 8
		per = append(per, sc{s.Name, filepath.Dir(filepath.Dir(s.Path)), n})
	}
	sort.Slice(per, func(i, j int) bool { return per[i].n > per[j].n })
	var top []string
	for i := 0; i < len(per) && i < 5; i++ {
		top = append(top, fmt.Sprintf("%s ~%dtok (%s)", per[i].name, (per[i].n+3)/4, shortSkillsDir(per[i].dir)))
	}
	if len(top) > 0 {
		row.note = "biggest: " + strings.Join(top, ", ")
	}
	rows = append(rows, row)

	// MCP: per-server tool schemas as they'd appear in the request.
	if m.mcpMgr != nil {
		toolBytes := map[string]int{}
		for _, t := range m.mcpMgr.Tools() {
			n := t.Def.Function.Name
			srv := n
			if i := strings.Index(strings.TrimPrefix(n, "mcp__"), "__"); i >= 0 {
				srv = strings.TrimPrefix(n, "mcp__")[:i]
			}
			schema, _ := json.Marshal(t.Def)
			toolBytes[srv] += len(schema) + len(n) + 8
		}
		for _, st := range m.mcpMgr.Statuses() {
			switch st.Status {
			case mcp.StatusReady:
				b := toolBytes[st.Name]
				rows = append(rows, ctxRow{fmt.Sprintf("mcp: %s (%d tools)", st.Name, st.Tools), b, ""})
			case mcp.StatusFailed:
				rows = append(rows, ctxRow{"mcp: " + st.Name, 0, "failed — contributes 0 tools"})
			case mcp.StatusDisabled:
				rows = append(rows, ctxRow{"mcp: " + st.Name, 0, "disabled"})
			default:
				rows = append(rows, ctxRow{"mcp: " + st.Name, 0, "still connecting — 0 tools yet"})
			}
		}
		if ib := m.mcpMgr.InstructionsBlock(); ib != "" {
			rows = append(rows, ctxRow{"mcp: server instructions", len(ib), ""})
		}
	}

	// Built-in tool schemas (what the provider is sent every request).
	var tb int
	var toolCount int
	if m.agent != nil {
		toolCount = len(m.agent.AllTools())
		for _, t := range m.agent.AllTools() {
			schema, _ := json.Marshal(t.Def)
			tb += len(schema) + 8
		}
	}
	note := "sent with every request"
	if m.agent == nil {
		note = "unavailable until a provider is configured"
	}
	rows = append(rows, ctxRow{fmt.Sprintf("tool schemas (%d tools)", toolCount), tb, note})

	// History: tokens already in the conversation (0 on a fresh session).
	var hist int
	if m.agent != nil {
		hist = agent.EstimateTokens(m.agent.Messages)
	}
	if hist > 0 {
		rows = append(rows, ctxRow{"conversation history", hist * 4, "estimated"})
	}
	// Session spend so far (real usage, if any request has happened).
	if m.agent != nil {
		if u := m.agent.Usage(); u.PromptTokens > 0 {
			rows = append(rows, ctxRow{"session spend so far", 0, fmt.Sprintf("%s in / %s out (actual)", tok(u.PromptTokens), tok(u.CompletionTokens))})
		}
	}

	// Render.
	var b strings.Builder
	b.WriteString("Fresh-session context audit (estimated tokens)\n")
	total := 0
	w := 0
	for _, r := range rows {
		if len(r.label) > w {
			w = len(r.label)
		}
		total += r.tokens()
	}
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s %7s", w, r.label, "~"+tok(r.tokens()))
		if r.note != "" {
			line += "  " + r.note
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "  %-*s %7s\n", w, "TOTAL injected before you type", "~"+tok(total))
	if m.runtime != nil && m.runtime.Policy != nil {
		status := m.runtime.Policy.Status()
		fmt.Fprintf(&b, "\nExecution policy: %s · backend: %s · network: %s\n", status.Mode, status.Backend, status.Network)
		fmt.Fprintf(&b, "  workspace: %s\n", status.Workspace)
		fmt.Fprintf(&b, "  read roots: %s\n", strings.Join(status.ReadRoots, ", "))
		fmt.Fprintf(&b, "  write roots: %s\n", strings.Join(status.WriteRoots, ", "))
		fmt.Fprintf(&b, "  immutable roots: %s\n", strings.Join(status.ImmutableRoots, ", "))
		fmt.Fprintf(&b, "  protected roots: %s\n", strings.Join(status.ProtectedRoots, ", "))
		if status.Degraded {
			fmt.Fprintf(&b, "  degraded: %s\n", status.Reason)
		}
		for _, audit := range m.runtime.Audits() {
			if audit.Error == "" {
				continue
			}
			fmt.Fprintf(&b, "  recent denial: %s (%s)\n", audit.Error, audit.Request.Fingerprint)
		}
	}
	b.WriteString("\nTrim: /mcp <name> disable · remove a skill from .agents/skills · /context-doctor again")
	return b.String()
}

// shortSkillsDir compacts a skills directory for the doctor's per-skill
// attribution: home-relative ("~/.ghg/skills") when under the user's home,
// cwd-relative ("./.agents/skills") when under the working directory,
// absolute otherwise.
func shortSkillsDir(dir string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return "~"
			}
			return "~" + string(filepath.Separator) + rel
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, dir); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "."
			}
			return "." + string(filepath.Separator) + rel
		}
	}
	return dir
}

// tok renders a token count compactly (1.2k, 350).
func tok(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// /memory — the visible half of the memory feature: the user sees exactly
// what gets injected each turn and can kill any line without leaving the
// TUI. Both scopes render numbered; /memory <n> [session] marks an entry
// done. The files stay plain markdown the user can also edit by hand.
func (m *model) memoryCommand(args []string) {
	scopes := []memory.Scope{memory.Installation(), memory.Session(m.sessionID)}

	// /memory <n> [session|installation] — mark entry n done.
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			m.append(errStyle.Render("/memory: entry number expected, got " + args[0]))
			return
		}
		which := "installation"
		if len(args) > 1 {
			which = args[1]
		}
		var s memory.Scope
		switch which {
		case "installation", "install", "global":
			s = scopes[0]
		case "session":
			s = scopes[1]
		default:
			m.append(errStyle.Render("/memory: scope must be installation or session, got " + which))
			return
		}
		if s.Path == "" {
			m.append(dimStyle.Render("(no " + which + " memory scope — start a session first)"))
			return
		}
		if err := s.Forget(n); err != nil {
			m.append(errStyle.Render("/memory: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s memory %d marked done", which, n)))
		return
	}

	// bare /memory — list both scopes, open entries first.
	var b strings.Builder
	b.WriteString(dimStyle.Render("memory — injected into every turn · /memory <n> [session] marks done · edit the files directly:"))
	any := false
	for _, s := range scopes {
		entries := s.Entries()
		if s.Path == "" || len(entries) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(&b, "\n%s (%s)", s.Name, s.Path)
		for _, e := range entries {
			if e.Done {
				fmt.Fprintf(&b, "\n  %s", dimStyle.Render(fmt.Sprintf("%d. ~~%s~~", e.N, e.Text)))
			} else {
				fmt.Fprintf(&b, "\n  %d. %s", e.N, e.Text)
			}
		}
	}
	if !any {
		b.WriteString("\n  (empty — the model saves facts with remember, or write a line like \"- [ ] prefers pnpm\" yourself)")
	}
	m.append(b.String())
}

// /schedule — create, list, cancel. The minimal surface: no editing, no
// pause/resume — delete and re-add.
func (m *model) scheduleCommand(args []string) {
	if len(args) == 0 || args[0] == "list" {
		m.scheduleList()
		return
	}
	switch args[0] {
	case "cancel", "delete", "rm":
		if len(args) < 2 {
			m.append(errStyle.Render("/schedule cancel <n>"))
			return
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || m.store == nil || m.sessionID == "" {
			m.append(errStyle.Render("/schedule cancel: entry number expected"))
			return
		}
		if err := m.store.DeleteSchedule(m.sessionID, n); err != nil {
			m.append(errStyle.Render("/schedule cancel: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("✓ scheduled task %d cancelled", n)))
	default: // /schedule @every 10m <prompt>
		if len(args) < 3 {
			m.append(errStyle.Render("/schedule @every 10m|<@at time> <prompt> — or: list, cancel <n>"))
			return
		}
		expr := strings.Join(args[:2], " ")
		s, err := schedule.Parse(expr)
		if err != nil {
			m.append(errStyle.Render("/schedule: " + err.Error()))
			return
		}
		if m.store == nil {
			m.append(errStyle.Render("/schedule: no session store"))
			return
		}
		m.persist() // a schedule needs a session row to hang off
		if m.sessionID == "" {
			m.append(errStyle.Render("/schedule: start a session first (send a message)"))
			return
		}
		prompt := strings.Join(args[2:], " ")
		anchor := time.Now()
		if !s.At.IsZero() {
			anchor = s.At
		}
		id, err := m.store.AddSchedule(m.sessionID, s.String(), prompt, anchor)
		if err != nil {
			m.append(errStyle.Render("/schedule: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("✓ scheduled task %d: %s — %s", id, s.String(), prompt)))
		if m.workerOnly && m.workerClient == nil && m.prog != nil && !m.ensureWorker() {
			m.append(errStyle.Render("scheduled task saved; worker unavailable: " + m.workerStartError))
		}
	}
}

func (m *model) scheduleList() {
	if m.store == nil || m.sessionID == "" {
		m.append(dimStyle.Render("(no session — schedules are per-session)"))
		return
	}
	tasks := m.store.Schedules(m.sessionID)
	if len(tasks) == 0 {
		m.append(dimStyle.Render("(no scheduled tasks — /schedule @every 10m <prompt>)"))
		return
	}
	var b strings.Builder
	b.WriteString(dimStyle.Render("scheduled tasks — fire as ⏰ turns · /schedule cancel <n>:"))
	for _, sc := range tasks {
		line := fmt.Sprintf("\n  %d. %s — %s", sc.ID, sc.Schedule, sc.Prompt)
		if s, err := schedule.Parse(sc.Schedule); err == nil && !s.At.IsZero() && !sc.LastFire.IsZero() {
			line += dimStyle.Render(" (fired)")
		}
		b.WriteString(line)
	}
	m.append(b.String())
}

// compactCommand handles "/compact <args…>": off restores the built-in
// default compaction model, "<model> [provider]" selects one (persisted).
func (m *model) compactCommand(args []string) {
	if len(args) == 0 {
		return
	}
	if !m.requireAgent() {
		return
	}
	if m.workerOnly {
		if args[0] == "off" {
			m.compactModel, m.compactProv = "", ""
		} else {
			if _, ok := m.cfg.Models[args[0]]; !ok {
				m.append(errStyle.Render("unknown model " + args[0]))
				return
			}
			m.compactModel = args[0]
			m.compactProv = ""
			if len(args) > 1 {
				m.compactProv = args[1]
			}
		}
		m.cfg.CompactModel, m.cfg.CompactProvider = m.compactModel, m.compactProv
		_ = m.saveConfig()
		if m.workerClient != nil {
			_ = m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("configure"), workerwire.ConfigureRequest{
				Model: m.modelName, Provider: m.provName, Role: m.currentRole(),
				Effort: m.currentEffort(), UpdateEffort: false, Mode: m.uiMode(),
				CompactModel: m.compactModel, CompactProvider: m.compactProv, UpdateCompact: true,
			})
		}
		return
	}
	if args[0] == "off" {
		m.compactModel, m.compactProv = "", ""
		m.applyCompactModel()
		m.cfg.CompactModel, m.cfg.CompactProvider = "", ""
		_ = m.saveConfig()
		return
	}
	if _, ok := m.cfg.Models[args[0]]; !ok {
		m.append(errStyle.Render("unknown model " + args[0]))
		return
	}
	m.compactModel = args[0]
	m.compactProv = ""
	if len(args) > 1 {
		m.compactProv = args[1]
	}
	m.applyCompactModel()
	if m.agent.CompactModel == "" { // resolve failed; don't persist a broken pick
		m.compactModel, m.compactProv = "", ""
		return
	}
	m.cfg.CompactModel, m.cfg.CompactProvider = m.compactModel, m.compactProv
	_ = m.saveConfig()
}

// compactPct returns the live threshold percent (the default when unset).
// cfg.CompactPct is the authoritative value; the agent's float is derived.
func (m *model) compactPct() int {
	if m.cfg == nil {
		return config.DefaultCompactPct
	}
	pct := m.cfg.CompactPct
	if pct == 0 {
		pct = config.DefaultCompactPct
	}
	return min(max(pct, 10), 90)
}

// setCompactPct applies a compaction-threshold percent (clamped 10–90): the
// agent compacts proactively once the estimated context use crosses it.
// Persisted as the new default. settings-driven, so no transcript note — the
// row's [NN%] badge is the feedback (same as the effort steppers).
func (m *model) setCompactPct(pct int) {
	if !m.requireAgent() {
		return
	}
	pct = min(max(pct, 10), 90)
	if m.workerOnly {
		m.cfg.CompactPct = pct
		_ = m.saveConfig()
		if m.workerClient != nil {
			_ = m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("configure"), workerwire.ConfigureRequest{
				Model: m.modelName, Provider: m.provName, Role: m.currentRole(),
				Effort: m.currentEffort(), Mode: m.uiMode(),
				CompactThreshold: float64(pct) / 100, UpdateCompactThreshold: true,
			})
		}
		return
	}
	m.agent.CompactThreshold = float64(pct) / 100
	m.cfg.CompactPct = pct
	_ = m.saveConfig()
}

// exportResultCommand handles `/export` and `/export-result` with `[chat|plan|review|last|message] [dest] [--format json|markdown] [--force]`.
func (m *model) exportResultCommand(text string) (tea.Model, tea.Cmd) {
	trimmed := text
	for _, pfx := range []string{"/export-result", "/export"} {
		if strings.HasPrefix(trimmed, pfx) {
			trimmed = strings.TrimPrefix(trimmed, pfx)
			break
		}
	}
	args := strings.Fields(strings.TrimSpace(trimmed))
	var kind, dest, format string
	var force bool

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force" || arg == "-f":
			force = true
		case arg == "--format" || arg == "-format":
			if i+1 < len(args) {
				i++
				format = args[i]
			}
		case strings.HasPrefix(arg, "--format="):
			format = strings.TrimPrefix(arg, "--format=")
		case arg == "plan" || arg == "review" || arg == "last" || arg == "message" || arg == "response" || arg == "chat" || arg == "log" || arg == "transcript":
			if kind == "" {
				kind = arg
			} else if dest == "" {
				dest = arg
			}
		default:
			if dest == "" {
				dest = arg
			}
		}
	}

	if format == "" {
		if strings.HasSuffix(strings.ToLower(dest), ".json") {
			format = export.FormatJSON
		} else {
			format = export.FormatMarkdown
		}
	}

	ctx := context.Background()
	var rec session.WorkflowResultRecord
	var ok bool

	if kind == "chat" || kind == "log" || kind == "transcript" {
		msgs := m.findChatMessages()
		if len(msgs) == 0 {
			m.append(dimStyle.Render("no chat messages found in this session to export"))
			return m, nil
		}
		rawPayload, _ := json.Marshal(msgs)
		rec = session.WorkflowResultRecord{
			ResultID:  fmt.Sprintf("chat-%x", time.Now().UnixNano()),
			SessionID: m.sessionID,
			Kind:      "chat",
			Version:   1,
			Payload:   string(rawPayload),
			CreatedAt: time.Now().UTC(),
		}
		ok = true
	} else if kind == "last" || kind == "message" || kind == "response" {
		lastMsg, found := m.findLastAssistantMessage()
		if !found {
			m.append(dimStyle.Render("no assistant message found in this session to export"))
			return m, nil
		}
		rawPayload, _ := json.Marshal(lastMsg.TextContent())
		rec = session.WorkflowResultRecord{
			ResultID:  fmt.Sprintf("msg-%x", time.Now().UnixNano()),
			SessionID: m.sessionID,
			Kind:      "message",
			Version:   1,
			Payload:   string(rawPayload),
			CreatedAt: time.Now().UTC(),
		}
		ok = true
	} else {
		if m.store != nil && m.sessionID != "" {
			var err error
			rec, ok, err = m.store.LatestWorkflowResult(ctx, m.sessionID, kind)
			if err != nil {
				m.append(errStyle.Render("export lookup failed: " + err.Error()))
				return m, nil
			}
		}
		if !ok && (kind == "plan" || kind == "") && m.proposedPlanMD != "" {
			planJSON, _ := json.Marshal(map[string]string{"markdown": m.proposedPlanMD})
			rec = session.WorkflowResultRecord{
				ResultID:  fmt.Sprintf("plan-%x", time.Now().UnixNano()),
				SessionID: m.sessionID,
				Kind:      "plan",
				Version:   2,
				Payload:   string(planJSON),
				Role:      config.RoleSmart,
				CreatedAt: time.Now().UTC(),
			}
			ok = true
		}
		if !ok && kind == "" {
			lastMsg, found := m.findLastAssistantMessage()
			if found {
				rawPayload, _ := json.Marshal(lastMsg.TextContent())
				rec = session.WorkflowResultRecord{
					ResultID:  fmt.Sprintf("msg-%x", time.Now().UnixNano()),
					SessionID: m.sessionID,
					Kind:      "message",
					Version:   1,
					Payload:   string(rawPayload),
					CreatedAt: time.Now().UTC(),
				}
				ok = true
			}
		}
	}

	if !ok {
		if kind != "" {
			m.append(dimStyle.Render(fmt.Sprintf("no completed %s result found in this session to export", kind)))
		} else {
			m.append(dimStyle.Render("no completed workflow result or assistant message found to export"))
		}
		return m, nil
	}

	if dest == "" {
		dest = export.DefaultExportFilename(rec.Kind, time.Now(), format)
	}

	rendered, err := export.RenderResult(rec, format)
	if err != nil {
		m.append(errStyle.Render("export render failed: " + err.Error()))
		return m, nil
	}

	cwd, _ := os.Getwd()
	finalPath, err := export.WriteExportFile(dest, rendered, force, cwd)
	if err != nil {
		if errors.Is(err, export.ErrDestinationExists) {
			m.append(errStyle.Render(fmt.Sprintf("export target %s already exists (add --force to overwrite)", filepath.Base(finalPath))))
		} else {
			m.append(errStyle.Render("export write failed: " + err.Error()))
		}
		return m, nil
	}

	rel, err := filepath.Rel(cwd, finalPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = finalPath
	}
	m.append(dimStyle.Render(fmt.Sprintf("◎ exported %s (%s) to %s", rec.Kind, rec.ResultID, rel)))
	return m, nil
}

func (m *model) findLastAssistantMessage() (models.Message, bool) {
	if m.agent != nil {
		msgs := m.agent.MessagesSnapshot()
		for i := len(msgs) - 1; i >= 0; i-- {
			if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
				return msgs[i], true
			}
		}
	}
	if m.store != nil && m.sessionID != "" {
		_, msgs, err := m.store.Load(m.sessionID)
		if err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
					return msgs[i], true
				}
			}
		}
	}
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockAssistant && strings.TrimSpace(m.blocks[i].text) != "" {
			return models.Message{Role: "assistant", Content: m.blocks[i].text}, true
		}
	}
	if strings.TrimSpace(m.current) != "" {
		return models.Message{Role: "assistant", Content: m.current}, true
	}
	return models.Message{}, false
}

func (m *model) findChatMessages() []models.Message {
	if m.agent != nil {
		msgs := m.agent.MessagesSnapshot()
		if len(msgs) > 1 || (len(msgs) == 1 && msgs[0].Role != "system") {
			return msgs
		}
	}
	if m.store != nil && m.sessionID != "" {
		_, msgs, err := m.store.Load(m.sessionID)
		if err == nil && len(msgs) > 0 {
			return msgs
		}
	}
	var out []models.Message
	for _, b := range m.blocks {
		switch b.kind {
		case blockAssistant:
			out = append(out, models.Message{Role: "assistant", Content: b.text})
		case blockTool:
			out = append(out, models.Message{Role: "tool", Content: b.text})
		case blockText:
			if strings.TrimSpace(b.text) != "" {
				out = append(out, models.Message{Role: "user", Content: b.text})
			}
		}
	}
	if len(out) == 0 && m.agent != nil {
		return m.agent.MessagesSnapshot()
	}
	return out
}
