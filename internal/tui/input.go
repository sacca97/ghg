package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// newInput builds the prompt textarea with ghg's keybindings and styling.
// Newlines come from ctrl+j / shift+enter / alt+enter; plain enter submits.
func newInput() textarea.Model {
	ti := textarea.New()
	ti.Placeholder = "Ask ghg anything… (/ for commands, tab completes, ctrl+p for settings)"
	ti.Prompt = "┃ "
	ti.SetHeight(1)
	ti.MaxHeight = 24 // input grows with content up to this many lines
	ti.ShowLineNumbers = false
	ti.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j", "shift+enter", "alt+enter"),
		key.WithHelp("ctrl+j", "newline"),
	)
	// ctrl+k clears the conversation (handled in (*model).key); don't let the
	// textarea's default delete-after-cursor shadow it.
	ti.KeyMap.DeleteAfterCursor = key.NewBinding()
	// The default adaptive styles misdetect the background over mosh/tmux;
	// use plain ANSI colors and no cursor-line background.
	ti.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ti.FocusedStyle.Placeholder = dimStyle
	ti.BlurredStyle.Placeholder = dimStyle
	ti.FocusedStyle.Prompt = botStyle
	ti.BlurredStyle.Prompt = dimStyle
	ti.Focus()
	return ti
}

// inputContentHeight returns the number of lines the input needs to show its
// whole value, wrapping each logical line the way the textarea does (at the
// content width, which excludes the "┃ " prompt). We must compute this from
// the value, not View(): the textarea clamps View() to its current height, so
// measuring it can never grow the box.
func (m *model) inputContentHeight() int {
	contentWidth := m.input.Width() - 2 // minus the "┃ " prompt
	if contentWidth < 1 {
		contentWidth = 1
	}
	h := 0
	for _, line := range strings.Split(m.input.Value(), "\n") {
		h += max(1, (lipgloss.Width(line)+contentWidth-1)/contentWidth)
	}
	return h
}

// growInput resizes the input box to fit its content (capped at MaxHeight).
// When the box grows, the textarea's internal viewport keeps the scroll offset
// it computed for the smaller height — repositionView only ever scrolls down
// to follow the cursor, never back up — so the top lines would be clipped out
// of view. The textarea doesn't expose its viewport, so on growth we rebuild
// it at the new height (a fresh viewport starts at the top), preserving the
// content and cursor-at-end.
func (m *model) growInput() {
	if m.width <= 0 {
		return
	}
	h := max(1, min(m.inputContentHeight(), m.input.MaxHeight))
	if h == m.input.Height() {
		return
	}
	if h < m.input.Height() {
		m.input.SetHeight(h) // shrinking never clips
		return
	}
	val := m.input.Value()
	ti := newInput()
	ti.SetWidth(m.input.Width() + 2) // Width() is content width; SetWidth takes total
	ti.SetHeight(h)
	ti.SetValue(val)
	ti.CursorEnd()
	m.input = ti
}

func (m *model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// interactive passthrough: forward keystrokes to the child's PTY instead
	// of editing the input box. ctrl+c ctrl+c breaks out (cancel), esc forwards
	// a single esc to the child (many prompts use esc to cancel).
	if m.iactive != nil {
		return m.iactiveKey(msg)
	}
	if m.permDialog != nil {
		m.permKey(msg)
		return m, nil
	}
	if m.settings != nil {
		return m.paletteKey(msg)
	}
	if m.rew != nil {
		return m.rewindKey(msg)
	}
	if m.picker != nil {
		return m.pickerKey(msg)
	}
	if m.mpicker != nil {
		return m.modelPickerKey(msg)
	}
	// Keyboard input cancels a pending transcript selection and any edge-scroll
	// tick attached to it.
	m.selection = nil
	// newline keys (ctrl+j / shift+enter / alt+enter) never submit; they go
	// straight to the textarea, which splits the line via InsertNewline.
	// Note: KeyCtrlM is NOT here — it shares KeyEnter's byte (CR=13), so
	// matching it would swallow every real enter keypress. ctrl+j (LF=10),
	// alt+enter, and the shift+enter escape sequences are all distinguishable.
	if msg.Type == tea.KeyCtrlJ ||
		(msg.Type == tea.KeyEnter && msg.Alt) ||
		(msg.Type == tea.KeyRunes && msg.Alt && string(msg.Runes) == "\r") ||
		isShiftEnterSeq(msg) {
		// bubbles gates InsertNewline on MaxHeight, treating the visual cap as
		// a content limit — after a paste reaches MaxHeight lines every ctrl+j
		// would be silently swallowed. Lift the cap for this one call so the
		// newline always lands (and the textarea's own repositionView scrolls
		// the new line into view), then reapply the visual cap via SetHeight,
		// which clamps rendering only, never content.
		cap := m.input.MaxHeight
		m.input.MaxHeight = 0
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
		m.input.MaxHeight = cap
		m.input.SetHeight(cap)
		// bubbles' InsertNewline scrolls the internal viewport to follow the
		// cursor while the box is still 1 line high (YOffset=1); the deferred
		// growInput rebuild inherits that stale offset and the first line
		// scrolls out of view. SetValue resets the scroll (Reset inside), and
		// CursorEnd keeps the caret at the end of the input.
		v := m.input.Value()
		m.input.SetValue(v)
		m.input.CursorEnd()
		m.refreshMenu()
		return m, cmd
	}
	// an open task detail view owns the keyboard until esc backs out of it
	if m.taskVP != nil {
		return m.taskViewKey(msg)
	}
	// Paste collapse (opt-in via config collapsePaste): a multi-line bracketed
	// paste lands as a [Pasted ~N lines] placeholder in the input instead of
	// spraying the textarea; the real text is held in pasteBuf and swapped in
	// at submit. Off by default — a paste you can't see is a paste you can't
	// trust.
	if msg.Paste && m.cfg != nil && m.cfg.CollapsePaste != nil && *m.cfg.CollapsePaste {
		if n := strings.Count(string(msg.Runes), "\n"); n >= 2 {
			m.pasteBuf = string(msg.Runes)
			m.input.SetValue(m.input.Value() + fmt.Sprintf("[Pasted ~%d lines]", n+1))
			m.input.CursorEnd()
			m.growInput()
			return m, nil
		}
	}

	switch msg.Type {
	case tea.KeyCtrlT:
		// focus the tasks dock (or unfocus it) — the persistent strip above
		// the input listing background subagents
		if len(m.dockTasks()) == 0 {
			return m, nil
		}
		m.tasksFocus = !m.tasksFocus
		m.clampTaskSel()
		return m, nil
	case tea.KeyCtrlD:
		return m.command("/detach")
	case tea.KeyCtrlC:
		if m.busy && m.cancel != nil {
			// explicit interruption: first press arms, second cancels
			// ponytail: no reset timer; the flag clears on turn end
			if !m.interrupt1 {
				m.interrupt1 = true
				return m, nil
			}
			m.cancel()
			return m, nil
		}
		// idle: two presses within a short window quit, so a stray ctrl+c
		// can't nuke the session. First press arms + hints; second quits.
		if m.quit1 {
			m.quit1 = false
			return m, tea.Quit
		}
		m.quit1 = true
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return quitArmMsg{} })

	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd

	case tea.KeyEsc:
		// esc interrupts the agent mid-response — UNLESS there's a draft in
		// the input box: clearing the draft takes priority so esc stays
		// predictable (it always edits YOUR text first), and the agent keeps
		// running untouched.
		if m.busy && m.cancel != nil && strings.TrimSpace(m.input.Value()) == "" {
			m.cancel()
			return m, nil
		}
		// Dismissing UI takes priority and only arms the window.
		dismissed := true
		switch {
		case m.namePrompt != nil: // cancel the inline fork/rename/auth prompt
			masked := m.namePrompt.mask
			m.closeNamePrompt()
			if masked { // the draft stash must not record a key into history
				m.escClr = false
				return m, nil
			}
		case m.menu != nil:
			if m.menu.cyc { // tab cycling previewed candidates: revert the input
				m.input.SetValue(m.menu.base)
			}
			m.menu = nil
		case m.queueSel >= 0: // leave queue navigation
			m.queueSel = -1
		case m.tasksFocus: // leave dock navigation, back to the main thread
			m.tasksFocus = false
		default:
			dismissed = false
		}
		if !dismissed {
			// A typed draft: double-esc clears it into the input history (not
			// the chat history — it's recallable with ↑ in case it was an
			// accident). The rewind picker never arms while a draft exists.
			if strings.TrimSpace(m.input.Value()) != "" {
				if m.escClr {
					m.escClr = false
					m.hist = append(m.hist, strings.TrimSpace(m.input.Value()))
					m.histIdx = len(m.hist)
					m.input.Reset()
					m.append(dimStyle.Render("draft cleared — ↑ recalls it"))
					return m, nil
				}
				m.escClr = true
				return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return escArmMsg{} })
			}
			// No draft: a second esc within a second opens the rewind picker —
			// scroll the history, jump back (or forward again after a rewind).
			if m.esc1 {
				m.esc1 = false
				m.openRewind()
				return m, nil
			}
			m.esc1 = true
			return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return escArmMsg{} })
		}
		m.esc1 = false   // a dismissal consumed the press; no stale arm carries over
		m.escClr = false // same for the draft-clear arm
		return m, nil

	case tea.KeyCtrlV:
		// image on the clipboard? save it and @-mention the file; otherwise
		// let the textarea do its usual text paste
		return m, pasteImageCmd

	case tea.KeyCtrlE:
		// expand/collapse the most recent tool result block
		for i := len(m.blocks) - 1; i >= 0; i-- {
			if m.blocks[i].kind == blockTool {
				m.blocks[i].toggle()
				m.refreshVP()
				return m, nil
			}
		}
		return m, nil

	case tea.KeyCtrlO:
		// toggle rendering of reasoning/thinking tokens
		m.toggleThinking()
		return m, nil

	case tea.KeyCtrlK:
		// clear the conversation, exactly as if /clear ran. Intercepted here
		// because the textarea's default KeyMap claims ctrl+k for
		// delete-after-cursor (newInput disables that binding).
		return m.command("/clear")

	case tea.KeyTab:
		// completion menu: tab/shift+tab cycle the selection WITH preview —
		// each step inserts the highlighted candidate (a single match just
		// completes), enter commits, esc dismisses and reverts the input.
		if m.menu != nil {
			m.menuCycle(1)
			return m, nil
		}
		m.openMenu()
		return m, nil

	case tea.KeyDown, tea.KeyCtrlN:
		if m.menu != nil {
			m.menu.idx = (m.menu.idx + 1) % len(m.menu.cands)
			return m, nil
		}
		if m.tasksFocus {
			m.taskSel = min(m.taskSel+1, len(m.dockTasks())-1)
			return m, nil
		}
		// while busy with a queue and an empty input, ↓ moves the queue
		// selection toward newer messages (and off the end to deselect)
		if m.busy && len(m.queue) > 0 && m.input.Value() == "" {
			if m.queueSel >= 0 {
				m.queueSel++
				if m.queueSel >= len(m.queue) {
					m.queueSel = -1
				}
			}
			return m, nil
		}
		// move within the textarea unless the cursor already sits on the
		// last (soft-wrapped) row, where ↓ falls through to history recall
		if !m.cursorOnLastLine() {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		m.histNext()
		return m, nil

	case tea.KeyShiftTab:
		if m.menu != nil {
			m.menuCycle(-1)
			return m, nil
		}
		return m, nil

	case tea.KeyUp, tea.KeyCtrlP:
		if m.menu != nil {
			m.menu.idx = (m.menu.idx + len(m.menu.cands) - 1) % len(m.menu.cands)
			return m, nil
		}
		if m.tasksFocus {
			m.taskSel = max(m.taskSel-1, 0)
			return m, nil
		}
		// while busy with a queue and an empty input, ↑ selects queued messages
		if m.busy && len(m.queue) > 0 && m.input.Value() == "" &&
			(msg.Type == tea.KeyUp || msg.Type == tea.KeyShiftTab) {
			if m.queueSel < 0 {
				m.queueSel = len(m.queue) - 1 // start at the newest
			} else if m.queueSel > 0 {
				m.queueSel--
			}
			return m, nil
		}
		// move within the textarea unless the cursor already sits on the
		// first (soft-wrapped) row, where ↑ falls through to history recall.
		// Holding ↑ auto-repeats at 30–80ms; a user who keeps holding past
		// the top is trying to reach the start of THIS message, not to
		// machine-gun through history — suppress the rollover while repeats
		// keep arriving, and only recall after a deliberate pause.
		if msg.Type == tea.KeyUp && !m.cursorOnFirstLine() {
			m.lastUp = m.nowFn()
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		if msg.Type == tea.KeyUp && m.nowFn().Sub(m.lastUp) < 300*time.Millisecond {
			m.lastUp = m.nowFn()
			return m, nil
		}
		m.lastUp = m.nowFn()
		if msg.Type == tea.KeyCtrlP { // command settings (opencode-style modal)
			m.openPalette()
			return m, nil
		}
		m.histPrev()
		return m, nil

	case tea.KeyDelete, tea.KeyBackspace:
		// delete the selected queued message (only when navigating the queue)
		if m.busy && m.queueSel >= 0 && m.queueSel < len(m.queue) {
			m.queue = append(m.queue[:m.queueSel], m.queue[m.queueSel+1:]...)
			if m.queueSel >= len(m.queue) {
				m.queueSel = len(m.queue) - 1
			}
			if len(m.queue) == 0 {
				m.queueSel = -1
			}
			return m, nil
		}
		// not navigating the queue: fall through to normal editing
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.refreshMenu()
		return m, cmd

	case tea.KeyEnter:
		if m.namePrompt != nil { // inline prompt (fork naming, /rename) commits
			onOK := m.namePrompt.onOK
			value := strings.TrimSpace(m.input.Value())
			m.closeNamePrompt() // restores the draft before onOK appends blocks
			onOK(value)
			return m, nil
		}
		if m.menu != nil {
			c := m.menu.cands[m.menu.idx]
			// a bare command previewed by tab cycling runs immediately, same
			// as picking it with arrows + enter (one-keystroke settings)
			if m.menu.cyc && m.menu.head == "" && execNow[c.Text] {
				m.menu = nil
				m.input.Reset()
				return m.command(c.Text)
			}
			// tab cycling already inserted the candidate: commit it; otherwise
			// insert it now (directories stay open for deeper completion)
			if m.menu.cyc {
				m.acceptPreview()
				return m, nil
			}
			// bare commands that act without further args run immediately
			if m.menu.head == "" && execNow[c.Text] {
				m.menu = nil
				m.input.Reset()
				return m.command(c.Text)
			}
			if m.accept() {
				return m, nil // completed something; next enter submits
			}
			// selection was already fully typed — fall through to submit
		}
		if m.tasksFocus { // open the selected task's detail view
			m.tasksFocus = false
			// dockTasks is time-dependent (settled tasks age out after
			// dockSettledGrace), so the strip can go empty — or shrink below
			// taskSel — between the last paint and this keypress
			if tasks := m.dockTasks(); len(tasks) > 0 {
				m.openTask(tasks[min(m.taskSel, len(tasks)-1)].ID)
			}
			return m, nil
		}
		text := strings.TrimSpace(m.input.Value())
		// a collapsed paste swaps its real text back in at submit
		if m.pasteBuf != "" {
			text = strings.Replace(text, strings.TrimSpace(fmt.Sprintf("[Pasted ~%d lines]", strings.Count(m.pasteBuf, "\n")+1)), strings.TrimSpace(m.pasteBuf), 1)
			m.pasteBuf = ""
		}
		if m.busy {
			switch {
			// settings commands don't touch the turn — run them now instead of
			// queueing them as messages for the model
			case text != "" && busyCmd(text):
				if !strings.HasPrefix(text, "/auth ") { // keys stay out of ↑-recallable history
					m.hist = append(m.hist, text)
					m.histIdx = len(m.hist)
				}
				m.input.Reset()
				m.menu = nil
				return m.command(text)
			case strings.HasPrefix(text, "!"): // shell escape runs now, not queued
				m.hist = append(m.hist, text)
				m.histIdx = len(m.hist)
				m.input.Reset()
				m.menu = nil
				m.runShell(text)
			case text != "": // codex-style: queue it (multiple allowed)
				m.queue = append(m.queue, text)
				m.hist = append(m.hist, text)
				m.histIdx = len(m.hist)
				m.input.Reset()
				m.menu = nil
			case len(m.queue) > 0: // grok-style: empty enter force-steers the queue
				// Interrupt the current generation so the queued messages
				// go out as the next turn immediately, not after the model
				// finishes whatever it's currently generating.
				if m.cancel != nil {
					m.cancel()
				}
			}
			return m, nil
		}
		if text == "" && len(m.queue) > 0 {
			// recovery: a turn that ended without draining (e.g. a wrapped
			// cancellation slipping past turnDoneMsg's check) leaves the
			// queue stranded; empty enter while idle sends the head now
			return m.drainQueueHead()
		}
		if text == "" {
			return m, nil
		}
		m.input.Reset()
		m.menu = nil
		// /auth with an inline key is kept out of input history: the key would
		// otherwise be ↑-recallable and rendered in the clear. The masked
		// prompt (bare /auth) is the recommended path.
		if !strings.HasPrefix(text, "/auth ") {
			m.hist = append(m.hist, text)
			m.histIdx = len(m.hist)
		}
		m.draft = ""
		if strings.HasPrefix(text, "/") {
			return m.command(text)
		}
		if strings.HasPrefix(text, "!") {
			m.runShell(text)
			return m, nil
		}
		return m.submit(text)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.refreshMenu()
	return m, cmd
}

// shiftEnterRe matches the common shift+enter encodings bubbletea doesn't map
// to a named key: CSI u (\x1b[13;2u), modifyOtherKeys (\x1b[27;2;13~), and
// kitty's shifted CR (\x1b[57441u). KeyMsg.String() renders each byte of
// unknown sequences quoted and comma-separated (digits as words), so we match
// the rendered form loosely.
var shiftEnterRe = regexp.MustCompile(
	`'\[', '1', '3', ';', '2', 'u'` + // CSI 13;2u
		`|'\[', '2', '7', ';', '2', ';', '1', '3', '~'` + // CSI 27;2;13~
		`|'\[', 'five', 'seven', 'four', 'four', 'one', 'u'`) // CSI 57441u

// isShiftEnterSeq reports whether msg is a shift+enter sequence bubbletea
// surfaced as an unknown/unmapped key.
func isShiftEnterSeq(msg tea.KeyMsg) bool {
	s := msg.String()
	return strings.HasPrefix(s, "unknown csi sequence:") && shiftEnterRe.MatchString(s)
}

// histPrev/histNext recall submitted inputs with the arrow keys.
func (m *model) histPrev() {
	if len(m.hist) == 0 || m.histIdx == 0 {
		return
	}
	if m.histIdx == len(m.hist) {
		m.draft = m.input.Value()
	}
	m.histIdx--
	m.input.SetValue(m.hist[m.histIdx])
}

func (m *model) histNext() {
	if m.histIdx >= len(m.hist) {
		return
	}
	m.histIdx++
	if m.histIdx == len(m.hist) {
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.hist[m.histIdx])
	}
}

// cursorOnFirstLine reports whether the textarea's cursor sits on the first
// (visual) row. A single logical line that soft-wraps to several rows counts
// as several, so ↑ only rolls over to history from the topmost one.
func (m *model) cursorOnFirstLine() bool {
	if m.input.Line() != 0 {
		return false
	}
	return m.input.LineInfo().RowOffset == 0
}

// cursorOnLastLine reports whether the textarea's cursor sits on the last
// (visual) row, mirroring cursorOnFirstLine for the ↓ edge.
func (m *model) cursorOnLastLine() bool {
	if m.input.Line() != m.input.LineCount()-1 {
		return false
	}
	li := m.input.LineInfo()
	return li.RowOffset >= li.Height-1
}
