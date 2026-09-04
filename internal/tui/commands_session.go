package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/export"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/schedule"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
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
}

// Compaction events are recorded in raw-log coordinates (session.Store.RawCutoff
// does the translation) so Load never double-folds a summary. The inspection
// surface below is what makes a bad summary erasable.

// /compact retry — drop the latest compaction event and re-compact from the
// raw log. This is the whole point of recording compactions as events: a bad
// summary is inspectable (/compact log) and erasable without losing history.
func (m *model) compactRetry() {
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
		if m.workerClient == nil && m.prog != nil && !m.ensureWorker() {
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
	compactModel, compactProv, _, err := m.parseCompactTarget(args)
	if err != nil {
		m.append(errStyle.Render(err.Error()))
		return
	}
	m.compactModel, m.compactProv = compactModel, compactProv
	m.cfg.CompactModel, m.cfg.CompactProvider = m.compactModel, m.compactProv
	_ = m.saveConfig()
	if m.workerClient != nil {
		_ = m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("configure"), workerwire.ConfigureRequest{
			Model: m.modelName, Provider: m.provName, Role: m.currentRole(),
			Effort: m.currentEffort(), UpdateEffort: false, Mode: m.uiMode(),
			CompactModel: m.compactModel, CompactProvider: m.compactProv, UpdateCompact: true,
		})
	}
}

func (m *model) parseCompactTarget(args []string) (model, provider string, off bool, err error) {
	if args[0] == "off" {
		return "", "", true, nil
	}
	if _, ok := m.cfg.Models[args[0]]; !ok {
		return "", "", false, fmt.Errorf("unknown model %s", args[0])
	}
	if len(args) > 1 {
		provider = args[1]
	}
	return args[0], provider, false, nil
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
	m.cfg.CompactPct = pct
	_ = m.saveConfig()
	if m.workerClient != nil {
		_ = m.workerClient.Send(workerwire.CommandConfigure, workerRequestID("configure"), workerwire.ConfigureRequest{
			Model: m.modelName, Provider: m.provName, Role: m.currentRole(),
			Effort: m.currentEffort(), Mode: m.uiMode(),
			CompactThreshold: float64(pct) / 100, UpdateCompactThreshold: true,
		})
	}
}

type exportOptions struct {
	kind   string
	dest   string
	format string
	force  bool
}

func parseExportOptions(text string) exportOptions {
	trimmed := text
	for _, pfx := range []string{"/export-result", "/export"} {
		if strings.HasPrefix(trimmed, pfx) {
			trimmed = strings.TrimPrefix(trimmed, pfx)
			break
		}
	}
	args := strings.Fields(strings.TrimSpace(trimmed))
	var opts exportOptions

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force" || arg == "-f":
			opts.force = true
		case arg == "--format" || arg == "-format":
			if i+1 < len(args) {
				i++
				opts.format = args[i]
			}
		case strings.HasPrefix(arg, "--format="):
			opts.format = strings.TrimPrefix(arg, "--format=")
		case arg == "plan" || arg == "review" || arg == "last" || arg == "message" || arg == "response" || arg == "chat" || arg == "log" || arg == "transcript":
			if opts.kind == "" {
				opts.kind = arg
			} else if opts.dest == "" {
				opts.dest = arg
			}
		default:
			if opts.dest == "" {
				opts.dest = arg
			}
		}
	}

	switch opts.kind {
	case "last", "message", "response":
		opts.kind = "message"
	case "log", "transcript":
		opts.kind = "chat"
	}
	if opts.format == "" {
		if strings.HasSuffix(strings.ToLower(opts.dest), ".json") {
			opts.format = export.FormatJSON
		} else {
			opts.format = export.FormatMarkdown
		}
	}
	return opts
}

func newExportRecord(prefix, sessionID, kind string, version int, payload string) session.WorkflowResultRecord {
	now := time.Now()
	return session.WorkflowResultRecord{
		ResultID:  fmt.Sprintf("%s-%x", prefix, now.UnixNano()),
		SessionID: sessionID,
		Kind:      kind,
		Version:   version,
		Payload:   payload,
		CreatedAt: now.UTC(),
	}
}

func (m *model) exportRecord(kind string) (session.WorkflowResultRecord, bool, error) {
	if kind == "chat" {
		msgs := m.findChatMessages()
		if len(msgs) == 0 {
			return session.WorkflowResultRecord{}, false, nil
		}
		rawPayload, _ := json.Marshal(msgs)
		return newExportRecord("chat", m.sessionID, "chat", 1, string(rawPayload)), true, nil
	}
	if kind == "message" {
		lastMsg, found := m.findLastAssistantMessage()
		if !found {
			return session.WorkflowResultRecord{}, false, nil
		}
		rawPayload, _ := json.Marshal(lastMsg.TextContent())
		return newExportRecord("msg", m.sessionID, "message", 1, string(rawPayload)), true, nil
	}

	var rec session.WorkflowResultRecord
	var ok bool
	if m.store != nil && m.sessionID != "" {
		var err error
		rec, ok, err = m.store.LatestWorkflowResult(context.Background(), m.sessionID, kind)
		if err != nil {
			return session.WorkflowResultRecord{}, false, err
		}
	}
	if !ok && (kind == "plan" || kind == "") && m.proposedPlanMD != "" {
		planJSON, _ := json.Marshal(map[string]string{"markdown": m.proposedPlanMD})
		rec = newExportRecord("plan", m.sessionID, "plan", 2, string(planJSON))
		rec.Role = config.RoleSmart
		ok = true
	}
	if !ok && kind == "" {
		lastMsg, found := m.findLastAssistantMessage()
		if found {
			rawPayload, _ := json.Marshal(lastMsg.TextContent())
			rec = newExportRecord("msg", m.sessionID, "message", 1, string(rawPayload))
			ok = true
		}
	}
	return rec, ok, nil
}

// exportResultCommand handles `/export` and `/export-result` with `[chat|plan|review|last|message] [dest] [--format json|markdown] [--force]`.
func (m *model) exportResultCommand(text string) (tea.Model, tea.Cmd) {
	opts := parseExportOptions(text)
	rec, ok, err := m.exportRecord(opts.kind)
	if err != nil {
		m.append(errStyle.Render("export lookup failed: " + err.Error()))
		return m, nil
	}

	if !ok {
		switch opts.kind {
		case "chat":
			m.append(dimStyle.Render("no chat messages found in this session to export"))
		case "message":
			m.append(dimStyle.Render("no assistant message found in this session to export"))
		case "":
			m.append(dimStyle.Render("no completed workflow result or assistant message found to export"))
		default:
			m.append(dimStyle.Render(fmt.Sprintf("no completed %s result found in this session to export", opts.kind)))
		}
		return m, nil
	}

	if opts.dest == "" {
		opts.dest = export.DefaultExportFilename(rec.Kind, time.Now(), opts.format)
	}

	rendered, err := export.RenderResult(rec, opts.format)
	if err != nil {
		m.append(errStyle.Render("export render failed: " + err.Error()))
		return m, nil
	}

	cwd, _ := os.Getwd()
	finalPath, err := export.WriteExportFile(opts.dest, rendered, opts.force, cwd)
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
	if msg, ok := lastAssistant(m.messages); ok {
		return msg, true
	}
	if m.store != nil && m.sessionID != "" {
		_, msgs, err := m.store.Load(m.sessionID)
		if err == nil {
			if msg, ok := lastAssistant(msgs); ok {
				return msg, true
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

func lastAssistant(msgs []models.Message) (models.Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
			return msgs[i], true
		}
	}
	return models.Message{}, false
}

func (m *model) findChatMessages() []models.Message {
	if len(m.messages) > 1 || (len(m.messages) == 1 && m.messages[0].Role != "system") {
		return slices.Clone(m.messages)
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
	return out
}
