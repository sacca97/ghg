package tui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"

	"github.com/charmbracelet/x/ansi"
)

// blockKind classifies a transcript block so a resize can re-render it at
// the new width. Assistant text reflows through glamour (markdown); tool
// results hold raw output and expand/collapse; every other block — user
// input, tool calls, status lines — re-wraps plainly (its styling is baked
// in at append time; only the wrap changes).
type blockKind int

const (
	blockText      blockKind = iota // already-styled line(s): re-wrap on resize
	blockAssistant                  // raw markdown: re-render through glamour
	blockTool                       // raw tool result: collapsed preview, expandable
	blockToolRun                    // a running tool call: verb line, collapses on completion
	blockPlan                       // proposed plan markdown: re-render with plan styling
)

// toolPreviewLines is how many lines of a tool result show when collapsed.
const toolPreviewLines = 5

// minRenderWidth is the smallest width blocks render at. A transient
// degenerate WindowSizeMsg (1–4 cols from a tmux/PTY handshake) would
// otherwise collapse blockTool/blockText into a one-char-per-line strip —
// those wrap with no floor, and a cached bad render persists until a width
// *change* forces a reflow. Below this the layout is unreadable either way.
const minRenderWidth = 8

// block is one finalized transcript entry. Text holds raw markdown for
// blockAssistant, raw tool output for blockTool, and styled content
// otherwise.
type block struct {
	kind     blockKind
	text     string
	expanded bool // blockTool/blockToolRun: show the full output (click / ctrl+e toggles)
	// blockToolRun: the tool-call id this row tracks and whether it's still
	// running — on completion the row collapses in place to one line.
	toolID      string
	toolRunning bool
	toolFailed  bool
	// y0/y1 are the block's line range in the last rendered content (set by
	// refreshVP); used to map a mouse click to the block under it.
	y0, y1 int
	// cache of the last render: valid while !stale and width matches.
	rendered string
	lines    int
	width    int
	stale    bool
}

// renderAt returns the block rendered at width, re-rendering only when the
// cache is cold (first render, width change, or text/expand mutation). This
// is what makes appends and resume cheap: unchanged blocks never re-render.
func (b *block) renderAt(width int) string {
	if !b.stale && b.width == width {
		return b.rendered
	}
	b.rendered = b.render(width)
	b.lines = lipgloss.Height(b.rendered)
	b.width, b.stale = width, false
	return b.rendered
}

// render renders the block at width (the full terminal width; assistant
// blocks get their marker + indent here so a resize re-renders everything).
func (b block) render(width int) string {
	switch b.kind {
	case blockAssistant:
		return renderMarkdownBlock("●", b.text, width)
	case blockPlan:
		return renderMarkdownBlock("◎", b.text, width)
	case blockTool:
		lines := strings.Split(strings.TrimRight(b.text, "\n"), "\n")
		if b.expanded || len(lines) <= toolPreviewLines {
			return wrap(dimStyle.Render("  "+strings.Join(lines, "\n  ")), width)
		}
		preview := toolPreview(lines)
		out := dimStyle.Render("  " + strings.Join(preview, "\n  "))
		hint := fmt.Sprintf("\n  … +%d lines (ctrl+e or click to expand)", len(lines)-len(preview))
		return wrap(out+dimStyle.Render(hint), width)
	case blockToolRun:
		return wrap(b.text, width)
	default:
		return wrap(b.text, width)
	}
}

func renderMarkdownBlock(marker, text string, width int) string {
	w := width - 2
	if w <= 0 {
		w = 80
	}
	body := indentLines(renderMarkdown(text, w), 2)
	return botStyle.Render(marker+" ") + strings.TrimPrefix(body, "  ")
}

func toolPreview(lines []string) []string {
	if len(lines) <= toolPreviewLines {
		return lines
	}
	if strings.HasSuffix(lines[len(lines)-1], "```") {
		const maxDiffPreviewLines = 8
		var preview []string
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				continue
			}
			if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
				if !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "+++") && len(preview) < maxDiffPreviewLines+1 {
					preview = append(preview, line)
				}
			} else if len(preview) < 2 {
				preview = append(preview, line)
			}
		}
		if len(preview) > 0 {
			return preview
		}
	}
	return lines[:toolPreviewLines]
}

// expand toggles a tool block and returns whether it changed.
func (b *block) toggle() bool {
	if b.kind != blockTool {
		return false
	}
	b.expanded = !b.expanded
	b.stale = true
	return true
}

// append adds finalized blocks to the transcript, separating blocks with a
// blank line so consecutive messages and tool calls breathe.
func (m *model) append(blocks ...string) {
	for _, s := range blocks {
		m.appendRaw(blockText, s)
	}
}

// appendAssistant appends raw assistant markdown; rendering happens in
// refreshVP at the current width.
func (m *model) appendAssistantBlock(s string) {
	m.appendRaw(blockAssistant, s)
}

func (m *model) appendPlanBlock(s string) {
	m.appendRaw(blockPlan, s)
}

func (m *model) appendRaw(kind blockKind, text string) {
	m.blocks = append(m.blocks, block{kind: kind, text: text})
	m.transcriptDirty = true
	if m.prog == nil {
		m.refreshVP()
	}
}

// refreshVP rebuilds the viewport content, bottom-anchored: short transcripts
// are padded from the top so messages grow upward from the input. Block
// renders are cached per width (renderAt), so a rebuild is an O(transcript)
// join of cached strings; the expensive glamour markdown render only happens
// for blocks that are new, mutated, or hit by a width change. This is what
// keeps resume and streaming appends near-linear.
func (m *model) refreshVP() {
	if m.width == 0 {
		return // tea hasn't started (resume path): the first WindowSizeMsg renders once at the real width
	}
	// Clamp to a sane minimum so a degenerate WindowSizeMsg (a 1–4 col width,
	// which tmux/PTY handshakes can emit transiently) never collapses blocks
	// into a one-char-per-line strip: blockTool/blockText wrap with no floor,
	// so width 1 renders one character per row. Below minRenderWidth the layout
	// is unreadable either way — render at the floor instead.
	width := max(m.width, minRenderWidth)
	var b strings.Builder
	if n := len(m.blocks); n > 0 {
		b.Grow(n * 24)
	}
	line := 0
	for i := range m.blocks {
		if i > 0 {
			b.WriteString("\n\n") // blank line between blocks
			line++                // the two newlines create one blank row
		}
		r := m.blocks[i].renderAt(width)
		m.blocks[i].y0 = line
		m.blocks[i].y1 = line + m.blocks[i].lines - 1
		b.WriteString(r)
		line = m.blocks[i].y1 + 1
	}
	content := b.String()
	if pad := m.contentPad(); pad > 0 {
		content = strings.Repeat("\n", pad) + content
	}
	m.viewportContent = content
	m.plainRows = nil
	m.vp.SetContent(content)
	if m.follow {
		m.vp.GotoBottom()
	}
	m.transcriptDirty = false
}

// contentPad is the number of blank lines refreshVP prepends when the
// transcript is shorter than the viewport (click-row mapping accounts for it).
func (m *model) contentPad() int {
	if len(m.blocks) == 0 {
		return m.vp.Height
	}
	h := m.blocks[len(m.blocks)-1].y1 + 1 // content height from the last block
	return max(m.vp.Height-h, 0)
}

// inputRule is the one-row divider between the transcript (plus its
// ephemeral spinner/queue rows) and the input box, so the thing you type into
// is visually separate from the thing you read. It replaces what used to be a
// bare blank line, so it costs no extra row — layout()'s chrome already counts
// exactly one row here.
//
// While an interactive bash command owns the terminal the input box is hidden,
// and a rule with nothing under it reads as a stray line, so that case keeps
// the blank row instead.
func (m *model) inputRule() string {
	if m.iactive != nil {
		return ""
	}
	w := min(max(m.width, 0), maxRuleWidth)
	if w == 0 {
		return ""
	}
	return dimStyle.Render(strings.Repeat("─", w))
}

// maxRuleWidth caps the divider so a very wide terminal doesn't draw a line
// across the whole desk; the transcript is the thing to follow, not the rule.
const maxRuleWidth = 120

// viewportView renders the transcript viewport at its full allocated height.
// The content is bottom-anchored — padding goes on top — so a short transcript
// still ends directly above the input. Keeping the height fixed is important:
// when scrolling lands on a blank separator, trimming it would move the input
// and status box, so scrolling never moves the stable tail.
func (m *model) viewportView() string {
	view := m.vp.View()
	if m.selection != nil && m.selection.hasRange() {
		view = m.selectedViewportView(view)
	}
	return sanitizeView(view)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// fmtTok renders a token count compactly: 12.3k, 1.2M, 134 raw under 1000.
func fmtTok(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprint(n)
	}
}

// fmtCost renders a USD spend compactly: 4 decimals under a dollar (where the
// cents would hide the signal), 2 at or above.
func fmtCost(d float64) string {
	if d >= 1 {
		return fmt.Sprintf("$%.2f", d)
	}
	return fmt.Sprintf("$%.4f", d)
}

// layout gives the viewport whatever height the chrome doesn't need,
// growing the input box with its content so the whole prompt stays visible.
func (m *model) layout() {
	m.growInput()
	// Rows View() spends outside the viewport, counted against m.height. Get
	// this wrong and the frame overflows the terminal: a too-tall frame makes
	// the terminal scroll on every repaint, which reads as "mouse scroll is
	// broken" — the wheel moves the viewport while the repaint shoves the
	// whole frame the other way.
	//   1 header · 1 separator above the input · input.Height() ·
	//   1 blank + the three-row status box
	// The old two-line ctrl+p hint is no longer rendered in View().
	chrome := 6 + m.input.Height()
	if m.quit1 || m.escClr || (m.esc1 && m.rew == nil && m.namePrompt == nil) {
		chrome++ // an armed-hint line renders under the input
	}
	if m.iactive != nil {
		// input box is hidden while a command has the terminal; drop its height
		// and the leading blank line View inserts before it.
		chrome -= m.input.Height()
	}
	if m.busy {
		chrome += 2 // blank line above the spinner + the spinner line itself
	}
	if m.current != "" {
		chrome += lipgloss.Height(m.currentView()) + 1 // + its blank separator
	}
	if !m.thinkStart.IsZero() && m.showThinking {
		chrome += lipgloss.Height(m.thinkView()) + 1
	}
	if m.iactive != nil {
		chrome += lipgloss.Height(m.interactiveView()) + 1
	}
	if m.permDialog != nil {
		chrome += lipgloss.Height(m.permView()) + 1
	}
	if m.rew != nil {
		chrome += lipgloss.Height(m.rewindView()) + 1
	}
	if m.menu != nil {
		chrome += min(len(m.menu.cands), menuRows) + 1
	}
	if len(m.queue) > 0 {
		chrome += len(m.queue) + 1
	}
	if m.taskVP != nil {
		m.refreshTaskVP() // the task pane owns the free area; size it to fit
	}
	m.dockRows = 0
	if dock := m.tasksDock(); dock != "" { // lipgloss.Height("") is 1, not 0
		m.dockRows = lipgloss.Height(dock)
		// clicking computes task rows from the strip's top; a focused dock's
		// hint row isn't a task — skip it
		m.dockSkip = 0
		if m.tasksFocus {
			m.dockSkip++
		}
		chrome += m.dockRows + 1 // strip + the blank line above the input
	}
	// Floor the viewport width too: a degenerate m.width (1–4 cols) would set
	// the viewport to 1 col and re-slice the transcript into a one-char strip,
	// regardless of the render floor in refreshVP.
	w, h := max(m.width, minRenderWidth), max(m.height-chrome, 1)
	if m.vp.Width != w || m.vp.Height != h || m.transcriptDirty {
		m.vp.Width, m.vp.Height = w, h
		m.refreshVP()
	}
}

// dockTop returns the screen row of the first TASK row in the dock: the dock
// renders as the last dockRows rows above the input box and bottom pad, but
// dockSkip non-task rows (the focused hint) sit on top of the task rows.
// layout() keeps both in sync with what View renders.
func (m *model) dockTop() int {
	return m.height - 2 - m.input.Height() - m.dockRows + m.dockSkip
}

// busyStats renders the busy line's elapsed time. Token totals belong only in
// the persistent bottom status bar. Returns "" when idle (turnStart zero).
func (m *model) busyStats() string {
	if m.turnStart.IsZero() {
		return ""
	}
	d := m.nowFn().Sub(m.turnStart)
	if d < 0 {
		d = 0
	}
	elapsed := d.Round(time.Second)
	return fmt.Sprintf(" %d:%02d", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
}

// contextLimitFor returns the context window for a model id on a models.
// Provider catalogs win, followed by explicit config, then models.dev.
func (m *model) contextLimitFor(provName, apiID string) int {
	if cat, ok := m.catalogs[provName]; ok {
		if n := cat.ContextLength(apiID); n > 0 {
			return n
		}
	}
	modelName := m.modelName
	if m.cfg != nil {
		if mdl, ok := m.cfg.Models[modelName]; ok {
			if n := mdl.ContextWindow(); n > 0 {
				return n
			}
		}
	}
	if n := config.LoadModelsDev().ContextLength(apiID, m.modelsDevProviderIDs(provName)...); n > 0 {
		return n
	}
	return 0
}

// contextStatus renders the latest provider-reported request size and, when
// known, the current model's advertised context window. It starts at zero
// until an assistant response reports usage.
func (m *model) contextStatus() string {
	used := m.workerContextTokens
	limit := m.contextLimit
	if limit <= 0 {
		return "ctx " + fmtTok(used)
	}
	return fmt.Sprintf("ctx %s/%s", fmtTok(used), fmtTok(limit))
}

// sessionCost returns the session's cumulative USD spend at the current
// model's advertised rates; ok is false when the provider's catalog has no
// pricing for the model, in which case the status line hides the segment.
func (m *model) sessionCost() (float64, bool) {
	cat, ok := m.catalogs[m.provName]
	if !ok {
		return 0, false
	}
	in, out, cacheRead, ok := cat.Pricing(m.currentModelID())
	if !ok {
		return 0, false
	}
	return models.SessionCost(m.currentUsage(), in, out, cacheRead), true
}

// tasksView renders the background-subagent list for /tasks.
func (m *model) tasksView() string {
	var tasks []agent.BackgroundTask
	for _, task := range m.workerTasks {
		tasks = append(tasks, agent.BackgroundTask{ID: task.ID, Description: task.Description, Prompt: task.Prompt, Status: agent.TaskStatus(task.Status), Report: task.Report, StartedAt: task.StartedAt, EndedAt: task.EndedAt, Restored: task.Restored})
	}
	slices.SortStableFunc(tasks, func(a, b agent.BackgroundTask) int {
		if n := b.StartedAt.Compare(a.StartedAt); n != 0 {
			return n
		}
		return strings.Compare(b.ID, a.ID)
	})
	if len(tasks) == 0 {
		return dimStyle.Render("(no background subagents)")
	}
	var b strings.Builder
	b.WriteString(dimStyle.Render(fmt.Sprintf("background subagents (%d):", len(tasks))))
	for _, t := range tasks {
		icon := "⏳"
		switch t.Status {
		case agent.TaskDone:
			icon = "✓"
		case agent.TaskError, agent.TaskCancelled:
			icon = "✗"
		}
		line := fmt.Sprintf("  %s %s  %s", icon, t.ID, t.Description)
		if t.Restored {
			line += dimStyle.Render("  (restored)")
		}
		if t.Status == agent.TaskRunning {
			line += dimStyle.Render(fmt.Sprintf("  (%ds)", int(time.Since(t.StartedAt).Seconds())))
		}
		b.WriteString("\n" + toolStyle.Render(line))
		if t.Status != agent.TaskRunning {
			report := t.Report
			if len(report) > 200 {
				report = report[:200] + "…"
			}
			b.WriteString("\n" + dimStyle.Render("      "+strings.ReplaceAll(report, "\n", " ")))
		}
	}
	return b.String()
}

// appendAssistant writes assistant text into the transcript, rendering it as
// markdown (glamour) and prefixing the first line of each segment with "● ".
// Consecutive segments of one message merge into a single block so the whole
// message re-renders as one markdown document on resize.
func (m *model) appendAssistant(s string) {
	if m.inMsg && len(m.blocks) > 0 && m.blocks[len(m.blocks)-1].kind == blockAssistant {
		m.blocks[len(m.blocks)-1].text += "\n\n" + s // same message: merge
		m.blocks[len(m.blocks)-1].stale = true
		m.transcriptDirty = true
		if m.prog == nil {
			m.refreshVP()
		}
		return
	}
	m.appendAssistantBlock(s)
	m.inMsg = true
}

// indentLines shifts rendered markdown right by n columns so the body sits
// under the transcript's "● " marker. Glamour indents every block from its
// 2-cell document margin; we subtract that margin and add n, preserving
// *relative* indentation (hanging list text, nested bullets, code blocks).
// Whitespace-only lines become truly empty so no stray dim cells render.
func indentLines(s string, n int) string {
	const docMargin = 2 // glamour styles.DarkStyleConfig Document.Margin
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(ansi.Strip(l)) == "" {
			lines[i] = ""
			continue
		}
		lead := len(l) - len(strings.TrimLeft(l, " "))
		shift := n + lead - docMargin
		if shift < 0 {
			shift = 0
		}
		lines[i] = strings.Repeat(" ", shift) + strings.TrimLeft(l, " ")
	}
	return strings.Join(lines, "\n")
}

// toggleThinking flips reasoning timer display (ctrl+o / settings) and persists
// the choice to the global config.
func (m *model) toggleThinking() {
	m.setThinking(!m.showThinking)
}

// setThinking applies the state without the transcript note (settings ←/→
// steppers call this); it still persists.
func (m *model) setThinking(on bool) {
	m.showThinking = on
	if !on {
		m.thinkStart = time.Time{}
	}
	b := on
	m.cfg.Thinking = &b
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
	}
}

func formatThinkingDuration(dur time.Duration) string {
	if dur < 0 {
		dur = 0
	}
	totalSec := int(dur / time.Second)
	if totalSec < 60 {
		return fmt.Sprintf("%ds", totalSec)
	}
	if totalSec < 3600 {
		m := totalSec / 60
		s := totalSec % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	return fmt.Sprintf("%dh %dm %ds", h, m, s)
}

func (m *model) flushThink() {
	hadReasoning := !m.thinkStart.IsZero()
	dur := m.nowFn().Sub(m.thinkStart)
	if dur < 0 {
		dur = 0
	}
	m.thinkStart = time.Time{}
	if hadReasoning && m.showThinking {
		m.append(thinkingStyle.Render("◌ Thinking " + formatThinkingDuration(dur)))
	}
}

// thinkView renders the in-flight reasoning timer (e.g. "◌ Thinking 3s").
func (m *model) thinkView() string {
	if m.thinkStart.IsZero() || !m.showThinking {
		return ""
	}
	dur := m.nowFn().Sub(m.thinkStart)
	if dur < 0 {
		dur = 0
	}
	s := "◌ Thinking " + formatThinkingDuration(dur)
	return thinkingStyle.Render(wrap(s, m.width))
}

// flushCurrent moves any in-flight partial line into the transcript and ends
// the current assistant segment.
func (m *model) flushCurrent() {
	cur := strings.TrimRight(m.current, " \n")
	m.current = ""
	if cur != "" {
		m.appendAssistant(cur)
	}
	m.inMsg = false
}

const menuRows = 8

func (m *model) currentView() string {
	s := m.current
	if !m.inMsg {
		s = botStyle.Render("● ") + s
	}
	return wrap(s, m.width) // streamed mid-flight: plain text; markdown renders on flush
}

func (m *model) View() string {
	var b strings.Builder
	left := fmt.Sprintf(" ghg · skills: %d loaded", m.skillsLoaded)
	if m.width > 0 {
		left = ansi.Truncate(left, m.width, "…")
	}
	b.WriteString(dimStyle.Render(left) + "\n")
	if m.settings != nil {
		b.WriteString(m.paletteView())
		return b.String()
	}
	if m.picker != nil {
		b.WriteString(m.pickerView())
		return b.String()
	}
	if m.taskVP != nil {
		b.WriteString(m.taskViewView())
		return b.String()
	}
	b.WriteString(m.viewportView() + "\n")
	if !m.thinkStart.IsZero() && m.showThinking {
		b.WriteString("\n" + m.thinkView() + "\n")
	}
	if m.current != "" {
		b.WriteString("\n" + m.currentView() + "\n")
	}
	if m.iactive != nil {
		b.WriteString("\n" + m.interactiveView() + "\n")
	}
	if m.permDialog != nil {
		b.WriteString("\n" + m.permView() + "\n")
	}
	if m.busy {
		hint := " thinking… (enter queues · /effort run now · esc interrupts · ctrl+c ctrl+c interrupts)"
		if m.iactive != nil {
			hint = " bash (interactive) — type to respond · ctrl+c ctrl+c to cancel"
		} else if m.interrupt1 {
			hint = " thinking… (esc or ctrl+c again to interrupt)"
		}
		b.WriteString("\n" + m.spin.View() + dimStyle.Render(m.busyStats()+hint) + "\n")
	}
	if len(m.queue) > 0 {
		nav := ""
		if m.busy && m.input.Value() == "" {
			nav = " · ↑/↓ select · del removes"
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf(" ⧗ queued (%d) — enter on empty input to steer into this turn%s", len(m.queue), nav)) + "\n")
		for i, q := range m.queue {
			// one line per queued message: truncate (never wrap) so long
			// messages don't crowd out the transcript
			line := ansi.Truncate(youStyle.Render(" ❯ ")+q, m.width, "…")
			if i == m.queueSel {
				line = ansi.Truncate(botStyle.Render(" → ")+q+dimStyle.Render("  (del to remove)"), m.width, "…")
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(m.inputRule() + "\n")
	// the persistent background-subagent strip sits just above the input box
	if dock := m.tasksDock(); dock != "" {
		b.WriteString(dock + "\n")
	}
	if m.rew != nil {
		b.WriteString(m.rewindView() + "\n\n")
	}
	if m.iactive == nil {
		if m.namePrompt != nil {
			b.WriteString(m.namePrompt.label + " ")
			if m.namePrompt.mask {
				// Secrets never echo: render the mask instead of the input's
				// live view (which would show the key in the clear). The "┃ "
				// prompt matches how the textarea renders its own first line.
				b.WriteString("┃ " + m.namePrompt.maskedValue(m.input.Value()))
			} else {
				b.WriteString(m.input.View())
			}
		} else {
			b.WriteString(m.input.View())
		}
	}
	if m.quit1 {
		// first idle ctrl+c armed the quit; make the second press discoverable
		b.WriteString("\n" + errStyle.Render("press ctrl+c again to quit"))
	}
	if m.escClr {
		b.WriteString("\n" + errStyle.Render("esc again: clear the input (↑ recalls it)"))
	} else if m.esc1 && m.rew == nil && m.namePrompt == nil {
		b.WriteString("\n" + dimStyle.Render("esc again: rewind the conversation"))
	}
	if m.menu != nil {
		b.WriteString("\n" + m.menuView())
	}
	b.WriteString("\n\n" + m.statusView()) // persistent status box, with a blank line above
	return b.String()
}

const (
	statusBoxRows     = 3
	statusCellPadding = 1
)

// statusInfoRow is the middle row of the three-row status box.
func statusInfoRow(height int) int {
	return height - statusBoxRows + 1
}

type statusField struct {
	text  string
	width int
}

// statusView renders the always-on status box below the input. The model,
// effort, and mode fields are interactive hitboxes; the rest is session
// context that stays put while the transcript scrolls.
func (m *model) statusView() string {
	model := m.modelName
	contextSize := m.contextStatus()
	if cost, ok := m.sessionCost(); ok {
		contextSize += " · " + fmtCost(cost)
	}
	mode := m.uiMode()
	effort := "(" + effortLabel(m.currentEffort()) + ")"
	folder := m.shortCWD
	if folder == "" {
		folder = shortCWD()
	}
	slotW := m.modelSlotW
	if slotW <= 0 {
		slotW = m.statusModelSlotWidth()
	}
	fields := []statusField{
		{text: folder, width: ansi.StringWidth(folder)},
		{text: model, width: slotW},
		{text: effort, width: ansi.StringWidth(effort)},
		{text: mode, width: ansi.StringWidth(mode)},
		{text: m.provName, width: ansi.StringWidth(m.provName)},
		{text: contextSize, width: ansi.StringWidth(contextSize)},
	}
	padding := statusCellPadding
	if m.width > 0 && statusFieldsWidth(fields, padding) > m.width {
		// Drop cell padding before truncating any content. This keeps the full
		// route and the context size visible in ordinary narrow terminals.
		padding = 0
	}
	if m.width > 0 {
		shrinkStatusFields(fields, max(m.width-statusFieldsOverhead(len(fields), padding), 0))
	}
	row, starts := renderStatusBox(fields, padding, m.width)
	m.statusModelX, m.statusModelW = 0, 0
	m.statusEffortX, m.statusEffortW = 0, 0
	m.statusModeX, m.statusModeW = 0, 0
	if len(starts) > 1 && statusFieldFits(starts[1], fields[1].width, m.width) {
		m.statusModelX, m.statusModelW = starts[1], fields[1].width
	}
	if len(starts) > 2 && statusFieldFits(starts[2], fields[2].width, m.width) {
		m.statusEffortX, m.statusEffortW = starts[2], fields[2].width
	}
	if len(starts) > 3 && statusFieldFits(starts[3], fields[3].width, m.width) {
		m.statusModeX, m.statusModeW = starts[3], fields[3].width
	}
	return dimStyle.Render(row)
}

func statusFieldFits(start, fieldWidth, boxWidth int) bool {
	if start < 1 || fieldWidth <= 0 {
		return false
	}
	if boxWidth <= 0 {
		return true
	}
	return start+fieldWidth <= boxWidth-1
}

func statusSegment(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(s)
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	if w > width {
		return ansi.Truncate(s, width, "…")
	}
	return s
}

func (m *model) statusModelSlotWidth() int {
	modelW := ansi.StringWidth(m.modelName)
	for _, role := range []string{config.RoleSmart, config.RoleDefault, config.RoleFast, config.RoleTiny} {
		target, err := m.roleRoute(role)
		if err != nil {
			continue
		}
		modelW = max(modelW, ansi.StringWidth(target.Model))
	}
	return modelW
}

func statusFieldsOverhead(n, padding int) int {
	if n == 0 {
		return 0
	}
	return 2 + 2*padding*n + n - 1 // outer borders, cell padding, separators
}

func statusFieldsWidth(fields []statusField, padding int) int {
	total := statusFieldsOverhead(len(fields), padding)
	for _, field := range fields {
		total += field.width
	}
	return total
}

// shrinkStatusFields preserves the controls and context size by shortening
// lower-priority context fields first when the terminal cannot fit the box.
func shrinkStatusFields(fields []statusField, available int) {
	if total := statusFieldsContentWidth(fields); total > available {
		remaining := total - available
		// Folder and provider are context; the model slot follows them. Keep
		// effort, mode, and context size intact for as long as the terminal permits.
		for _, i := range []int{0, 4, 1, 2, 3, 5} {
			if remaining == 0 {
				break
			}
			minWidth := 0
			if i == 2 || i == 3 {
				minWidth = 1
			}
			cut := min(fields[i].width-minWidth, remaining)
			if cut <= 0 {
				continue
			}
			fields[i].width -= cut
			remaining -= cut
		}
	}
}

func statusFieldsContentWidth(fields []statusField) int {
	total := 0
	for _, field := range fields {
		total += field.width
	}
	return total
}

// renderStatusBox returns the three rows and the content start column for
// every field. The starts are screen columns, including the left border.
func renderStatusBox(fields []statusField, padding, width int) (string, []int) {
	starts := make([]int, len(fields))
	var row strings.Builder
	row.WriteString("│")
	column := 1
	for i, field := range fields {
		row.WriteString(strings.Repeat(" ", padding))
		column += padding
		starts[i] = column
		row.WriteString(statusSegment(field.text, field.width))
		column += field.width
		row.WriteString(strings.Repeat(" ", padding))
		column += padding
		if i < len(fields)-1 {
			row.WriteString("│")
			column++
		}
	}
	if width > 0 {
		if width < 2 {
			short := ansi.Truncate(row.String(), width, "…")
			return short + "\n" + short + "\n" + short, starts
		}
		if target := width - 1; column < target {
			row.WriteString(strings.Repeat(" ", target-column))
			column = target
		}
	}
	row.WriteString("│")
	boxWidth := column + 1
	rowText := row.String()
	if width > 0 {
		switch {
		case width == 1:
			return "┌\n│\n└", nil
		case boxWidth > width:
			inner := strings.TrimSuffix(strings.TrimPrefix(rowText, "│"), "│")
			inner = ansi.Truncate(inner, width-2, "")
			if gap := width - 2 - ansi.StringWidth(inner); gap > 0 {
				inner += strings.Repeat(" ", gap)
			}
			rowText = "│" + inner + "│"
			boxWidth = width
		case boxWidth < width:
			rowText += strings.Repeat(" ", width-boxWidth)
			boxWidth = width
		}
	}
	top := "┌" + strings.Repeat("─", boxWidth-2) + "┐"
	bottom := "└" + strings.Repeat("─", boxWidth-2) + "┘"
	return top + "\n" + rowText + "\n" + bottom, starts
}

// shortCWD renders the working directory compactly for the status line: the
// home directory collapses to ~ and only the last two path segments survive,
// so a deep path doesn't crowd out the rest of the status.
func shortCWD() string {
	dir := cwd()
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home) {
		dir = "~" + strings.TrimPrefix(dir, home)
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	if len(parts) > 3 {
		return "…/" + strings.Join(parts[len(parts)-3:], "/")
	}
	return dir
}

func (m *model) menuView() string {
	// window of menuRows candidates around the selection
	start := 0
	if m.menu.idx >= menuRows {
		start = m.menu.idx - menuRows + 1
	}
	end := min(start+menuRows, len(m.menu.cands))

	nameW := 0
	for _, c := range m.menu.cands[start:end] {
		nameW = max(nameW, ansi.StringWidth(c.Text))
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		c := m.menu.cands[i]
		pad := max(0, nameW-ansi.StringWidth(c.Text))
		line := c.Text + strings.Repeat(" ", pad) + "  "
		if i == m.menu.idx {
			b.WriteString(botStyle.Render("→ "+line) + dimStyle.Render(c.Desc))
		} else {
			b.WriteString("  " + line + dimStyle.Render(c.Desc))
		}
		b.WriteByte('\n')
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("  (%d/%d)", m.menu.idx+1, len(m.menu.cands))))
	return b.String()
}

func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Wrap(s, width, " ") // word-aware: break at spaces, not mid-token
}

func truncLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}
