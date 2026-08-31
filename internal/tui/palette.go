package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/sacca97/ghg/internal/mcp"
)

// paletteItem is one row in the ctrl+p command settings. It mirrors opencode's
// DialogSelectOption: title + description + category header + a dimmed hint
// (the keybind or slash form — the settings teaches the shortcuts).
//
// Items are interactive: every row either toggles a live setting in place
// (enter, or ←/→ for reversible ones) or opens a sub-panel inside the settings
// where the change is explored and applied without leaving ctrl+p. Nothing
// closes the settings just to make a change — esc backs out instead.
type paletteItem struct {
	title    string // display name, e.g. "Model"
	category string // "Agent", "Session", "Display", "App"

	// dynDesc/dynHint render live state, so the settings always shows the
	// current value instead of a static description.
	dynDesc func(m *model) string
	dynHint func(m *model) string

	suggested bool // pinned into a "Suggested" group when the filter is empty

	// action rows: enter runs it (settings stays open)
	run func(m *model) (tea.Model, tea.Cmd)

	// sub-panel rows: enter/→ drills in (push), esc pops back
	panel func(m *model) *ppanel

	// reversible rows: ←/→ step the value backward/forward without a panel
	stepBack func(m *model)
	stepFwd  func(m *model)
}

// panelKind enumerates the settings's sub-panels.
type panelKind int

const (
	panelModel panelKind = iota
	panelRole
	panelMode
	panelEffort
	panelGoal
	panelCompact
	panelTheme
)

// ppanel is a settings sub-panel: the interactive editor behind a row. Key
// handling switches on kind; the slice fields hold whatever that kind lists
// (models, effort levels, …).
type ppanel struct {
	kind  panelKind
	title string

	items      []modelItem // panelModel: flattened provider/model routes
	idx        int
	role       string // panelModel: user-facing role whose route is edited; empty is a direct switch
	staleHints []string

	levels []string // panelEffort: available levels ("" = off)
	lidx   int

	prepare string // panelGoal: text submitted when the editor closes

	cands []string // panelCompact: model names from config
	list  []string // panelCompact: "default (…)" + cands; panelTheme: {"auto","light","dark"}
	midx  int      // panelCompact: selection, 0 = the built-in default; panelTheme: selection

	err    string // inline error from a failed apply (bad compact model, …)
	offset int    // first visible rendered row in a scrollable panel

	// direct marks a panel a slash command opened straight into (bare /effort,
	// /theme): enter applies and closes the whole settings instead of popping
	// back to the root list, since the user never asked for ctrl+p.
	direct bool
}

// settings is the ctrl+p command settings: a modal full-screen dialog with its
// own filter line (opencode's DialogSelect). Typing fuzzy-filters, ↑/↓ moves,
// enter applies or drills in, ←/→ steps reversible settings, esc pops a level.
type settings struct {
	items  []paletteItem // filtered
	all    []paletteItem // unfiltered
	idx    int
	filter string
	stack  []*ppanel
	offset int // first visible rendered row in the root list
}

// Hint/keybind constants for the settings-only rows that don't dispatch
// through the command switch. /help renders from these too, so a keybind or
// description lives in exactly one place.
const (
	palHintRewind   = "esc esc"
	palDescRewind   = "rewind the conversation"
	palHintThinking = "ctrl+o"
	palHintQuit     = "ctrl+c ctrl+c"
)

// slashHint looks a command's one-liner up in the registry so the settings
// and /help can never disagree about what a command does.
func slashHint(m *model, name string) string {
	if e := registryFind(name); e != nil {
		return e.Hint
	}
	return name
}

func (m *model) paletteItems() []paletteItem {
	return []paletteItem{
		{title: "Model", category: "Agent", suggested: true,
			// first suggestion: ctrl+p → enter opens the role selector
			dynDesc: func(m *model) string { return m.provName + "/" + m.modelName },
			dynHint: func(m *model) string { return "/model · tab" },
			panel: func(m *model) *ppanel {
				return m.modelRolePanel(false)
			}},
		{title: "Mode", category: "Agent", suggested: true,
			dynDesc: func(m *model) string { return m.uiMode() },
			dynHint: func(m *model) string { return "plan · execute" },
			panel:   func(m *model) *ppanel { return m.modePanel(false) }},
		{title: "Reasoning effort", category: "Agent",
			dynDesc: func(m *model) string {
				if m.agent == nil {
					return "configure a provider first"
				}
				return "thinking level for " + m.agent.Model
			},
			dynHint: func(m *model) string { return "/effort " + slashHint(m, "/effort") },
			panel: func(m *model) *ppanel {
				levels := m.effortsFor()
				pp := &ppanel{kind: panelEffort, title: "Reasoning effort", levels: levels}
				for i, e := range levels {
					if e == m.currentEffort() {
						pp.lidx = i
						break
					}
				}
				return pp
			},
			stepBack: func(m *model) { m.setEffort(prevEffort(m.effortsFor(), m.currentEffort())) },
			stepFwd:  func(m *model) { m.setEffort(nextEffort(m.effortsFor(), m.currentEffort())) }},
		{title: "Plan", category: "Agent", suggested: true,
			dynDesc: func(m *model) string { return slashHint(m, "/plan") },
			dynHint: func(m *model) string { return "/plan <goal>" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				m.input.SetValue("/plan ")
				m.input.CursorEnd()
				m.refreshMenu()
				return m, nil
			}},
		{title: "Execute plan", category: "Agent", suggested: true,
			dynDesc: func(m *model) string { return slashHint(m, "/execute") },
			dynHint: func(m *model) string { return "/execute" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.command("/execute")
			}},
		{title: "Resume session", category: "Session", suggested: true,
			dynDesc: func(m *model) string { return slashHint(m, "/resume") },
			dynHint: func(m *model) string { return "/resume" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				m.openPicker()
				return m, nil
			}},
		{title: "Rewind conversation", category: "Session", suggested: true,
			dynDesc: func(m *model) string {
				if len(m.future) > 0 {
					return "rewound — browse to go back further or forward again"
				}
				return "jump back (or forward) to any earlier message"
			},
			dynHint: func(m *model) string { return palHintRewind },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				if m.busy {
					return m, nil
				}
				m.openRewind()
				return m, nil
			}},
		{title: "Fork session", category: "Session",
			dynDesc: func(m *model) string { return slashHint(m, "/fork") },
			dynHint: func(m *model) string { return "/fork" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				if !m.busy {
					m.forkCommand("")
				}
				return m, nil
			}},
		{title: "Export chat", category: "Session",
			dynDesc: func(m *model) string { return "export the full conversation to Markdown or JSON" },
			dynHint: func(m *model) string { return "/export-result chat" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.exportResultCommand("/export-result chat")
			}},
		{title: "Export latest plan", category: "Session",
			dynDesc: func(m *model) string { return "export latest plan to Markdown or JSON" },
			dynHint: func(m *model) string { return "/export-result plan" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.exportResultCommand("/export-result plan")
			}},
		{title: "Export latest review", category: "Session",
			dynDesc: func(m *model) string { return "export latest review to Markdown or JSON" },
			dynHint: func(m *model) string { return "/export-result review" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.exportResultCommand("/export-result review")
			}},
		{title: "Export last message", category: "Session",
			dynDesc: func(m *model) string { return "export last assistant reply to a file" },
			dynHint: func(m *model) string { return "/export-result last" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.exportResultCommand("/export-result last")
			}},
		{title: "Export workflow result", category: "Session",
			dynDesc: func(m *model) string { return slashHint(m, "/export-result") },
			dynHint: func(m *model) string { return "/export-result" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				m.input.SetValue("/export-result ")
				m.input.CursorEnd()
				m.refreshMenu()
				return m, nil
			}},
		{title: "Rename session", category: "Session",
			dynDesc: func(m *model) string {
				if m.sessionID == "" || m.store == nil {
					return "retitle this session"
				}
				if meta, _, err := m.store.Load(m.sessionID); err == nil && meta.Title != "" {
					return meta.Title
				}
				return "retitle this session"
			},
			dynHint: func(m *model) string { return "/rename " + slashHint(m, "/rename") },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				if !m.busy {
					m.renameCommand("")
				}
				return m, nil
			}},
		{title: "New session", category: "Session",
			dynDesc: func(m *model) string { return slashHint(m, "/clear") },
			dynHint: func(m *model) string { return "/clear" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.command("/clear")
			}},
		{title: "Compact session", category: "Session", suggested: true,
			dynDesc: func(m *model) string { return slashHint(m, "/compact") },
			dynHint: func(m *model) string { return "/compact" },
			run:     func(m *model) (tea.Model, tea.Cmd) { return m.command("/compact") }},
		{title: "Context doctor", category: "Session",
			dynDesc: func(m *model) string { return slashHint(m, "/context-doctor") },
			dynHint: func(m *model) string { return "/context-doctor" },
			run:     func(m *model) (tea.Model, tea.Cmd) { return m.command("/context-doctor") }},
		{title: "Bug report", category: "Session",
			dynDesc: func(m *model) string { return slashHint(m, "/report") },
			dynHint: func(m *model) string { return "/report" },
			run:     func(m *model) (tea.Model, tea.Cmd) { return m.command("/report") }},
		{title: "MCP servers", category: "Session",
			dynDesc: func(m *model) string { return slashHint(m, "/mcp") }, // live count: [n/n ready] badge
			dynHint: func(m *model) string { return "/mcp" },
			run:     func(m *model) (tea.Model, tea.Cmd) { return m.command("/mcp") }},
		{title: "Compaction model", category: "Session",
			dynDesc: func(m *model) string {
				if m.compactModel == "" {
					return "default (" + m.defaultCompactModelName() + ")"
				}
				return m.compactModel
			},
			dynHint: func(m *model) string { return "/compact <model>" },
			panel: func(m *model) *ppanel {
				names := make([]string, 0, len(m.cfg.Models))
				for name := range m.cfg.Models {
					names = append(names, name)
				}
				sort.Strings(names)
				pp := &ppanel{
					kind:  panelCompact,
					title: "Compaction model",
					cands: names,
					list:  append([]string{"default (" + m.defaultCompactModelName() + ")"}, names...),
				}
				for i, name := range pp.list {
					if name == m.compactModel {
						pp.midx = i
						break
					}
				}
				return pp
			}},
		{title: "Compaction level", category: "Session",
			dynDesc: func(m *model) string {
				return "auto-compact at this share of the context window"
			},
			dynHint:  func(m *model) string { return "←/→" },
			stepBack: func(m *model) { m.setCompactPct(m.compactPct() - 10) },
			stepFwd:  func(m *model) { m.setCompactPct(m.compactPct() + 10) }},
		{title: "Goal", category: "Session",
			dynDesc: func(m *model) string {
				if m.goal == "" {
					return fmt.Sprintf("keep working until the goal is met (max %d rounds)", m.goalMaxRounds())
				}
				return truncLine(m.goal, 40)
			},
			dynHint: func(m *model) string { return "/goal " + slashHint(m, "/goal") },
			panel: func(m *model) *ppanel {
				pp := &ppanel{kind: panelGoal, title: "Goal", prepare: m.goal}
				return pp
			}},
		{title: "Thinking timer", category: "Display",
			dynDesc: func(m *model) string { return "show or hide elapsed thinking time" },
			dynHint: func(m *model) string { return palHintThinking },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.toggleThinking()
				return m, nil
			},
			stepBack: func(m *model) { m.setThinking(false) },
			stepFwd:  func(m *model) { m.setThinking(true) }},
		{title: "Theme", category: "Display",
			dynDesc: func(m *model) string { return "current: " + CurrentTheme() },
			dynHint: func(m *model) string { return "/theme " + slashHint(m, "/theme") },
			panel: func(m *model) *ppanel {
				list := []string{"auto", "light", "dark"}
				cur := m.cfg.Theme
				if cur == "" {
					cur = "auto"
				}
				pp := &ppanel{kind: panelTheme, title: "Theme", list: list}
				for i, t := range list {
					if t == cur {
						pp.midx = i
						break
					}
				}
				return pp
			},
			stepBack: func(m *model) { m.setTheme("light") },
			stepFwd:  func(m *model) { m.setTheme("dark") }},
		{title: "Mouse capture", category: "Display",
			dynDesc: func(m *model) string { return slashHint(m, "/mouse") },
			dynHint: func(m *model) string { return "/mouse" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				return m.command("/mouse")
			},
			stepBack: func(m *model) { m.setMouse(false) },
			stepFwd:  func(m *model) { m.setMouse(true) }},
		{title: "Help", category: "App",
			dynDesc: func(m *model) string { return slashHint(m, "/help") },
			dynHint: func(m *model) string { return "/help" },
			run: func(m *model) (tea.Model, tea.Cmd) {
				m.settings = nil
				return m.command("/help")
			}},
		{title: "Quit", category: "App",
			dynDesc: func(m *model) string { return "exit ghg" },
			dynHint: func(m *model) string { return "/quit · " + palHintQuit },
			run:     func(m *model) (tea.Model, tea.Cmd) { return m, tea.Quit }},
	}
}

// setMouse applies a mouse-capture state (the settings's reversible steppers
// need to set an explicit value; /mouse toggles).
func (m *model) setMouse(on bool) {
	if m.mouseOn == on {
		return
	}
	m.command("/mouse")
}

func (m *model) openPalette() {
	all := m.paletteItems()
	m.settings = &settings{all: all}
	m.settings.applyFilter(m)
}

// openPaletteOn opens the settings and drills straight into the named row's
// sub-panel (used by bare slash commands like /theme that should land on a
// switcher, not toggle blindly). The invocation counts as being inside the
// panel — not the settings — so enter applies AND closes; esc pops back to
// the root list.
func (m *model) openPaletteOn(title string) {
	m.openPalette()
	for i, it := range m.settings.items {
		if strings.EqualFold(it.title, title) && it.panel != nil {
			m.settings.idx = i
			pp := it.panel(m)
			pp.direct = true
			m.settings.stack = append(m.settings.stack, pp)
			return
		}
	}
}

// paletteFilterMatch is a cheap fuzzy match: all query runes must appear in
// order across title+category (case-insensitive). Good enough for ~10 rows
// without pulling in fuzzysort.
func paletteFilterMatch(query, hay string) bool {
	if query == "" {
		return true
	}
	hay = strings.ToLower(hay)
	for _, r := range strings.ToLower(query) {
		i := strings.IndexRune(hay, r)
		if i < 0 {
			return false
		}
		hay = hay[i+1:]
	}
	return true
}

// itemHaystack is the text a filter query matches against: title, category,
// and the slash name (not the hint's usage text — "new sess" shouldn't match
// /goal's "resume" in its hint).
func itemHaystack(m *model, it paletteItem) string {
	s := it.title + " " + it.category
	if it.dynHint != nil {
		if f := strings.Fields(it.dynHint(m)); len(f) > 0 && strings.HasPrefix(f[0], "/") {
			s += " " + f[0]
		}
	}
	return s
}

// applyFilter recomputes the visible rows. With an empty filter,
// suggested entries pin into a "Suggested" category on top (opencode), then
// everything else grouped by category.
func (p *settings) applyFilter(m *model) {
	q := p.filter
	var items []paletteItem
	for _, it := range p.all {
		if paletteFilterMatch(q, itemHaystack(m, it)) {
			items = append(items, it)
		}
	}
	// stable category grouping (first-seen order)
	seen := map[string]bool{}
	var cats []string
	for _, it := range items {
		if !seen[it.category] {
			seen[it.category] = true
			cats = append(cats, it.category)
		}
	}
	var grouped []paletteItem
	for _, c := range cats {
		for _, it := range items {
			if it.category == c {
				grouped = append(grouped, it)
			}
		}
	}
	if q == "" {
		var sugg []paletteItem
		for _, it := range grouped {
			if it.suggested {
				sugg = append(sugg, it)
			}
		}
		if len(sugg) > 0 {
			for i := range sugg {
				sugg[i].category = "Suggested"
			}
			grouped = append(sugg, grouped...)
		}
	}
	p.items = grouped
	p.offset = 0
	if p.idx >= len(p.items) {
		p.idx = max(len(p.items)-1, 0)
	}
}

// selected returns the highlighted row (nil when the filter matched nothing).
func (p *settings) selected() *paletteItem {
	if len(p.items) == 0 {
		return nil
	}
	return &p.items[p.idx]
}

// top returns the active sub-panel (nil = the root command list).
func (p *settings) top() *ppanel {
	if len(p.stack) == 0 {
		return nil
	}
	return p.stack[len(p.stack)-1]
}

// move moves the root-list selection by delta, wrapping at both ends.
func (p *settings) move(delta int) {
	n := len(p.items)
	if n == 0 {
		return
	}
	p.idx = (p.idx + delta + n) % n
}

// paletteKey handles input while the settings is open: esc pops one level
// (sub-panel → root list → closed), typing edits the filter or the active
// sub-panel's editor.
func (m *model) paletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.settings
	if p == nil {
		return m, nil
	}
	if pp := p.top(); pp != nil {
		tm, cmd := m.panelKey(msg, pp)
		m.ensurePaletteVisible()
		return tm, cmd
	}
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.settings = nil
	case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
		p.move(-1)
	case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
		p.move(1)
	case tea.KeyLeft:
		if it := p.selected(); it != nil && it.stepBack != nil {
			it.stepBack(m)
		}
	case tea.KeyRight:
		it := p.selected()
		if it == nil {
			break
		}
		if it.stepFwd != nil {
			it.stepFwd(m)
		} else if it.panel != nil {
			m.pushPanel(it)
		}
	case tea.KeyEnter:
		it := p.selected()
		if it == nil {
			return m, nil
		}
		switch {
		case it.panel != nil:
			m.pushPanel(it)
		case it.run != nil:
			return it.run(m)
		}
	case tea.KeyBackspace, tea.KeyDelete:
		if len(p.filter) > 0 {
			p.filter = p.filter[:len(p.filter)-1]
			p.applyFilter(m)
		}
	case tea.KeyRunes:
		p.filter += string(msg.Runes)
		p.idx = 0
		p.applyFilter(m)
	}
	if m.settings != nil {
		m.ensurePaletteVisible()
	}
	return m, nil
}

// paletteMouse handles list clicks and scrolling. The modal consumes mouse
// events so the transcript cannot move underneath an open picker.
func (m *model) paletteMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.settings == nil {
		return m, nil
	}
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if pp := m.settings.top(); pp != nil {
			return m.panelMouse(msg.Y, pp)
		}
		return m.paletteRootMouse(msg.Y)
	}
	delta := 0
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		delta = -3
	case tea.MouseButtonWheelDown:
		delta = 3
	default:
		return m, nil
	}
	if pp := m.settings.top(); pp != nil {
		switch pp.kind {
		case panelModel:
			pp.idx = paletteClamp(pp.idx+delta, 0, len(pp.items)-1)
			if len(pp.items) > 0 && pp.role == "" {
				m.previewModel(pp.items[pp.idx])
			}
		case panelRole, panelMode, panelCompact, panelTheme:
			pp.midx = paletteClamp(pp.midx+delta, 0, len(pp.list)-1)
		case panelEffort:
			pp.lidx = paletteClamp(pp.lidx+delta, 0, len(pp.levels)-1)
		}
	} else if len(m.settings.items) > 0 {
		m.settings.idx = paletteClamp(m.settings.idx+delta, 0, len(m.settings.items)-1)
	}
	m.ensurePaletteVisible()
	return m, nil
}

// paletteRootListStart is the first row of the root list inside paletteView.
// The caller adds one for the always-visible TUI header.
func (m *model) paletteRootListStart() int {
	start := 1 // title
	if !m.paletteCompact() {
		start++ // separator below title
	}
	start++ // filter
	if !m.paletteCompact() {
		start++ // separator below filter
	}
	return start
}

// paletteRootMouse selects and activates the root row under a click. Category
// headings, separators, and the footer are intentionally inert.
func (m *model) paletteRootMouse(y int) (tea.Model, tea.Cmd) {
	p := m.settings
	if p == nil {
		return m, nil
	}
	rows, positions := m.paletteRootRows()
	row := y - 1 - m.paletteRootListStart() // account for the TUI header
	if row < 0 {
		return m, nil
	}
	actual := p.offset + row
	if actual < 0 || actual >= len(rows) {
		return m, nil
	}
	for i, position := range positions {
		if position != actual {
			continue
		}
		p.idx = i
		return m.activatePaletteSelection()
	}
	return m, nil
}

// panelMouse selects and commits the panel row under a click. It shares the
// keyboard Enter path so role/model persistence and direct-panel closing keep
// exactly the same behavior regardless of input device.
func (m *model) panelMouse(y int, pp *ppanel) (tea.Model, tea.Cmd) {
	if m.settings == nil || pp == nil {
		return m, nil
	}
	rows, _, footer := m.panelContent(pp)
	cap := m.panelListCapacity(len(footer))
	start := min(max(pp.offset, 0), len(rows))
	visible := min(cap, len(rows)-start)
	panelStart := 1 // title
	if !m.paletteCompact() {
		panelStart++ // separator below title
	}
	row := y - 1 - panelStart // account for the TUI header
	if row < 0 || row >= visible {
		return m, nil
	}
	selected := start + row
	switch pp.kind {
	case panelModel:
		if selected >= len(pp.items) {
			return m, nil // the empty-state row is not selectable
		}
		pp.idx = selected
	case panelRole, panelMode, panelCompact, panelTheme:
		if selected >= len(pp.list) {
			return m, nil
		}
		pp.midx = selected
	case panelEffort:
		if selected >= len(pp.levels) {
			return m, nil
		}
		pp.lidx = selected
	case panelGoal:
		return m, nil // the goal row is an editor, not a button
	}
	return m.panelKey(tea.KeyMsg{Type: tea.KeyEnter}, pp)
}

func (m *model) activatePaletteSelection() (tea.Model, tea.Cmd) {
	if m.settings == nil {
		return m, nil
	}
	it := m.settings.selected()
	if it == nil {
		return m, nil
	}
	if it.panel != nil {
		m.pushPanel(it)
		return m, nil
	}
	if it.run != nil {
		return it.run(m)
	}
	return m, nil
}

func paletteClamp(n, low, high int) int {
	if high < low {
		return low
	}
	return min(max(n, low), high)
}

// pushPanel drills into an item's sub-panel. Items whose setting can't be
// listed (no models configured) fail in place with a transcript note.
func (m *model) pushPanel(it *paletteItem) {
	pp := it.panel(m)
	if pp == nil {
		m.append(errStyle.Render(it.title + ": nothing to choose from (check ~/.ghg/config.json)"))
		return
	}
	m.settings.stack = append(m.settings.stack, pp)
	pp.offset = 0
}

// panelKey routes keys inside a sub-panel: esc applies-and-pops (goal) or
// just pops, ↑/↓ moves, enter applies.
func (m *model) panelKey(msg tea.KeyMsg, pp *ppanel) (tea.Model, tea.Cmd) {
	p := m.settings
	pop := func() {
		p.stack = p.stack[:len(p.stack)-1]
		// a slash command opened this panel directly (bare /effort, /theme):
		// commit-and-close, never land on the root list the user didn't open
		if pp.direct && len(p.stack) == 0 {
			m.settings = nil
		}
	}

	switch pp.kind {
	case panelModel:
		if len(pp.items) == 0 {
			if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
				pop()
			}
			break
		}
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			pop()
		case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
			pp.idx = (pp.idx - 1 + len(pp.items)) % len(pp.items)
			if pp.role == "" {
				m.previewModel(pp.items[pp.idx])
			}
		case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
			pp.idx = (pp.idx + 1) % len(pp.items)
			if pp.role == "" {
				m.previewModel(pp.items[pp.idx])
			}
		case tea.KeyEnter:
			it := pp.items[pp.idx]
			if it.unavailable {
				pp.err = "model unavailable: " + it.unavailableReason
				break
			}
			if pp.role == "" {
				m.switchModel(it.model, it.provider)
				pop()
				break
			}
			if err := m.selectRoleModel(pp.role, it); err != nil {
				pp.err = err.Error()
				break
			}
			if pp.direct {
				m.settings = nil
			} else {
				pop()
			}
		}

	case panelRole:
		if len(pp.list) == 0 {
			if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
				pop()
			}
			break
		}
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			pop()
		case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
			pp.midx = (pp.midx - 1 + len(pp.list)) % len(pp.list)
		case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
			pp.midx = (pp.midx + 1) % len(pp.list)
		case tea.KeyEnter, tea.KeyRight:
			child := m.roleModelPanel(pp.list[pp.midx], pp.direct)
			p.stack = append(p.stack, child)
		}

	case panelMode:
		if len(pp.list) == 0 {
			if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
				pop()
			}
			break
		}
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			pop()
		case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
			pp.midx = (pp.midx - 1 + len(pp.list)) % len(pp.list)
		case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
			pp.midx = (pp.midx + 1) % len(pp.list)
		case tea.KeyLeft, tea.KeyRight, tea.KeyEnter:
			if err := m.setMode(pp.list[pp.midx]); err != nil {
				pp.err = err.Error()
				break
			}
			pp.err = ""
			if msg.Type == tea.KeyEnter {
				pop()
			}
		}

	case panelEffort:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			pop()
		case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
			pp.lidx = (pp.lidx - 1 + len(pp.levels)) % len(pp.levels)
		case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
			pp.lidx = (pp.lidx + 1) % len(pp.levels)
		case tea.KeyLeft, tea.KeyRight, tea.KeyEnter:
			// ←/→ and enter all apply the highlighted level: selecting is the
			// point of the panel, so any confirm key is a commitment
			m.setEffort(pp.levels[pp.lidx])
			if msg.Type == tea.KeyEnter {
				pop()
			}
		}

	case panelCompact:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			pop()
		case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
			pp.midx = (pp.midx - 1 + len(pp.list)) % len(pp.list)
		case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
			pp.midx = (pp.midx + 1) % len(pp.list)
		case tea.KeyLeft, tea.KeyRight, tea.KeyEnter:
			// apply immediately so a bad pick reports its error inline while
			// the panel is still open
			if pp.midx == 0 {
				m.compactCommand([]string{"off"})
				pp.err = ""
			} else {
				name := pp.list[pp.midx]
				args := []string{name}
				if mdl := m.cfg.Models[name]; len(mdl.Providers) > 0 {
					args = append(args, mdl.Providers[0])
				}
				m.compactCommand(args)
				pp.err = ""
				if m.compactModel != name {
					pp.err = "couldn't resolve " + name + " — kept previous"
				}
			}
			if msg.Type == tea.KeyEnter && pp.err == "" {
				pop()
			}
		}

	case panelTheme:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			pop()
		case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab:
			pp.midx = (pp.midx - 1 + len(pp.list)) % len(pp.list)
		case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
			pp.midx = (pp.midx + 1) % len(pp.list)
		case tea.KeyLeft, tea.KeyRight, tea.KeyEnter:
			m.setTheme(pp.list[pp.midx]) // applies live; re-renders the transcript
			if msg.Type == tea.KeyEnter {
				pop()
			}
		}

	case panelGoal:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.commitGoal(pp) // esc applies too — the editor is the goal
			pop()
		case tea.KeyEnter:
			m.commitGoal(pp)
			pop()
		case tea.KeyBackspace, tea.KeyDelete:
			if len(pp.prepare) > 0 {
				pp.prepare = pp.prepare[:len(pp.prepare)-1]
			}
		case tea.KeyRunes, tea.KeySpace:
			pp.prepare += string(msg.Runes)
		}
	}
	return m, nil
}

// previewModel switches live as the model panel browses, without persisting:
// the pick becomes the default only on enter (switchModel).
func (m *model) previewModel(it modelItem) {
	if it.model == m.modelName && it.provider == m.provName {
		return
	}
	ag, mn, pn, err := buildAgent(m.cfg, it.model, it.provider, m.sysPrompt)
	if err != nil {
		return // unresolved routes stay visible but unselectable-feeling
	}
	ag.ReasoningToggle = m.reasoningToggleFor(pn, ag.Model)
	if m.agent != nil {
		ag.Effort = m.agent.Effort
		ag.Messages = append(ag.Messages, m.agent.Messages[1:]...) // carry history
		ag.CompactBackend, ag.CompactModel = m.agent.CompactBackend, m.agent.CompactModel
		ag.CompactProvider, ag.CompactProtocol = m.agent.CompactProvider, m.agent.CompactProtocol
		ag.CompactThreshold = m.agent.CompactThreshold
	} else {
		ag.Effort = m.cfg.DefaultEffort
		if ag.Effort == "" {
			ag.Effort = "medium"
		}
		ag.CompactThreshold = compactThresholdFor(m.cfg)
	}
	m.agent, m.modelName, m.provName = ag, mn, pn
	m.configureArtifactAgent(m.agent)
	m.applyCompactModel()
	m.wireTasks()
	if !slices.Contains(m.effortsFor(), ag.Effort) {
		m.setEffort("") // the previewed model doesn't support the current level
	}
}

// commitGoal applies the goal panel's text: set, clear (empty), or resume.
// Resuming an unchanged goal continues with the check prompt; a fresh or
// edited goal starts at round 0 (mirrors /goal resume vs /goal <text>).
func (m *model) commitGoal(pp *ppanel) {
	goal := strings.TrimSpace(pp.prepare)
	if goal == m.goal {
		if goal != "" && !m.busy {
			m.goalRounds = 0
			m.append(dimStyle.Render("◎ resuming goal: " + goal))
			m.submitGoal(goalContinuePrompt(goal))
		}
		return
	}
	m.setGoal(goal)
	if goal == "" {
		m.append(dimStyle.Render("(goal cleared)"))
		return
	}
	m.append(dimStyle.Render("◎ goal set: " + goal))
	if !m.busy {
		m.submit(goal)
	}
}

// prevEffort mirrors nextEffort in reverse for ← stepping.
func prevEffort(levels []string, cur string) string {
	for i, e := range levels {
		if e == cur {
			return levels[(i-1+len(levels))%len(levels)]
		}
	}
	return levels[0]
}

// paletteBodyHeight is the space below the always-visible TUI header. A zero
// height means a headless test model has not received a WindowSizeMsg yet;
// leave that case unbounded so those models still render the complete dialog.
func (m *model) paletteBodyHeight() int {
	if m.height <= 0 {
		return 0
	}
	return max(m.height-1, 1)
}

func (m *model) paletteCompact() bool {
	h := m.paletteBodyHeight()
	return h > 0 && h <= 12
}

func paletteFitLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

func paletteFitLines(lines []string, width, height int) string {
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = paletteFitLine(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

// paletteRootRows turns the grouped item list into rendered rows and records
// the rendered row occupied by each selectable item. Scrolling by rendered
// rows keeps category headings and blank separators aligned with the list.
func (m *model) paletteRootRows() ([]string, []int) {
	p := m.settings
	rows := make([]string, 0, len(p.items)*2+1)
	positions := make([]int, len(p.items))
	lastCat := ""
	hintW := 0
	for _, it := range p.items {
		if it.dynHint != nil {
			hintW = max(hintW, ansi.StringWidth(it.dynHint(m)))
		}
	}
	for i, it := range p.items {
		if it.category != lastCat {
			if lastCat != "" {
				rows = append(rows, "")
			}
			rows = append(rows, dimStyle.Render("  "+it.category))
			lastCat = it.category
		}
		hint := ""
		if it.dynHint != nil {
			hint = dimStyle.Render(fmt.Sprintf("%*s", hintW, it.dynHint(m)))
		}
		line := " " + it.title
		if it.dynDesc != nil {
			line += dimStyle.Render("  — " + it.dynDesc(m))
		}
		state := paletteState(m, it)
		if i == p.idx {
			rows = append(rows, botStyle.Render("→")+line+state+"  "+hint)
		} else {
			rows = append(rows, " "+line+state+"  "+hint)
		}
		positions[i] = len(rows) - 1
	}
	if len(p.items) == 0 {
		rows = append(rows, dimStyle.Render("  (no matches)"))
	}
	return rows, positions
}

func (m *model) rootListCapacity() int {
	rows, _ := m.paletteRootRows()
	if h := m.paletteBodyHeight(); h > 0 {
		chrome := 6 // title, two separators, filter, separator, footer
		if m.paletteCompact() {
			chrome = 3 // title, filter, footer
		}
		return max(h-chrome, 1)
	}
	return max(len(rows), 1)
}

// ensurePaletteVisible keeps the selected item (or active panel selection) in
// the modal viewport after keyboard navigation, filtering, or resizing.
func (m *model) ensurePaletteVisible() {
	if m.settings == nil {
		return
	}
	if pp := m.settings.top(); pp != nil {
		m.ensurePanelVisible(pp)
		return
	}
	p := m.settings
	rows, positions := m.paletteRootRows()
	cap := m.rootListCapacity()
	selected := 0
	if len(p.items) > 0 {
		selected = positions[min(max(p.idx, 0), len(positions)-1)]
	}
	if selected < p.offset {
		p.offset = selected
	}
	if selected >= p.offset+cap {
		p.offset = selected - cap + 1
	}
	p.offset = min(max(p.offset, 0), max(len(rows)-cap, 0))
}

// paletteView renders the modal dialog: a title bar, the filter line, and a
// scrollable category-grouped list. A sub-panel replaces the root list.
func (m *model) paletteView() string {
	p := m.settings
	if p == nil {
		return ""
	}
	m.ensurePaletteVisible()
	compact := m.paletteCompact()
	lines := []string{" Commands"}
	if pp := p.top(); pp != nil {
		lines[0] = " Commands › " + pp.title
		if !compact {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(m.panelView(pp), "\n")...)
		return paletteFitLines(lines, m.width, m.paletteBodyHeight())
	}
	if p.filter != "" {
		lines[0] += "  — type to filter"
	}
	if !compact {
		lines = append(lines, "")
	}
	lines = append(lines, " "+youStyle.Render("❯ ")+p.filter+dimStyle.Render("█"))
	if !compact {
		lines = append(lines, "")
	}
	rows, _ := m.paletteRootRows()
	cap := m.rootListCapacity()
	start := min(max(p.offset, 0), len(rows))
	end := min(start+cap, len(rows))
	lines = append(lines, rows[start:end]...)
	if !compact {
		lines = append(lines, "")
	}
	more := ""
	if start > 0 {
		more += " ↑ more"
	}
	if end < len(rows) {
		more += " ↓ more"
	}
	lines = append(lines, dimStyle.Render(fmt.Sprintf("  (%d/%d) ↑/↓ select · enter open/apply · ←/→ change · esc close%s",
		min(p.idx+1, len(p.items)), len(p.items), more)))
	return paletteFitLines(lines, m.width, m.paletteBodyHeight())
}

// paletteState renders a row's live value (toggle state, effort level, …).
func paletteState(m *model, it paletteItem) string {
	switch it.title {
	case "Reasoning effort":
		return dimStyle.Render("  [" + effortLabel(m.currentEffort()) + "]")
	case "Thinking timer":
		return dimStyle.Render("  [" + onOff(m.showThinking) + "]")
	case "Mouse capture":
		return dimStyle.Render("  [" + onOff(m.mouseOn) + "]")
	case "Goal":
		if m.goal != "" {
			return dimStyle.Render("  [on]")
		}
	case "Compaction level":
		return dimStyle.Render(fmt.Sprintf("  [%d%%]", m.compactPct()))
	case "MCP servers":
		if m.mcpMgr == nil {
			return ""
		}
		ready, total := 0, 0
		for _, st := range m.mcpMgr.Statuses() {
			total++
			if st.Status == mcp.StatusReady {
				ready++
			}
		}
		return dimStyle.Render(fmt.Sprintf("  [%d/%d ready]", ready, total))
	}
	return ""
}

// panelContent returns selectable rows, the selected row, and fixed footer
// rows for the active sub-panel. Keeping these separate lets large model
// lists scroll without losing the selection or the help line.
func (m *model) panelContent(pp *ppanel) (rows []string, selected int, footer []string) {
	switch pp.kind {
	case panelModel:
		for i, it := range pp.items {
			cur := ""
			currentModel, currentProvider := m.modelName, m.provName
			if pp.role != "" {
				if target, err := m.roleRoute(pp.role); err == nil {
					currentModel, currentProvider = target.Model, target.Provider
				}
			}
			if it.model == currentModel && it.provider == currentProvider {
				cur = dimStyle.Render("  (current)")
			}
			line := modelItemLabel(it)
			if it.fromCatalog || it.unavailable {
				line = dimStyle.Render(line)
			}
			if i == pp.idx {
				selected = len(rows)
				rows = append(rows, botStyle.Render(" → "+line)+cur)
			} else {
				rows = append(rows, "   "+line+cur)
			}
		}
		if len(rows) == 0 {
			rows = append(rows, dimStyle.Render("  (no models from configured providers)"))
		}
		if pp.err != "" {
			footer = append(footer, errStyle.Render("  "+pp.err))
		}
		action := "switch"
		if pp.role != "" {
			action = "save"
		}
		position := 0
		if len(pp.items) > 0 {
			position = pp.idx + 1
		}
		footer = append(footer, "", dimStyle.Render(fmt.Sprintf("  (%d/%d) ↑/↓ select · enter %s · esc back", position, len(pp.items), action)))
		if len(pp.staleHints) > 0 {
			footer = append(footer, dimStyle.Render("  catalog stale for "+strings.Join(pp.staleHints, ", ")+" — /model refresh to pull newly announced models"))
		}

	case panelRole:
		active := m.activeRoleLabel()
		for i, label := range pp.list {
			cur := ""
			if label == active {
				cur = dimStyle.Render("  (current)")
			}
			if i == pp.midx {
				selected = len(rows)
				rows = append(rows, botStyle.Render(" → "+label)+cur)
			} else {
				rows = append(rows, "   "+label+cur)
			}
		}
		footer = []string{"", dimStyle.Render("  ↑/↓ select · enter choose model · esc back")}

	case panelMode:
		for i, mode := range pp.list {
			cur := ""
			if mode == m.uiMode() {
				cur = dimStyle.Render("  (current)")
			}
			if i == pp.midx {
				selected = len(rows)
				rows = append(rows, botStyle.Render(" → "+mode)+cur)
			} else {
				rows = append(rows, "   "+mode+cur)
			}
		}
		if pp.err != "" {
			footer = append(footer, errStyle.Render("  "+pp.err))
		}
		footer = append(footer, "", dimStyle.Render("  ↑/↓ select · enter apply · esc back"))

	case panelEffort:
		for i, e := range pp.levels {
			cur := ""
			if e == m.currentEffort() {
				cur = dimStyle.Render("  (current)")
			}
			if i == pp.lidx {
				selected = len(rows)
				rows = append(rows, botStyle.Render(" → "+effortLabel(e))+cur)
			} else {
				rows = append(rows, "   "+effortLabel(e)+cur)
			}
		}
		if len(rows) == 0 {
			rows = append(rows, dimStyle.Render("  (no effort levels)"))
		}
		footer = []string{"", dimStyle.Render("  ↑/↓ select · enter/←/→ apply · esc back")}

	case panelCompact:
		for i, name := range pp.list {
			cur := ""
			if (i == 0 && m.compactModel == "") || (i > 0 && name == m.compactModel) {
				cur = dimStyle.Render("  (current)")
			}
			if i == pp.midx {
				selected = len(rows)
				rows = append(rows, botStyle.Render(" → "+name)+cur)
			} else {
				rows = append(rows, "   "+name+cur)
			}
		}
		if len(rows) == 0 {
			rows = append(rows, dimStyle.Render("  (no models configured)"))
		}
		if pp.err != "" {
			footer = append(footer, errStyle.Render("  "+pp.err))
		}
		footer = append(footer, "", dimStyle.Render("  ↑/↓ select · enter/←/→ apply · esc back"))

	case panelTheme:
		cur := m.cfg.Theme
		if cur == "" {
			cur = "auto"
		}
		for i, name := range pp.list {
			mark := ""
			if name == cur {
				mark = dimStyle.Render("  (current)")
			}
			if i == pp.midx {
				selected = len(rows)
				rows = append(rows, botStyle.Render(" → "+name)+mark)
			} else {
				rows = append(rows, "   "+name+mark)
			}
		}
		footer = []string{"", dimStyle.Render("  ↑/↓ select · enter/←/→ apply · esc back")}

	case panelGoal:
		rows = []string{" " + youStyle.Render("❯ ") + pp.prepare + dimStyle.Render("█")}
		footer = []string{"", dimStyle.Render(fmt.Sprintf("  type the goal · empty clears · enter/esc apply · max %d rounds (/goal rounds)", m.goalMaxRounds()))}
	}
	return rows, selected, footer
}

func (m *model) panelListCapacity(footerLen int) int {
	if h := m.paletteBodyHeight(); h > 0 {
		// The settings title and its optional separator are outside panelView.
		prefix := 1
		if !m.paletteCompact() {
			prefix++
		}
		return max(h-prefix-footerLen, 1)
	}
	return 1 << 30
}

func (m *model) ensurePanelVisible(pp *ppanel) {
	rows, selected, footer := m.panelContent(pp)
	cap := m.panelListCapacity(len(footer))
	if selected < pp.offset {
		pp.offset = selected
	}
	if selected >= pp.offset+cap {
		pp.offset = selected - cap + 1
	}
	pp.offset = min(max(pp.offset, 0), max(len(rows)-cap, 0))
}

// panelView renders the active sub-panel with a bounded, scrollable list.
func (m *model) panelView(pp *ppanel) string {
	m.ensurePanelVisible(pp)
	rows, _, footer := m.panelContent(pp)
	cap := m.panelListCapacity(len(footer))
	start := min(max(pp.offset, 0), len(rows))
	end := min(start+cap, len(rows))
	visible := slices.Clone(rows[start:end])
	more := ""
	if start > 0 {
		more += " ↑ more"
	}
	if end < len(rows) {
		more += " ↓ more"
	}
	if more != "" && len(footer) > 0 {
		footer[len(footer)-1] += more
	}
	visible = append(visible, footer...)
	return paletteFitLines(visible, m.width, 0)
}
