package tui

import (
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// textPosition is a display-cell position in the unpadded transcript.
// Columns are terminal cells, not byte or rune offsets, so wide Unicode text
// can be selected without splitting a grapheme.
type textPosition struct {
	row int
	col int
}

type selectionState struct {
	anchor, focus textPosition
	dragging      bool
	pendingTool   int
	edgeDir       int
	edgeX, edgeY  int
	edgePending   bool
}

func (s *selectionState) hasRange() bool {
	return s != nil && s.anchor != s.focus
}

func orderedSelection(a, b textPosition) (textPosition, textPosition) {
	if a.row < b.row || (a.row == b.row && a.col <= b.col) {
		return a, b
	}
	return b, a
}

func (m *model) ensurePlainRows() []string {
	if m.plainRows == nil {
		m.plainRows = strings.Split(ansi.Strip(m.viewportContent), "\n")
	}
	return m.plainRows
}

// transcriptPosition maps the screen coordinate used by the main viewport to
// a transcript row. The top two screen rows belong to the header and its
// separator; contentPad is not part of the selectable transcript.
func (m *model) transcriptPosition(x, y int) (textPosition, bool) {
	if x < 0 || y < 2 || m.vp.Height <= 0 || y >= 2+m.vp.Height {
		return textPosition{}, false
	}
	paddedRow := m.vp.YOffset + y - 2
	pad := m.contentPad()
	row := paddedRow - pad
	rows := m.ensurePlainRows()
	if row < 0 || paddedRow < 0 || paddedRow >= len(rows) {
		return textPosition{}, false
	}
	width := ansi.StringWidth(rows[paddedRow])
	return textPosition{row: row, col: min(x, width)}, true
}

func (m *model) transcriptRow(row int) (string, bool) {
	idx := row + m.contentPad()
	rows := m.ensurePlainRows()
	if idx < 0 || idx >= len(rows) {
		return "", false
	}
	return rows[idx], true
}

func selectionColumns(row int, s *selectionState, lineWidth int) (int, int, bool) {
	if s == nil || !s.hasRange() || row < min(s.anchor.row, s.focus.row) || row > max(s.anchor.row, s.focus.row) {
		return 0, 0, false
	}
	start, end := orderedSelection(s.anchor, s.focus)
	left, right := 0, lineWidth
	if row == start.row {
		left = start.col
	}
	if row == end.row {
		right = end.col
	}
	left = max(0, min(left, lineWidth))
	right = max(0, min(right, lineWidth))
	return left, right, right > left
}

func (m *model) selectedText() string {
	if m.selection == nil || !m.selection.hasRange() {
		return ""
	}
	start, end := orderedSelection(m.selection.anchor, m.selection.focus)
	parts := make([]string, 0, end.row-start.row+1)
	for row := start.row; row <= end.row; row++ {
		line, ok := m.transcriptRow(row)
		if !ok {
			continue
		}
		width := ansi.StringWidth(line)
		left, right := 0, width
		if row == start.row {
			left = start.col
		}
		if row == end.row {
			right = end.col
		}
		left = max(0, min(left, width))
		right = max(0, min(right, width))
		parts = append(parts, ansi.Cut(line, left, right))
	}
	return strings.Join(parts, "\n")
}

var selectionStyle = lipgloss.NewStyle().Reverse(true)

func (m *model) selectedViewportView(view string) string {
	rows := m.ensurePlainRows()
	pad := m.contentPad()
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		paddedRow := m.vp.YOffset + i
		if paddedRow < 0 || paddedRow >= len(rows) {
			continue
		}
		plain := rows[paddedRow]
		left, right, ok := selectionColumns(paddedRow-pad, m.selection, ansi.StringWidth(plain))
		if !ok {
			continue
		}
		width := ansi.StringWidth(line)
		left = min(left, width)
		right = min(right, width)
		if right <= left {
			continue
		}
		before := ansi.Cut(line, 0, left)
		selected := ansi.Cut(plain, left, right)
		after := ansi.Cut(line, right, width)
		lines[i] = before + selectionStyle.Render(selected) + after
	}
	return strings.Join(lines, "\n")
}

func (m *model) blockAtTranscriptRow(row int) int {
	for i := range m.blocks {
		if row >= m.blocks[i].y0 && row <= m.blocks[i].y1 {
			return i
		}
	}
	return -1
}

// handleTranscriptMouse owns click/drag gestures in the main transcript.
// A press only records intent; release distinguishes a stationary tool click
// from a drag and lets the latter copy asynchronously.
func (m *model) handleTranscriptMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		pos, ok := m.transcriptPosition(msg.X, msg.Y)
		if !ok {
			return false, nil
		}
		m.selection = &selectionState{
			anchor:      pos,
			focus:       pos,
			pendingTool: m.blockAtTranscriptRow(pos.row),
		}
		return true, nil
	}
	if m.selection == nil {
		return false, nil
	}

	switch msg.Action {
	case tea.MouseActionMotion:
		if msg.Button != tea.MouseButtonLeft {
			return true, nil
		}
		if pos, ok := m.transcriptPosition(msg.X, msg.Y); ok {
			if pos != m.selection.anchor {
				m.selection.dragging = true
			}
			m.selection.focus = pos
		}
		return true, m.updateSelectionEdge(msg)

	case tea.MouseActionRelease:
		if msg.Button != tea.MouseButtonLeft && msg.Button != tea.MouseButtonNone {
			return true, nil
		}
		if pos, ok := m.transcriptPosition(msg.X, msg.Y); ok {
			if pos != m.selection.anchor {
				m.selection.dragging = true
			}
			m.selection.focus = pos
		}
		m.selection.edgeDir = 0
		m.selection.edgePending = false
		if m.selection.dragging && m.selection.hasRange() {
			text := m.selectedText()
			if text != "" {
				return true, copySelection(text)
			}
			m.selection = nil
			return true, nil
		}
		tool := m.selection.pendingTool
		m.selection = nil
		if tool >= 0 && tool < len(m.blocks) && m.blocks[tool].toggle() {
			m.refreshVP()
		}
		return true, nil
	}
	return false, nil
}

func (m *model) updateSelectionEdge(msg tea.MouseMsg) tea.Cmd {
	if m.selection == nil || !m.selection.dragging {
		return nil
	}
	dir := 0
	first := 2
	last := first + m.vp.Height - 1
	if msg.Y <= first {
		dir = -1
	} else if msg.Y >= last {
		dir = 1
	}
	if dir == 0 {
		m.selection.edgeDir = 0
		m.selection.edgePending = false
		return nil
	}
	m.selection.edgeDir = dir
	m.selection.edgeX, m.selection.edgeY = msg.X, msg.Y
	if m.selection.edgePending {
		return nil
	}
	m.selection.edgePending = true
	return selectionEdgeTick()
}

func (m *model) selectionEdgeTick() tea.Cmd {
	if m.selection == nil || m.selection.edgeDir == 0 {
		return nil
	}
	if m.selection.edgeDir < 0 {
		if m.vp.AtTop() {
			m.selection.edgeDir = 0
			m.selection.edgePending = false
			return nil
		}
		m.vp.ScrollUp(1)
	} else {
		if m.vp.AtBottom() {
			m.selection.edgeDir = 0
			m.selection.edgePending = false
			return nil
		}
		m.vp.ScrollDown(1)
	}
	m.follow = m.vp.AtBottom()
	if pos, ok := m.transcriptPosition(m.selection.edgeX, m.selection.edgeY); ok {
		m.selection.focus = pos
	}
	return selectionEdgeTick()
}

type selectionCopyMsg struct{ err error }
type selectionEdgeMsg struct{}

func copySelection(text string) tea.Cmd {
	return func() tea.Msg {
		return selectionCopyMsg{err: clipboard.WriteAll(text)}
	}
}

func selectionEdgeTick() tea.Cmd {
	return tea.Tick(16*time.Millisecond, func(time.Time) tea.Msg { return selectionEdgeMsg{} })
}
