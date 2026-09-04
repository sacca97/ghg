package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

type picker struct {
	metas    []session.Meta
	idx      int
	previews map[string][2]string
	pendingD bool
}

func (m *model) pickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	moveUp := func() {
		p.pendingD = false
		if p.idx < len(p.metas)-1 {
			p.idx++
			p.loadPreview(m.store)
		}
	}
	moveDown := func() {
		p.pendingD = false
		if p.idx > 0 {
			p.idx--
			p.loadPreview(m.store)
		}
	}
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		p.pendingD = false
		m.picker = nil
	case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
		moveUp()
	case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
		moveDown()
	case tea.KeyEnter:
		p.pendingD = false
		if len(p.metas) == 0 {
			m.picker = nil
			return m, nil
		}
		id := p.metas[p.idx].ID
		m.picker = nil
		if err := m.resume(id); err != nil {
			m.append(errStyle.Render(err.Error()))
		}
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "k":
			moveUp()
		case "j":
			moveDown()
		case "d":
			if !p.pendingD {
				p.pendingD = true
				return m, nil
			}
			p.pendingD = false
			if len(p.metas) == 0 {
				return m, nil
			}
			toDelete := p.metas[p.idx]
			if m.store != nil {
				if err := m.store.DeleteSession(toDelete.ID); err != nil {
					m.append(errStyle.Render("delete session failed: " + err.Error()))
					return m, nil
				}
			}
			if m.sessionID == toDelete.ID {
				m.resetSessionState()
			}
			p.metas = slices.Delete(p.metas, p.idx, p.idx+1)
			delete(p.previews, toDelete.ID)
			m.append(dimStyle.Render("◎ deleted session " + toDelete.ID))
			if len(p.metas) == 0 {
				m.picker = nil
				return m, nil
			}
			if p.idx >= len(p.metas) {
				p.idx = len(p.metas) - 1
			}
			p.loadPreview(m.store)
		default:
			p.pendingD = false
		}
	default:
		p.pendingD = false
	}
	return m, nil
}

func (p *picker) loadPreview(store *session.Store) {
	id := p.metas[p.idx].ID
	if _, ok := p.previews[id]; !ok {
		u, a := store.LastExchange(id)
		p.previews[id] = [2]string{u, a}
	}
}

func (m *model) resume(id string) error {
	return m.resumeDisplay(id)
}

func (m *model) resumeDisplay(id string) error {
	if m.store == nil {
		return errors.New("session store unavailable")
	}
	if m.busy {
		return errors.New("cannot resume while the worker is busy")
	}
	meta, msgs, err := m.store.Load(id)
	if err != nil {
		return err
	}
	if err := m.beginWorkerTransition(); err != nil {
		return err
	}

	role := m.currentRole()
	route, routeErr := resolveDisplayRoute(m.cfg, m.profiles, meta.Model, meta.Provider, role)
	if routeErr == nil {
		m.modelName, m.provName, m.modelID = route.ModelName, route.ProviderName, route.APIID
		m.protocol, m.role, m.contextLimit = route.Protocol, route.Role, route.ContextLimit
	} else {
		m.modelName, m.provName = meta.Model, meta.Provider
		m.modelID = meta.Model
		m.role = role
		m.protocol = ""
		m.contextLimit = m.contextLimitFor(m.provName, m.modelID)
	}
	if meta.Effort != "" {
		m.effort = meta.Effort
	} else if m.cfg != nil {
		m.effort = m.cfg.DefaultEffort
	}

	if len(msgs) == 0 || msgs[0].Role != "system" {
		msgs = append([]models.Message{{Role: "system", Content: m.sysPrompt}}, msgs...)
	}
	m.setMessages(msgs)
	m.sessionID = meta.ID
	m.usage = usageFromMeta(meta, msgs)
	m.workerContextTokens = 0
	m.workerTasks = nil
	if tasks, taskErr := m.store.LoadTasks(meta.ID); taskErr == nil {
		m.workerTasks = make(map[string]workerwire.TaskState, len(tasks))
		for _, task := range tasks {
			m.workerTasks[task.ID] = workerwire.TaskState{
				ID: task.ID, Description: task.Description, Prompt: task.Prompt,
				Status: task.Status, Report: task.Report,
				StartedAt: task.StartedAt, EndedAt: task.EndedAt, Restored: true,
			}
		}
	}
	if record, ok, loadErr := m.store.LoadGoal(meta.ID); loadErr == nil && ok {
		m.applyGoalRecord(record)
	} else {
		m.goalRecord = nil
	}
	m.modelSlotW = m.statusModelSlotWidth()
	m.future = nil
	m.proposedPlanMD = ""
	if m.store != nil {
		if planRecord, ok, err := m.store.LatestWorkflowResult(context.Background(), meta.ID, "plan"); err == nil && ok {
			var payload map[string]string
			if json.Unmarshal([]byte(planRecord.Payload), &payload) == nil && payload["markdown"] != "" {
				m.proposedPlanMD = payload["markdown"]
			}
		}
	}
	m.planCurrent = ""
	m.blocks = nil
	m.msgBlock = nil
	m.seedTranscript(msgs[1:], 1)
	m.append(dimStyle.Render(fmt.Sprintf("resumed %s · %s · %s @ %s", meta.ID, meta.Title, m.modelName, m.provName)))
	if routeErr != nil {
		m.append(errStyle.Render("resume route: " + routeErr.Error()))
	}
	interrupted := 0
	for _, msg := range msgs {
		if msg.Role == "tool" && strings.HasPrefix(msg.Content, "Error: tool call interrupted") {
			interrupted++
		}
	}
	if interrupted > 0 {
		m.append(dimStyle.Render(fmt.Sprintf("⚠ %d tool call(s) were interrupted when this session last ended; the model knows and can retry them.", interrupted)))
	}
	m.workerStartFailed = false
	m.workerStartError = ""
	if m.prog != nil && !m.ensureWorker() {
		m.append(errStyle.Render("worker unavailable: " + m.workerStartError))
	}
	return nil
}

func usageFromMeta(meta session.Meta, msgs []models.Message) models.Usage {
	u := models.Usage{PromptTokens: meta.UsageIn, CompletionTokens: meta.UsageOut}
	u.AddCached(meta.UsageCached)
	if u.PromptTokens != 0 || u.CompletionTokens != 0 || u.Cached() != 0 {
		return u
	}
	for _, msg := range msgs {
		if msg.Usage == nil {
			continue
		}
		u.PromptTokens += msg.Usage.PromptTokens
		u.CompletionTokens += msg.Usage.CompletionTokens
		u.AddCached(msg.Usage.Cached())
	}
	return u
}

func (m *model) seedTranscript(msgs []models.Message, base int) {
	for i, msg := range msgs {
		bi := -1
		switch msg.Role {
		case "user":
			bi = len(m.blocks)
			m.blocks = append(m.blocks, block{kind: blockText, text: youStyle.Render("❯ ") + linkifyFilePaths(msg.TextContent(), realFileExists)})
		case "assistant":
			if strings.TrimSpace(msg.TextContent()) != "" {
				bi = len(m.blocks)
				m.blocks = append(m.blocks, block{kind: blockAssistant, text: strings.TrimRight(msg.TextContent(), "\n")})
			}
			for _, tc := range msg.ToolCalls {
				m.blocks = append(m.blocks, block{kind: blockText, text: toolStyle.Render("⚒ "+tc.Function.Name+" ") + dimStyle.Render(tc.Function.Arguments)})
			}
		case "tool":
			if strings.HasPrefix(msg.Content, "Error: tool call interrupted") {
				m.blocks = append(m.blocks, block{kind: blockText, text: errStyle.Render("⚒ "+msg.Name+" ") + dimStyle.Render("— interrupted: session ended before a result was recorded")})
			}
		}
		for len(m.msgBlock) <= base+i {
			m.msgBlock = append(m.msgBlock, -1)
		}
		m.msgBlock[base+i] = bi
	}
	m.follow = true
	m.refreshVP()
}

// /fork copies the conversation (whole, or up to a rewind-picker selection)
// into a NEW session with a chosen title and switches to it — "copy
// conversation with new name"; the original stays untouched and /resume-able
// (opencode's Session.fork, packages/opencode/src/session/session.ts:691).
// /rename retitles the current session. Both share one inline prompt: the
// input box is repurposed with a prefixed label, enter commits, esc cancels.

// forkCommand implements /fork [name].
func (m *model) forkCommand(arg string) {
	if m.store == nil {
		m.append(errStyle.Render("no session store"))
		return
	}
	if arg != "" {
		m.fork(m.messageCount(), arg)
		return
	}
	// bare: suggest "<title> (fork #N)" and let the user rename inline
	suggest := "session (fork #1)"
	if m.sessionID != "" {
		if meta, _, err := m.store.Load(m.sessionID); err == nil {
			if t, err := m.store.ForkTitle(meta.Title); err == nil {
				suggest = t
			}
		}
	}
	m.openForkPrompt(m.messageCount(), false, suggest)
}

// openForkPrompt asks for a name, then forks at cut. picker notes when the
// prompt came from the rewind picker, for the confirmation line.
func (m *model) openForkPrompt(cut int, picker bool, suggest ...string) {
	name := ""
	if len(suggest) > 0 {
		name = suggest[0]
	}
	m.openNamePrompt("⑂ fork name:", name, func(title string) {
		m.fork(cut, title)
	})
	if picker {
		m.append(dimStyle.Render("⑂ forking from the selected message — name the copy (enter) or esc"))
	}
}

// fork copies the history through conversation index cut (inclusive) into a
func (m *model) fork(cut int, title string) {
	m.forkWorker(cut, title)
}

func (m *model) forkWorker(cut int, title string) {
	title = strings.TrimSpace(title)
	if title == "" {
		m.append(errStyle.Render("fork needs a name"))
		return
	}
	if m.sessionID == "" {
		m.append(errStyle.Render("no session to fork"))
		return
	}
	if m.busy || m.workerLiveWork {
		m.append(dimStyle.Render("(worker is busy — fork after this work finishes)"))
		return
	}
	if m.workerClient == nil && !m.ensureWorker() {
		m.append(errStyle.Render("fork failed: worker unavailable: " + m.workerStartError))
		return
	}
	requestID := workerRequestID("fork")
	if err := m.workerClient.Send(workerwire.CommandFork, requestID, workerwire.ForkRequest{Cut: cut, Title: title}); err != nil {
		m.append(errStyle.Render("fork failed: " + err.Error()))
		return
	}
}

// renameCommand implements /rename [title].
func (m *model) renameCommand(arg string) {
	if m.store == nil {
		m.append(errStyle.Render("no session store"))
		return
	}
	if arg != "" {
		m.rename(arg)
		return
	}
	cur := ""
	if m.sessionID != "" {
		if meta, _, err := m.store.Load(m.sessionID); err == nil {
			cur = meta.Title
		}
	}
	m.openNamePrompt("✎ session name:", cur, m.rename)
}

func (m *model) rename(title string) {
	title = strings.TrimSpace(title)
	if title == "" {
		m.append(errStyle.Render("rename needs a title"))
		return
	}
	if m.sessionID == "" {
		m.append(errStyle.Render("no session to rename"))
		return
	}
	if m.workerClient == nil && !m.ensureWorker() {
		m.append(errStyle.Render("rename failed: worker unavailable: " + m.workerStartError))
		return
	}
	requestID := workerRequestID("rename")
	if err := m.workerClient.Send(workerwire.CommandRename, requestID, workerwire.RenameRequest{Title: title}); err != nil {
		m.append(errStyle.Render("rename failed: " + err.Error()))
		return
	}
}

// Rewind: double-esc while idle opens a picker over the conversation's
// authored user messages. Browsing live-scrolls the transcript (opencode's
// dialog-timeline onMove). enter rewinds the conversation to just before the
// selected message — Agent.Messages and the DB are truncated, the transcript
// is rebuilt, and the message text lands back in the input for editing
// (opencode's undo: "the input restore is what makes it feel good"). The
// clipped tail is kept in memory as a redo stack: reopening the picker while
// rewound lists the clipped messages dimmed below the live ones, and enter on
// one moves forward again. Submitting anything new discards the future.
// Fork from any entry with f.

// rewindEntry is one row of the rewind picker. cut is the conversation index
// the entry points at: for a live message it is its index in agent.Messages,
// for a clipped "future" message it is its original conversation index
// (base + position in the redo stack, where base = len(agent.Messages)).
// enter rewinds to just before cut; f forks the history through cut.
type rewindEntry struct {
	cut    int
	text   string     // single-line preview
	when   *time.Time // when the message was submitted (nil = unknown)
	future bool       // clipped by the active rewind; selecting moves forward
}

type rewindState struct {
	entries []rewindEntry // chronological: oldest first, latest LAST
	sel     int           // direct index into entries; starts at the latest
	savedVP int           // viewport offset on open, restored on cancel
}

// rewindBase is where the conversation was cut. future is kept ordered by
// original conversation index (oldest first), so the boundary is simply
// len(agent.Messages); future[i] holds original index base+i.

// Cuts never split a tool_call from its results: entries sit at user
// messages and an assistant message's tool calls/results always follow it,
// so moving the cut to "before the user message" is boundary-safe.
type escArmMsg struct{} // disarms the idle double-esc window

func (m *model) rewindEntries() []rewindEntry {
	messages := m.messagesSnapshot()
	var out []rewindEntry
	for i, msg := range messages {
		if msg.Role == "user" && msg.Authored {
			out = append(out, rewindEntry{cut: i, text: oneLine(msg.TextContent()), when: msg.SentAt})
		}
	}
	for i, msg := range m.future {
		if msg.Role == "user" && msg.Authored {
			out = append(out, rewindEntry{
				cut: len(messages) + i, text: oneLine(msg.TextContent()), when: msg.SentAt, future: true,
			})
		}
	}
	return out
}

func oneLine(s string) string { return truncLine(strings.Join(strings.Fields(s), " "), 100) }

// scrollToMsg live-scrolls the viewport so the block rendering
// agent.Messages[msgIdx] is near the top.
func (m *model) scrollToMsg(msgIdx int) {
	if msgIdx < 0 || msgIdx >= len(m.msgBlock) {
		return
	}
	bi := m.msgBlock[msgIdx]
	if bi < 0 || bi >= len(m.blocks) {
		return
	}
	m.follow = false
	m.vp.SetYOffset(max(m.blocks[bi].y0-1, 0))
}

func (m *model) openRewind() {
	entries := m.rewindEntries()
	if len(entries) == 0 {
		m.append(dimStyle.Render("(nothing to rewind to yet)"))
		return
	}
	m.rew = &rewindState{entries: entries, sel: len(entries) - 1, savedVP: m.vp.YOffset}
	m.scrollToMsg(entries[len(entries)-1].cut) // selection starts on the latest
}

func (m *model) rewindKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := m.rew
	sel := func() rewindEntry { return r.entries[r.sel] }
	switch msg.Type {
	case tea.KeyEsc:
		m.vp.SetYOffset(r.savedVP) // put the scroll back where the user had it
		m.rew = nil
	case tea.KeyUp: // up the list = toward the oldest (top)
		r.sel = max(r.sel-1, 0)
		m.scrollToMsg(sel().cut)
	case tea.KeyDown: // down the list = toward the latest (bottom)
		r.sel = min(r.sel+1, len(r.entries)-1)
		m.scrollToMsg(sel().cut)
	case tea.KeyEnter:
		e := sel()
		m.requestWorkerRewind(e.cut)
		m.rew = nil
		return m, nil
	case tea.KeyRunes:
		if string(msg.Runes) == "f" {
			e := sel()
			m.rew = nil
			m.openForkPrompt(e.cut, true) // the copy keeps the selected message
			return m, nil
		}
	}
	return m, nil
}

func (m *model) requestWorkerRewind(cut int) {
	if m.workerClient == nil && !m.ensureWorker() {
		m.append(errStyle.Render("rewind: worker unavailable: " + m.workerStartError))
		return
	}
	current := m.messagesSnapshot()
	all := append(slices.Clone(current), m.future...)
	cut = min(max(cut, 1), max(len(all)-1, 1))
	if len(current) == 0 || len(all) <= 1 {
		m.append(dimStyle.Render("(nothing to rewind to yet)"))
		return
	}
	text := ""
	if msg := m.messageAt(cut); msg.Role == "user" && msg.Authored {
		text = msg.TextContent()
	}
	if cut < len(current) {
		m.future = append(slices.Clone(current[cut:]), m.future...)
	} else if cut > len(current) {
		m.future = slices.Clone(m.future[cut-len(current):])
	}
	requestID := workerRequestID("rewind")
	m.workerHistoryRequest = requestID
	m.workerRewindRestore = text
	if err := m.workerClient.Send(workerwire.CommandRewind, requestID, workerwire.RewindRequest{
		Cut: cut, Messages: all[:cut],
	}); err != nil {
		m.workerHistoryRequest = ""
		m.workerRewindRestore = ""
		m.append(errStyle.Render("rewind: " + err.Error()))
		return
	}
	m.append(dimStyle.Render("⟲ rewinding conversation…"))
}

// messageAt reads conversation index i across the live/redo boundary.
func (m *model) messageAt(i int) models.Message {
	if i < 0 {
		return models.Message{}
	}
	messages := m.messagesSnapshot()
	if i < len(messages) {
		return messages[i]
	}
	i -= len(messages)
	if i < len(m.future) {
		return m.future[i]
	}
	return models.Message{}
}

// rebuildTranscript resets the block list from stored messages.
func (m *model) rebuildTranscript() {
	m.blocks = nil
	m.msgBlock = nil
	m.workerContextTokens = m.usage.PromptTokens + m.usage.CompletionTokens
	messages := m.messagesSnapshot()
	if len(messages) > 0 && messages[0].Role == "system" {
		m.seedTranscript(messages[1:], 1)
	} else {
		m.seedTranscript(messages, 0)
	}
}

// rewindView renders the picker strip above the input: oldest at the top,
// latest at the bottom, so ↑ moves toward older and ↓ toward newer. Each entry
// takes two rows — the preview line, then a dimmed timestamp beneath it.
func (m *model) rewindView() string {
	r := m.rew
	const maxRows = 8 // entry rows; each renders as 2 lines
	// window over entries; sel starts at the latest (bottom) so anchor there
	start := max(0, min(r.sel-maxRows/2, len(r.entries)-maxRows))
	end := min(start+maxRows, len(r.entries))
	var b strings.Builder
	b.WriteString(dimStyle.Render("⏪ rewind — enter: rewind here · f: fork from here · esc: cancel"))
	for row := start; row < end; row++ {
		e := r.entries[row]
		b.WriteString("\n")
		if row == r.sel {
			b.WriteString(youStyle.Render("❯ " + e.text))
		} else if e.future {
			b.WriteString(dimStyle.Render("  " + e.text + " (rewound)"))
		} else {
			b.WriteString("  " + e.text)
		}
		b.WriteString("\n    " + dimStyle.Render(rewindWhen(e.when)))
	}
	fmt.Fprintf(&b, "\n%s", dimStyle.Render(fmt.Sprintf("  (%d/%d) ↑ older · ↓ newer", r.sel+1, len(r.entries))))
	return b.String()
}

// rewindWhen renders an entry's submission time for the picker. Pre-SentAt
// sessions have no per-message timestamp; show an em dash rather than a wrong
// or blank line.
func rewindWhen(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04") + " · " + ago(*t)
}

// discardFuture drops the redo stack: any new activity while rewound makes
// the clipped tail unreachable (branch-point semantics).
func (m *model) discardFuture() { m.future = nil }

func (m *model) resetSessionState() {
	if m.workerClient != nil || m.workerProcess != nil {
		m.stopWorker()
		m.workerStartFailed = false
	}
	m.messages = []models.Message{{Role: "system", Content: m.sysPrompt}}
	m.modelID = ""
	m.usage = models.Usage{}
	m.contextLimit = 0
	m.workerTasks = nil
	m.workerStartError = ""
	m.workerContextTokens = 0
	m.blocks = nil
	m.msgBlock = nil
	m.future = nil
	m.proposedPlanMD = ""
	m.planCurrent = ""
	m.setGoal("")
	m.goalRecord = nil
	m.sessionID = ""
}

const previewLines = 5

func (m *model) pickerView() string {
	p := m.picker
	rows := []string{}
	expanded := 3 + 2*previewLines
	budget := max(m.height-2-expanded-1, 2)
	lo := max(p.idx-budget/2, 0)
	hi := min(lo+budget+1, len(p.metas))
	for i := hi - 1; i >= lo; i-- {
		meta := p.metas[i]
		title := meta.Title
		if title == "" {
			title = "(untitled)"
		}
		line := fmt.Sprintf("%s  %s · %s · %s @ %s", meta.ID, title, ago(meta.UpdatedAt), meta.Model, meta.Provider)
		if i != p.idx {
			rows = append(rows, wrap("    "+line, m.width))
			continue
		}
		rows = append(rows, wrap(botStyle.Render("  → ")+line, m.width))
		prev := p.previews[meta.ID]
		rows = append(rows, previewBlock(youStyle.Render("❯ "), prev[0], m.width)...)
		rows = append(rows, previewBlock(botStyle.Render("● "), prev[1], m.width)...)
	}
	footer := fmt.Sprintf("  (%d/%d) ↑/k older · ↓/j newer · enter resume · dd delete · esc cancel", p.idx+1, len(p.metas))
	if p.pendingD {
		footer = fmt.Sprintf("  (%d/%d) press d again to delete session %s", p.idx+1, len(p.metas), p.metas[p.idx].ID)
	}
	rows = append(rows, dimStyle.Render(footer))
	for len(rows) < m.height-1 {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

func previewBlock(prefix, text string, width int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	w := max(width-8, 8)
	var lines []string
	for i, l := range strings.Split(text, "\n") {
		wrapped := strings.Split(ansi.Hardwrap(l, w, true), "\n")
		for j, wl := range wrapped {
			if i == 0 && j == 0 {
				lines = append(lines, "      "+prefix+wl)
			} else {
				lines = append(lines, "        "+wl)
			}
		}
	}
	if len(lines) > previewLines {
		lines = append(lines[:previewLines], dimStyle.Render(fmt.Sprintf("        … +%d lines (full text after resume)", len(lines)-previewLines)))
	}
	return lines
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
