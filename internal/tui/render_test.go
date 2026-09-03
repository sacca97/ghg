package tui

import (
	"context"
	"encoding/json"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A degenerate WindowSizeMsg (1–4 cols, which a tmux/PTY handshake can emit
// transiently) must not collapse blocks into a one-char-per-line strip.
// blockTool/blockText wrap with no floor, so width 1 renders one character
// per row and — because renders cache per width and only a width *change*
// reflows — the strip persists. refreshVP floors the render width at
// minRenderWidth so the transcript stays readable.
func TestDegenerateWidthDoesNotStrip(t *testing.T) {
	m := compactCmdModel()
	m.appendAssistant("Here is the assistant reply that must stay readable.")
	m.append(dimStyle.Render("  - Liked the post (815 Likes. Liked)"))

	// A bogus narrow size arrives (tmux handshake hiccup).
	tm, _ := m.Update(mkWinSize(1, 30))
	m = tm.(*model)

	// The bug: text collapses to one character per line. The assistant "● "
	// marker is legitimately a single glyph, so look for a *run* of 1-char
	// lines — the signature of the strip — rather than any single short line.
	lines := strings.Split(ansi.Strip(m.vp.View()), "\n")
	var run int
	for _, l := range lines {
		if ansi.StringWidth(strings.TrimRight(l, " ")) == 1 {
			run++
			if run >= 3 {
				t.Fatalf("text collapsed into a one-char-per-line strip at degenerate width (run of %d): %q", run, lines)
			}
		} else {
			run = 0
		}
	}
}

// After the transient narrow size, a resize to the real width still reflows
// correctly (the floor never becomes a stale cached baseline).
func TestDegenerateWidthThenRealResizeReflows(t *testing.T) {
	m := compactCmdModel()
	m.append(dimStyle.Render("  some tool output line that is long enough to wrap at eighty cols"))

	tm, _ := m.Update(mkWinSize(1, 30)) // degenerate
	m = tm.(*model)
	tm, _ = m.Update(mkWinSize(80, 30)) // real width
	m = tm.(*model)

	var maxW int
	for _, l := range strings.Split(ansi.Strip(m.vp.View()), "\n") {
		if w := ansi.StringWidth(l); w > maxW {
			maxW = w
		}
	}
	if maxW > 80 {
		t.Fatalf("line exceeds real width after reflow: %d", maxW)
	}
}

// frameModel is a model with a transcript long enough to fill the fixed-height
// viewport, making any undercounted `chrome` visible in the rendered frame.
func frameModel() *model {
	m := compactCmdModel()
	m.queueSel = -1
	m.width, m.height = 100, 30
	for i := range 80 {
		m.agent.Messages = append(m.agent.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("answer %d padded out a bit", i)})
	}
	m.seedTranscript(m.agent.Messages, 0)
	return m
}

// The rendered frame must never be taller than the terminal. A frame one row
// too tall makes the terminal scroll on every repaint. The visible symptom is
// not "the layout is off" — it is "the mouse wheel is broken", because the
// wheel scrolls the viewport while the repaint shoves the whole frame the
// other way.
//
// layout() computes the viewport height as m.height - chrome, so this is
// really a test that chrome counts every row View() spends outside the
// viewport. It caught chrome missing the trailing blank + three-row status box (+4 in
// every state) and the armed-hint row (+1 more).
func TestFrameNeverExceedsTerminalHeight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *model)
	}{
		{"idle", func(m *model) {}},
		{"busy", func(m *model) { m.busy = true }},
		{"queued messages", func(m *model) { m.queue = []string{"one", "two"} }},
		{"busy with queue", func(m *model) { m.busy = true; m.queue = []string{"one"} }},
		{"multiline input", func(m *model) { m.input.SetValue("a\nb\nc") }},
		{"quit armed", func(m *model) { m.quit1 = true }},
		{"esc armed", func(m *model) { m.esc1 = true }},
		{"clear armed", func(m *model) { m.escClr = true }},
		{"permission modal", func(m *model) {
			m.permDialog = &permDialog{req: tools.GateRequest{Tool: "bash", Command: "git status"}}
		}},
		{"rewind open", func(m *model) {
			m.rew = &rewindState{entries: []rewindEntry{{text: "hello"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := frameModel()
			tc.setup(m)
			m.layout()
			if h := lipgloss.Height(m.View()); h > m.height {
				t.Errorf("frame is %d rows in a %d-row terminal (%+d): layout()'s chrome "+
					"undercounts View()'s non-viewport rows, so the terminal scrolls on "+
					"every repaint and the wheel fights it", h, m.height, h-m.height)
			}
		})
	}
}

// A narrow or not-yet-sized terminal must not produce a taller frame either:
// width 0 happens before the first WindowSizeMsg.
//
// The floor is the fixed chrome itself (header, tips, divider, input, status —
// 9 rows with a one-line input): below that nothing can fit and the frame
// necessarily overflows, so those sizes are out of scope rather than a bug.
func TestFrameFitsAtDegenerateSizes(t *testing.T) {
	for _, size := range [][2]int{{0, 0}, {10, 10}, {20, 12}, {400, 60}} {
		m := frameModel()
		m.width, m.height = size[0], size[1]
		m.layout()
		if h := lipgloss.Height(m.View()); m.height > 0 && h > m.height {
			t.Errorf("%dx%d: frame %d rows exceeds height %d", size[0], size[1], h, m.height)
		}
	}
}

// The divider sits directly above the input box so the thing you type into is
// visually separate from the thing you read, and it replaces a blank line
// rather than adding a row (see layout()'s chrome).
func TestInputRuleSeparatesInputFromTranscript(t *testing.T) {
	m := frameModel()
	m.input.SetValue("typing")
	m.layout()
	lines := strings.Split(ansi.Strip(m.View()), "\n")

	rule := -1
	for i, ln := range lines {
		if s := strings.TrimSpace(ln); s != "" && strings.Trim(s, "─") == "" {
			rule = i
		}
	}
	if rule < 0 {
		t.Fatalf("no divider row in the frame:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[rule+1], "typing") {
		t.Errorf("divider should sit directly above the input, got %q then %q",
			lines[rule], lines[rule+1])
	}
}

// An interactive bash command hides the input box; a divider with nothing
// under it would read as a stray line, so that case keeps the blank row.
func TestInputRuleHiddenWhileInteractive(t *testing.T) {
	m := frameModel()
	if m.inputRule() == "" {
		t.Fatal("expected a divider while the input box is shown")
	}
	m.iactive = &interactive{}
	if got := m.inputRule(); got != "" {
		t.Errorf("interactive bash hides the input box, so the divider should be blank, got %q", got)
	}
}

func TestTruncLineRuneSafe(t *testing.T) {
	s := "Hello 世界 ⏳"
	for w := 0; w <= 15; w++ {
		got := truncLine(s, w)
		if !utf8.ValidString(got) {
			t.Fatalf("truncLine(%q, %d) produced invalid UTF-8: %q", s, w, got)
		}
		if w > 0 && ansi.StringWidth(got) > w {
			t.Fatalf("truncLine(%q, %d) width %d exceeds max %d", s, w, ansi.StringWidth(got), w)
		}
	}
}

// Regression: a short transcript must sit directly above the input box, with
// no run of blank viewport rows between the last assistant reply and the
// prompt. The viewport is bottom-anchored (padding goes on top), and its fixed
// height keeps the prompt stationary while the transcript scrolls.
func TestNoGapBetweenLastReplyAndInput(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))
	m.append(" ❯ hi")
	m.appendAssistantBlock("Hi! What can I help you with today?")
	m.append(" ❯ how are you")
	m.appendAssistantBlock("Doing well, thanks for asking! Ready to dig into some code whenever you are. What are you working on?")
	m.layout()

	lines := strings.Split(ansi.Strip(m.View()), "\n")

	// locate the input box and the last assistant line
	inputRow, lastReplyRow := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "Ask ghg anything") {
			inputRow = i
		}
		if strings.Contains(l, "What are you working on?") {
			lastReplyRow = i
		}
	}
	if inputRow < 0 || lastReplyRow < 0 {
		t.Fatalf("could not find reply (%d) or input (%d) rows:\n%s", lastReplyRow, inputRow, strings.Join(lines, "\n"))
	}
	// allow at most one blank separator line between the reply and the prompt
	if gap := inputRow - lastReplyRow - 1; gap > 1 {
		t.Fatalf("found %d blank rows between last reply (row %d) and input (row %d):\n%s",
			gap, lastReplyRow, inputRow, strings.Join(lines, "\n"))
	}
}

// At the bottom of a short transcript, any padding belongs above the content;
// the viewport render must end on the final reply rather than a blank row.
func TestViewportViewHasNoTrailingBlankRows(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))
	m.append(" ❯ hi")
	m.appendAssistantBlock("Short reply.")
	m.layout()

	rendered := m.viewportView()
	lines := strings.Split(rendered, "\n")
	if last := lines[len(lines)-1]; strings.TrimSpace(ansi.Strip(last)) == "" {
		t.Fatalf("viewport render still has a trailing blank row: %q", rendered)
	}
}

func TestFmtTok(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 999: "999", 1000: "1.0k", 12345: "12.3k",
		1_000_000: "1.0M", 1_234_567: "1.2M",
	} {
		if got := fmtTok(in); got != want {
			t.Errorf("fmtTok(%d) = %q, want %q", in, got, want)
		}
	}
}

// Context size and route controls live in the bottom status bar; the header is
// reserved for the app identity and loaded-skill count.
func TestHeaderKeepsContextInStatus(t *testing.T) {
	m := compactCmdModel()
	m.agent = agent.New(testBackend("https://x", "k"), "kimi-k3-fast", 100, "sys")
	m.agent.ContextLimit = 100000
	m.follow = true
	m.agent.AddUsage(models.Usage{PromptTokens: 12345, CompletionTokens: 678})
	m.agent.AddUsage(models.Usage{ // cached tokens accumulate through the details struct
		PromptTokens: 1,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: 4000},
	})
	m.width = 200 // wide enough for the full status context block
	head := strings.SplitN(m.View(), "\n", 2)[0]
	for _, unwanted := range []string{"12.3k", "4.0k", "678", "ctx", "tok"} {
		if strings.Contains(head, unwanted) {
			t.Errorf("header should not contain context details %q: %q", unwanted, head)
		}
	}
	status := m.statusView()
	if !strings.Contains(status, "ctx ") || !strings.Contains(status, "/100.0k") {
		t.Errorf("status missing context size: %q", status)
	}
	for _, unwanted := range []string{"↓", "↑", "tok", "12.3k", "4.0k", "678"} {
		if strings.Contains(status, unwanted) {
			t.Errorf("status should not contain request token usage %q: %q", unwanted, status)
		}
	}
	if strings.Contains(head, "⚡") || strings.Contains(head, "kimi-k3-fast") || strings.Contains(head, shortCWD()) {
		t.Errorf("header should not repeat the route or working directory: %q", head)
	}
}

func TestHeaderContainsOnlyAppAndSkillCount(t *testing.T) {
	m := compactCmdModel()
	m.skillsLoaded = 33
	m.goal = "finish the release"
	m.follow = false
	m.width = 120

	head := strings.SplitN(ansi.Strip(m.View()), "\n", 2)[0]
	if got, want := strings.TrimSpace(head), "ghg · skills: 33 loaded"; got != want {
		t.Fatalf("header = %q, want exactly %q", got, want)
	}
}

func TestHeaderShowsLoadedSkillCount(t *testing.T) {
	m := compactCmdModel()
	m.skillsLoaded = 33
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if !strings.Contains(head, " ghg · skills: 33 loaded") {
		t.Fatalf("header should show the loaded skill count next to ghg: %q", head)
	}
}

// The header omits the context block entirely and leaves route details to the
// bottom status line.
func TestHeaderOmitsContext(t *testing.T) {
	m := compactCmdModel()
	m.width = 120
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if strings.Contains(head, "⣿") {
		t.Errorf("header should not contain a context block: %q", head)
	}
	if strings.Contains(head, "⚡") || strings.Contains(head, "kimi-k3-fast") {
		t.Errorf("header should omit effort and route controls: %q", head)
	}
}

func TestHeaderOmitsEffortControl(t *testing.T) {
	m := compactCmdModel()
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if strings.Contains(head, "⚡") || strings.Contains(head, "effort") {
		t.Fatalf("header should not render a reasoning control: %q", head)
	}
}

// The view contains only application content; terminal control sequences are
// owned by Run, which uses Bubble Tea's alternate-screen lifecycle. Mouse
// capture remains ON by default for wheel scroll and app-owned selection.
func TestViewRendersTranscript(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	m.appendAssistant("hello **world**")
	v := m.View()
	if strings.Contains(v, "\x1b[?1049h") || strings.Contains(v, "\x1b[?47h") {
		t.Fatal("view must not enter the alternate screen")
	}
	for _, want := range []string{"ghg", "hello", "world"} {
		if !strings.Contains(stripAll(v), want) {
			t.Errorf("inline view missing %q", want)
		}
	}
	// mouse capture on by default (wheel scroll and clicks)
	if !m.mouseOn {
		t.Fatal("mouse capture must default on for wheel scroll")
	}
}

func stripAll(s string) string {
	out := strings.Builder{}
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' && s[i] != 'h' && s[i] != 'l' {
				i++
			}
			i++
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func TestThinkingDisplayEphemeralAndCollapsedTranscript(t *testing.T) {
	m := compactCmdModel()
	m.showThinking = true
	m.Update(mkWinSize(80, 20))

	t0 := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	currTime := t0
	m.now = func() time.Time { return currTime }

	// Stream multiple lines of reasoning containing sensitive text
	um, _ := m.Update(thinkMsg("secret reasoning tokens\ninternal model deliberation"))
	m = um.(*model)

	// Advance clock by 3.2s
	currTime = t0.Add(3200 * time.Millisecond)

	// Live thinkView should only show the timer, never raw reasoning tokens
	tv := stripAll(m.thinkView())
	if tv != "◌ Thinking 3s" {
		t.Fatalf("live thinkView should be timer '◌ Thinking 3s', got %q", tv)
	}
	if strings.Contains(tv, "secret") || strings.Contains(tv, "deliberation") {
		t.Fatalf("live thinkView leaked reasoning tokens: %q", tv)
	}

	// Tool starts: finalized timer precedes tool-call row
	um, _ = m.Update(toolStartMsg{id: "c1", name: "grep", args: `{"pattern":"secretKey","path":"."}`})
	m = um.(*model)

	if len(m.blocks) < 2 {
		t.Fatalf("expected at least 2 blocks (timer + tool), got %d", len(m.blocks))
	}
	timerBlock := stripAll(m.blocks[len(m.blocks)-2].text)
	if timerBlock != "◌ Thinking 3s" {
		t.Fatalf("expected finalized timer block '◌ Thinking 3s', got %q", timerBlock)
	}
	toolBlock := stripAll(m.blocks[len(m.blocks)-1].text)
	if !strings.Contains(toolBlock, "⚒ grep") || !strings.Contains(toolBlock, `{"pattern":"secretKey","path":"."}`) {
		t.Fatalf("expected tool row with name and full args, got %q", toolBlock)
	}

	// Send tool result
	um, _ = m.Update(toolEndMsg{id: "c1", name: "grep", result: "secret_file.go: secretKey = 12345"})
	m = um.(*model)

	// Verify tool result output never appears in rendered TUI blocks
	for _, b := range m.blocks {
		bText := stripAll(b.render(m.width))
		if strings.Contains(bText, "secretKey = 12345") {
			t.Fatalf("tool result output leaked into transcript block: %q", bText)
		}
	}

	// Tool name and full args remain visible after completion
	completedRow := stripAll(m.blocks[len(m.blocks)-1].render(m.width))
	if !strings.Contains(completedRow, "⚒ grep") || !strings.Contains(completedRow, `{"pattern":"secretKey","path":"."}`) {
		t.Fatalf("tool name and arguments must remain visible after completion, got %q", completedRow)
	}

	// When thinking display is disabled, no timer line is appended
	m2 := compactCmdModel()
	m2.showThinking = false
	m2.Update(mkWinSize(80, 20))
	um2, _ := m2.Update(thinkMsg("invisible reasoning"))
	m2 = um2.(*model)
	um2, _ = m2.Update(textMsg("Answer with thinking off"))
	m2 = um2.(*model)
	for _, b := range m2.blocks {
		stripped := stripAll(b.text)
		if strings.Contains(stripped, "Thinking") || strings.Contains(stripped, "invisible reasoning") {
			t.Fatalf("disabled thinking should not add thought line to transcript: %q", stripped)
		}
	}
}

func TestFormatThinkingDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: -5 * time.Second, want: "0s"},
		{d: 0, want: "0s"},
		{d: 400 * time.Millisecond, want: "0s"},
		{d: 1200 * time.Millisecond, want: "1s"},
		{d: 3200 * time.Millisecond, want: "3s"},
		{d: 59 * time.Second, want: "59s"},
		{d: 60 * time.Second, want: "1m 0s"},
		{d: 65 * time.Second, want: "1m 5s"},
		{d: 119 * time.Second, want: "1m 59s"},
		{d: 3599 * time.Second, want: "59m 59s"},
		{d: 3600 * time.Second, want: "1h 0m 0s"},
		{d: 3665 * time.Second, want: "1h 1m 5s"},
		{d: 7325 * time.Second, want: "2h 2m 5s"},
	}
	for _, tc := range tests {
		if got := formatThinkingDuration(tc.d); got != tc.want {
			t.Errorf("formatThinkingDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// existsAll/existsNone inject file-existence without touching the disk.
func existsAll(string) bool  { return true }
func existsNone(string) bool { return false }

func TestLinkifyFilePaths(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		exists func(string) bool
		want   string // substring expected in output ("" = input unchanged)
	}{
		{"relative path", "see internal/tui/tui.go now", existsAll, "]8;;file://"},
		{"dot-relative", "see ./docs/features.md now", existsAll, "]8;;file://"},
		{"absolute", "see /etc/hostname now", existsAll, "file:///etc/hostname"},
		{"line ref kept", "see internal/tui/tui.go:42 now", existsAll, "]8;;file://"},
		{"line ref in uri", "internal/tui/tui.go:42", existsAll, "tui.go:42\x07"},
		{"missing file untouched", "see internal/tui/tui.go now", existsNone, ""},
		{"bare filename, exists", "see tui.go now", existsAll, "]8;;file://"},
		{"bare filename, missing", "see ghost.go now", existsNone, ""},
		{"no extension untouched", "see internal/tui now", existsNone, ""},
		{"markdown link target skipped", "[x](internal/tui/tui.go)", existsAll, ""},
		{"markdown link text skipped", "[internal/tui/tui.go](https://x)", existsAll, ""},
		{"url untouched", "see https://example.com/a/b.html now", existsAll, "https://example.com/a/b.html"},
		{"trailing dot excluded", "see internal/tui/tui.go. Done", existsAll, "file://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := linkifyFilePaths(tt.in, tt.exists)
			if tt.want == "" {
				if got != tt.in {
					t.Errorf("input should pass through unchanged:\n in: %q\ngot: %q", tt.in, got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("output missing %q:\n in: %q\ngot: %q", tt.want, tt.in, got)
			}
		})
	}
}

// A linkified path must stay cell-identical: OSC 8 is zero-width, so strip
// and width see only the original text.
func TestLinkifyFilePathsWidthNeutral(t *testing.T) {
	in := "open internal/tui/tui.go:42 please"
	got := linkifyFilePaths(in, existsAll)
	if ansi.Strip(got) != in {
		t.Errorf("stripped output must equal input:\n in: %q\ngot: %q", in, ansi.Strip(got))
	}
	if ansi.StringWidth(got) != ansi.StringWidth(in) {
		t.Errorf("width changed: %d vs %d", ansi.StringWidth(got), ansi.StringWidth(in))
	}
}

// The stripped text must still hold the full path: file:// carries the
// absolute form but the visible text is what the user typed.
func TestLinkifyFilePathsKeepsVisibleText(t *testing.T) {
	in := "fix internal/tui/tui.go:42"
	got := linkifyFilePaths(in, existsAll)
	if !strings.Contains(ansi.Strip(got), "internal/tui/tui.go:42") {
		t.Errorf("visible text lost: %q", ansi.Strip(got))
	}
}

func TestSplitLineRef(t *testing.T) {
	tests := []struct{ ref, path, line string }{
		{"a/b.go:42", "a/b.go", "42"},
		{"a/b.go", "a/b.go", ""},
		{"a/b.go:x", "a/b.go:x", ""},       // non-numeric suffix stays in path
		{"a/b.go:", "a/b.go", ""},          // trailing punctuation trimmed
		{"/a/b:1/c.go", "/a/b:1/c.go", ""}, // ':' not trailing-number
	}
	for _, tt := range tests {
		p, l := splitLineRef(tt.ref)
		if p != tt.path || l != tt.line {
			t.Errorf("splitLineRef(%q) = (%q, %q), want (%q, %q)", tt.ref, p, l, tt.path, tt.line)
		}
	}
}

// targetURI is the gate between a link destination and a clickable URI.
func TestTargetURI(t *testing.T) {
	tests := []struct {
		dest   string
		exists func(string) bool
		want   string // "" means "not clickable"
	}{
		{"https://example.com/x", existsNone, "https://example.com/x"},
		{"http://example.com", existsNone, "http://example.com"},
		{"mailto:a@b.c", existsNone, "mailto:a@b.c"},
		{"file:///etc/hostname", existsNone, "file:///etc/hostname"},
		{"#anchor", existsAll, ""},
		{"docs/features.md", existsAll, "file://"},
		{"./docs/features.md", existsAll, "file://"},
		{"docs/features.md", existsNone, ""},        // missing file: not clickable
		{"not a path at all", existsNone, ""},       // prose
		{"/docs/features.md", existsAll, "file://"}, // glamour-normalized ./ form
		{"internal/tui/tui.go:7", existsAll, "file://"},
	}
	for _, tt := range tests {
		got := targetURI(tt.dest, tt.exists)
		if tt.want == "" {
			if got != "" {
				t.Errorf("targetURI(%q) = %q, want unlinked", tt.dest, got)
			}
			continue
		}
		if !strings.Contains(got, tt.want) {
			t.Errorf("targetURI(%q) = %q, want substring %q", tt.dest, got, tt.want)
		}
	}
}

// --- glamour output rewiring (runs through the real renderer) --------------

// renderRaw renders markdown without the link passes so tests can compare
// against the pre-linkify shape.
func renderRaw(t *testing.T, s string, width int) string {
	t.Helper()
	out, err := mdRenderer(width).Render(s)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return stripLinePadding(strings.Trim(out, "\n"))
}

func TestHyperlinkGlamourLinksMarkdownLink(t *testing.T) {
	raw := renderRaw(t, "See [the docs](https://example.com/docs) end.", 80)
	linked := hyperlinkGlamourLinks(raw, existsNone)

	if !strings.Contains(linked, ansi.SetHyperlink("https://example.com/docs")) {
		t.Errorf("label should open an OSC 8 link to the href:\n%q", linked)
	}
	if !strings.Contains(linked, "the docs") {
		t.Errorf("label text lost: %q", ansi.Strip(linked))
	}
	// the href must not render a second time as visible text
	if strings.Contains(ansi.Strip(linked), "https://example.com/docs") {
		t.Errorf("href should not be duplicated visibly: %q", ansi.Strip(linked))
	}
	// visible text unchanged apart from dropping the appended href
	if !strings.Contains(ansi.Strip(raw), "the docs https://example.com/docs") {
		t.Fatalf("fixture assumption broken, raw stripped: %q", ansi.Strip(raw))
	}
}

func TestHyperlinkGlamourLinksAutolink(t *testing.T) {
	raw := renderRaw(t, "Go to https://bare.example.com/x now.", 80)
	linked := hyperlinkGlamourLinks(raw, existsNone)

	if !strings.Contains(linked, ansi.SetHyperlink("https://bare.example.com/x")) {
		t.Errorf("autolink should become clickable:\n%q", linked)
	}
	// visible text identical: the link was already shown once
	if ansi.Strip(linked) != ansi.Strip(raw) {
		t.Errorf("autolink strip mismatch:\nraw:    %q\nlinked: %q", ansi.Strip(raw), ansi.Strip(linked))
	}
}

func TestHyperlinkGlamourLinksFileDestination(t *testing.T) {
	raw := renderRaw(t, "Open [the feature map](./docs/features.md) here.", 80)
	linked := hyperlinkGlamourLinks(raw, existsAll)

	if !strings.Contains(linked, "]8;;file://") {
		t.Errorf("existing relative file destination should become file://:\n%q", linked)
	}
	if strings.Contains(ansi.Strip(linked), "/docs/features.md") {
		t.Errorf("file href should not render visibly: %q", ansi.Strip(linked))
	}

	// missing file: untouched by the rewiring (glamour normalized ./ → /)
	plain := hyperlinkGlamourLinks(raw, existsNone)
	if strings.Contains(plain, "]8;") {
		t.Errorf("missing file must not become a link: %q", plain)
	}
	if !strings.Contains(ansi.Strip(plain), "/docs/features.md") {
		t.Errorf("unlinked href should stay visible: %q", ansi.Strip(plain))
	}
}

func TestHyperlinkGlamourLinksAnchorUntouched(t *testing.T) {
	raw := renderRaw(t, "Jump [below](#section) now.", 80)
	linked := hyperlinkGlamourLinks(raw, existsAll)
	if strings.Contains(linked, "]8;") {
		t.Errorf("anchors are never hyperlinked: %q", linked)
	}
}

// The width contract: OSC 8 sequences are zero-width and Hardwrap-safe, so a
// linkified render wraps identically to the raw one.
func TestHyperlinkGlamourLinksWrapSafe(t *testing.T) {
	md := "See [the documentation page](https://example.com/some/long/path) for details."
	linked := wrapWideLines(hyperlinkGlamourLinks(renderRaw(t, md, 40), existsNone), 40)
	for i, l := range strings.Split(linked, "\n") {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d exceeds width 40 (%d): %q", i, w, l)
		}
	}
	// the click target survives wrapping
	if !strings.Contains(linked, ansi.SetHyperlink("https://example.com/some/long/path")) {
		t.Errorf("hyperlink broken by wrapping: %q", linked)
	}
}

// --- end-to-end through renderMarkdown --------------------------------------

func TestRenderMarkdownLinksClickable(t *testing.T) {
	out := renderMarkdown("See [the docs](https://example.com/docs) and https://bare.example.com/x.", 80)
	if !strings.Contains(out, ansi.SetHyperlink("https://example.com/docs")) {
		t.Errorf("markdown link should be clickable: %q", out)
	}
	if !strings.Contains(out, ansi.SetHyperlink("https://bare.example.com/x")) {
		t.Errorf("autolink should be clickable: %q", out)
	}
	plain := ansi.Strip(out)
	if strings.Count(plain, "https://example.com/docs") != 0 {
		t.Errorf("href rendered as duplicate text: %q", plain)
	}
	if strings.Count(plain, "https://bare.example.com/x") != 1 {
		t.Errorf("autolink should render exactly once: %q", plain)
	}
}

func TestRenderMarkdownBareFilePath(t *testing.T) {
	// real file, resolved against the process CWD (the package dir). Paths
	// are linkified post-render so the OSC 8 sequences survive wrapping.
	out := renderMarkdown("The bug is in render.go and render_test.go.", 80)
	if !strings.Contains(out, "]8;;file://") {
		t.Errorf("bare existing file path should be linkified: %q", out)
	}
	// glamour splits styled words across atoms; compare on the visible text
	plain := strings.Join(strings.Fields(ansi.Strip(out)), " ")
	if !strings.Contains(plain, "render_test.go") {
		t.Errorf("visible path text lost: %q", plain)
	}
	// nonexistent path stays plain
	out = renderMarkdown("Ghost at no/such/file.go remains text.", 80)
	if strings.Contains(out, "]8;") {
		t.Errorf("nonexistent file must not linkify: %q", out)
	}
	if !strings.Contains(ansi.Strip(out), "no/such/file.go") {
		t.Errorf("plain path text lost: %q", ansi.Strip(out))
	}
}

// --- user-facing wiring -----------------------------------------------------

// User messages render as "❯ text" blocks; file refs in them are clickable.
func TestUserMessageFileLink(t *testing.T) {
	m := compactCmdModel()
	m.width = 80
	m.append(youStyle.Render("❯ ") + linkifyFilePaths("look at render_test.go please", realFileExists))
	rendered := m.blocks[len(m.blocks)-1].render(80)
	if !strings.Contains(rendered, "]8;;file://") {
		t.Errorf("user file ref should be clickable: %q", rendered)
	}
	if !strings.Contains(ansi.Strip(rendered), "❯ look at render_test.go please") {
		t.Errorf("user text must render verbatim: %q", ansi.Strip(rendered))
	}
}

// realFileExists against the actual repo: the test binary runs in
// internal/tui, so its own source file exists and a ghost path doesn't.
func TestRealFileExists(t *testing.T) {
	if !realFileExists("render_test.go") {
		t.Error("own test file should exist relative to package CWD")
	}
	if realFileExists("no/such/ghost.go") {
		t.Error("ghost path should not exist")
	}
	if !realFileExists(filepath.Join(mustWd(t), "render_test.go")) {
		t.Error("absolute path to own test file should exist")
	}
	if realFileExists(".") {
		t.Error("a directory is not a linkable file")
	}
}

func mustWd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func mkWinSize(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

func TestRenderMarkdownBasics(t *testing.T) {
	out := renderMarkdown("# Title\n\nsome **bold** text\n\n- a\n- b\n\n```go\nfmt.Println()\n```", 80)
	plain := ansi.Strip(out)
	for _, want := range []string{"Title", "bold", "• a", "• b", "fmt.Println()"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered output missing %q:\n%s", want, plain)
		}
	}
	// bold is styled, not literal asterisks
	if strings.Contains(plain, "**") {
		t.Errorf("markdown markers should be consumed:\n%s", plain)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Error("expected ANSI styling in rendered output")
	}
}

func TestRenderMarkdownStripsRightPadding(t *testing.T) {
	out := renderMarkdown("short line", 80)
	for i, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > 12 {
			t.Errorf("line %d padded to width %d (should be unpadded): %q", i, w, l)
		}
	}
}

func TestRenderMarkdownFallback(t *testing.T) {
	if got := renderMarkdown("", 80); got != "" {
		t.Errorf("empty input should pass through, got %q", got)
	}
	// width<=0 is clamped to the minimum render width, never passed through
	// unwrapped (that was the overflow bug)
	out := renderMarkdown("plain text", 0)
	plain := strings.Join(strings.Fields(ansi.Strip(out)), " ")
	if plain != "plain text" {
		t.Errorf("content must survive the clamp, got %q", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if ansi.StringWidth(l) > 8 {
			t.Errorf("clamped render must respect width 8: %q", l)
		}
	}
}

func TestRenderMarkdownWrapsToWidth(t *testing.T) {
	long := strings.Repeat("word ", 40)
	out := renderMarkdown(long, 40)
	for i, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d exceeds width 40 (%d): %q", i, w, l)
		}
	}
}

func TestIndentLines(t *testing.T) {
	// relative shift: glamour's 2-cell document margin becomes n; deeper
	// lines keep their relative indent (nested bullets, code blocks)
	in := "  first\n\n    second" // margin + 2 extra
	want := "  first\n\n    second"
	if got := indentLines(in, 2); got != want {
		t.Errorf("indentLines:\ngot  %q\nwant %q", got, want)
	}
	// whitespace-only lines become truly empty (no stray styled cells)
	if got := indentLines("  \n  x", 2); got != "\n  x" {
		t.Errorf("blank line should be empty: %q", got)
	}
}

// Assistant segments land in the transcript as raw markdown; rendering (with
// the ● marker and body indent) happens per-width in block.render.
func TestAppendAssistantRendersMarkdown(t *testing.T) {
	m := compactCmdModel()
	m.width = 80
	m.appendAssistant("results:\n\n- **one**\n- two")
	if len(m.blocks) == 0 {
		t.Fatal("no transcript block")
	}
	if m.blocks[0].kind != blockAssistant {
		t.Fatalf("assistant text should be stored raw (blockAssistant), got %v", m.blocks[0].kind)
	}
	rendered := ansi.Strip(m.blocks[0].render(80))
	if !strings.HasPrefix(rendered, "● ") {
		t.Errorf("first line should carry the marker: %q", rendered)
	}
	if !strings.Contains(rendered, "• one") || !strings.Contains(rendered, "• two") {
		t.Errorf("list should be rendered: %q", rendered)
	}
	if strings.Contains(rendered, "**") {
		t.Errorf("markdown markers should be consumed: %q", rendered)
	}
	// continuation segment: merges into the same block (one marker, one doc)
	m.appendAssistant("more text")
	if len(m.blocks) != 1 {
		t.Fatalf("continuation should merge into the open block, got %d blocks", len(m.blocks))
	}
	full := ansi.Strip(m.blocks[0].render(80))
	if strings.Count(full, "● ") != 1 {
		t.Errorf("continuation segment must not add a second marker:\n%s", full)
	}
	if !strings.Contains(full, "more text") {
		t.Errorf("merged content missing: %q", full)
	}
}

// A width change re-renders the whole transcript: assistant markdown reflows
// and status/tool lines re-wrap.
func TestResizeRewrapsTranscript(t *testing.T) {
	m := compactCmdModel()
	m.width = 80
	m.appendAssistant("a paragraph of assistant text that should reflow when the terminal gets narrower")
	m.append(dimStyle.Render("status line with enough words to need rewrapping at a narrow width ok"))
	// narrow the terminal via a WindowSizeMsg (the real resize path)
	tm, _ := m.Update(mkWinSize(40, 24))
	m = tm.(*model)
	for _, b := range m.blocks {
		for i, l := range strings.Split(ansi.Strip(b.render(m.width)), "\n") {
			if w := ansi.StringWidth(l); w > 40 {
				t.Errorf("after resize to 40: block line %d is %d wide: %q", i, w, l)
			}
		}
	}
	// and back wide again
	tm, _ = m.Update(mkWinSize(120, 24))
	m = tm.(*model)
	for _, b := range m.blocks {
		for i, l := range strings.Split(ansi.Strip(b.render(m.width)), "\n") {
			if w := ansi.StringWidth(l); w > 120 {
				t.Errorf("after resize to 120: block line %d is %d wide", i, w)
			}
		}
	}
}

// Table rendering: pipes separate columns, a header rule with box-drawing
// joints, cell content wraps within width, and alignment markers hold. Pins
// the explicit Table style (stock Dark/Light leave separators to lipgloss
// defaults — a dependency bump must not silently unformat tables).
func TestRenderMarkdownTable(t *testing.T) {
	md := "| Name | Age | City |\n|:---|---:|---|\n| Alice | 30 | New York |\n| Bob | 25 | London |"
	out := renderMarkdown(md, 50)
	plain := ansi.Strip(out)
	for _, want := range []string{"│", "─", "Alice", "New York"} {
		if !strings.Contains(plain, want) {
			t.Errorf("table render missing %q:\n%s", want, plain)
		}
	}
	// every rendered line respects width
	for i, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > 50 {
			t.Errorf("line %d exceeds width 50 (%d): %q", i, w, l)
		}
	}
	// markdown pipes consumed, not literal
	if strings.Contains(plain, "|---|") {
		t.Errorf("table markers should be consumed:\n%s", plain)
	}
}

// A wide table at narrow width wraps cell content instead of overflowing or
// mangling columns.
func TestRenderMarkdownTableNarrow(t *testing.T) {
	md := "| Package | Purpose |\n|---|---|\n| internal/agent | the agent loop with a long description that must wrap around |"
	out := renderMarkdown(md, 40)
	for i, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d exceeds width 40 (%d): %q", i, w, l)
		}
	}
	plain := ansi.Strip(out)
	if !strings.Contains(plain, "internal/agent") || !strings.Contains(plain, "wrap") {
		t.Errorf("wrapped table lost content:\n%s", plain)
	}
}

// Rendered markdown must never exceed the render width, even with long
// unbreakable code lines, at any terminal width.
func TestRenderedLinesNeverExceedWidth(t *testing.T) {
	src := "some **text** here and `code`\n\n- item one\n- item two\n\n```\nx := " + strings.Repeat("y", 60) + "\n```\n\nplain " + strings.Repeat("word ", 30)
	for _, w := range []int{8, 20, 40, 58, 80} {
		out := renderMarkdown(src, w)
		for i, l := range strings.Split(out, "\n") {
			if got := ansi.StringWidth(l); got > w {
				t.Errorf("width %d: line %d is %d wide: %q", w, i, got, ansi.Strip(l))
			}
		}
	}
}

// Regression: at a narrow terminal width, a long tool-call command must wrap
// and stay fully visible — never truncated with "…".
func TestToolCallLineWrapsNotTruncates(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(40, 24))
	longCmd := `{"command":"cd /some/deeply/nested/directory && go test ./internal/... -run TestSomething -count=1 -v"}`
	tm, _ := m.Update(toolStartMsg{name: "bash", args: longCmd})
	m = tm.(*model)

	blk := m.blocks[len(m.blocks)-1]
	rendered := ansi.Strip(blk.render(m.width))
	for _, l := range strings.Split(rendered, "\n") {
		if strings.Contains(l, "…") {
			t.Fatalf("tool call line truncated: %q", rendered)
		}
		if w := ansi.StringWidth(l); w > 40 {
			t.Fatalf("line exceeds width 40 (%d): %q", w, l)
		}
	}
	// the full command survives: every chunk of the original is present
	joined := strings.Join(strings.Fields(rendered), " ")
	for _, frag := range []string{"go test", "count=1", "TestSomething", "nested/directory"} {
		if !strings.Contains(joined, frag) {
			t.Errorf("rendered command missing %q:\n%s", frag, joined)
		}
	}
}

// Regression: resumed sessions render full tool-call args too (no 120-char cut).
func TestResumedToolCallNotTruncated(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(50, 24))
	args := `{"command":"` + strings.Repeat("echo hello; ", 20) + `"}`
	msg := models.Message{Role: "assistant"}
	var tc models.ToolCall
	tc.Function.Name = "bash"
	tc.Function.Arguments = args
	msg.ToolCalls = []models.ToolCall{tc}
	m.seedTranscript([]models.Message{msg}, 1)

	var found bool
	for _, b := range m.blocks {
		rendered := ansi.Strip(b.render(m.width))
		if !strings.Contains(rendered, "⚒ bash") {
			continue
		}
		found = true
		if strings.Contains(rendered, "…") {
			t.Fatalf("resumed tool call truncated: %q", rendered)
		}
		for _, l := range strings.Split(rendered, "\n") {
			if w := ansi.StringWidth(l); w > 50 {
				t.Fatalf("line exceeds width 50 (%d): %q", w, l)
			}
		}
		joined := strings.Join(strings.Fields(rendered), " ")
		if c := strings.Count(joined, "echo hello;"); c != 20 {
			t.Fatalf("expected all 20 'echo hello;' chunks, got %d:\n%s", c, rendered)
		}
	}
	if !found {
		t.Fatal("no bash tool block rendered from the resumed transcript")
	}
}

// statusModel builds a model with an agent so statusView has data.
func statusModel() *model {
	m := newGrowModel()
	m.agent = &agent.Agent{}
	return m
}

// The status box always renders below the input with directory, model,
// mode, provider, and context size — regardless of scroll or state.
func TestStatusLineAlwaysShown(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "kimi-k3-fast"
	m.provName = "inference"
	m.agent.Effort = "high"
	m.agent.ContextLimit = 128000
	m.agent.Messages = []models.Message{{Role: "system", Content: "system prompt"}}

	v := m.View()
	for _, want := range []string{"kimi-k3-fast", "(high)", "execute", "inference", "ctx 8/128.0k"} {
		if !strings.Contains(v, want) {
			t.Errorf("status box should show %q\n--- view tail ---\n%s", want, tailLines(v, 8))
		}
	}
	// the directory is present (compacted to its last segments) — assert
	// against the actual cwd via shortCWD, not a hardcoded checkout name:
	// tests run from internal/tui regardless of the repo folder's name.
	if dir := shortCWD(); !strings.Contains(v, dir) {
		t.Errorf("status box should show the working directory %q\n%s", dir, tailLines(v, 8))
	}
}

func TestContextStatusUsesLatestReportedRequest(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	m.agent.Messages = []models.Message{
		{Role: "system", Content: strings.Repeat("x", 40000)},
		{Role: "assistant", Content: "first", Usage: &models.Usage{PromptTokens: 100, CompletionTokens: 20}},
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "latest", Usage: &models.Usage{PromptTokens: 300, CompletionTokens: 40}},
	}

	if got, want := m.contextStatus(), "ctx 340/128.0k"; got != want {
		t.Fatalf("context status = %q, want %q", got, want)
	}
}

func TestContextStatusEstimatesActiveTokensWithoutReportedResponse(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	m.agent.Messages = []models.Message{{Role: "system", Content: strings.Repeat("x", 40000)}}

	if got, want := m.contextStatus(), "ctx 10.0k/128.0k"; got != want {
		t.Fatalf("context status = %q, want %q", got, want)
	}
}

// With no conversation yet the context reads zero, and effort remains a
// separate control.
func TestStatusLineDefaults(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "m"
	m.provName = "p"

	v := m.View()
	if !strings.Contains(v, "ctx 0") {
		t.Errorf("empty session should read ctx 0\n%s", tailLines(v, 6))
	}
	if !strings.Contains(v, "│ m │ (off) │ execute │") {
		t.Errorf("effort off should remain a separate indicator\n%s", tailLines(v, 6))
	}
	if !strings.Contains(v, "│ m │ (off) │ execute │ p │") {
		t.Errorf("model, mode, and provider should appear\n%s", tailLines(v, 6))
	}
}

func TestStatusBoxHasBordersAndDelimiters(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "model"
	m.provName = "provider"

	lines := strings.Split(ansi.Strip(m.statusView()), "\n")
	if len(lines) != statusBoxRows {
		t.Fatalf("status box should have %d rows, got %d: %q", statusBoxRows, len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "┌") || !strings.HasSuffix(lines[0], "┐") {
		t.Errorf("top row should be bordered: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "│") || !strings.HasSuffix(lines[1], "│") {
		t.Errorf("content row should be bordered: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "└") || !strings.HasSuffix(lines[2], "┘") {
		t.Errorf("bottom row should be bordered: %q", lines[2])
	}
	if got := strings.Count(lines[1], "│"); got != 7 {
		t.Errorf("content row should delimit all six cells, got %d vertical bars: %q", got, lines[1])
	}
	for i, line := range lines {
		if got := ansi.StringWidth(line); got > m.width {
			t.Errorf("status row %d is wider than terminal: %d > %d", i, got, m.width)
		}
	}
}

func TestBottomStatusHitboxMatchesRenderedRow(t *testing.T) {
	m := statusModel()
	tm, _ := m.Update(mkWinSize(100, 30))
	m = tm.(*model)

	lines := strings.Split(ansi.Strip(m.View()), "\n")
	renderedRow := -1
	for i, line := range lines {
		if strings.Contains(line, "execute") && strings.Contains(line, "ctx") {
			renderedRow = i
			break
		}
	}
	if renderedRow < 0 {
		t.Fatalf("could not find the rendered status row:\n%s", m.View())
	}
	if renderedRow != statusInfoRow(m.height) {
		t.Fatalf("status hitbox row=%d, rendered row=%d; the bottom controls would not receive clicks", statusInfoRow(m.height), renderedRow)
	}
}

func TestStatusBoxFitsNarrowWidths(t *testing.T) {
	for _, width := range []int{1, 2, 6, 10, 40} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := statusModel()
			m.width = width
			lines := strings.Split(ansi.Strip(m.statusView()), "\n")
			if len(lines) != statusBoxRows {
				t.Fatalf("status box should have %d rows, got %d: %q", statusBoxRows, len(lines), lines)
			}
			for i, line := range lines {
				if got := ansi.StringWidth(line); got > width {
					t.Errorf("status row %d is wider than terminal: %d > %d (%q)", i, got, width, line)
				}
			}
		})
	}
}

func TestStatusModelSlotStaysFixedAcrossRoleChanges(t *testing.T) {
	m := compactCmdModel()
	m.width = 120
	short := "short-model"
	long := "a-deliberately-long-selected-model"
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleDefault: {Model: short, Provider: "inference"},
		config.RoleSmart:   {Model: long, Provider: "inference"},
		config.RoleFast:    {Model: short, Provider: "inference"},
		config.RoleTiny:    {Model: short, Provider: "inference"},
	}
	m.cfg.Models[short] = config.Model{Providers: []string{"inference"}}
	m.cfg.Models[long] = config.Model{Providers: []string{"inference"}}

	m.modelName = short
	m.agent.Role = config.RoleDefault
	_ = m.View()
	effortX, modeX, modelW := m.statusEffortX, m.statusModeX, m.statusModelW
	if modelW != len(long) {
		t.Fatalf("model slot should reserve the longest selected role model: got %d, want %d", modelW, len(long))
	}

	m.modelName = long
	m.agent.Role = config.RoleSmart
	_ = m.View()
	if m.statusEffortX != effortX || m.statusModeX != modeX || m.statusModelW != modelW {
		t.Fatalf("following status controls moved with the model: short=(%d,%d,%d), long=(%d,%d,%d)", effortX, modeX, modelW, m.statusEffortX, m.statusModeX, m.statusModelW)
	}

	m.width = 40
	_ = m.View()
	if m.statusEffortW == 0 || m.statusModeW == 0 {
		t.Fatalf("narrow status should retain effort and mode controls: effort=%d mode=%d", m.statusEffortW, m.statusModeW)
	}
	if m.statusModelW >= len(long) {
		t.Fatalf("narrow status should truncate the model slot, got width %d", m.statusModelW)
	}
}

// Request usage is no longer rendered in the status segment; context size is
// the useful persistent value there instead.
func TestStatusLineOmitsTokenUsage(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	u := models.Usage{PromptTokens: 10000, CompletionTokens: 500}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 4000}
	m.agent.AddUsage(u)

	got := m.statusView()
	if strings.ContainsAny(got, "↓↑") || strings.Contains(got, "tok") {
		t.Errorf("status should no longer show directional token usage: %q", got)
	}
	if !strings.Contains(got, "ctx 0") {
		t.Errorf("status should show context size instead: %q", got)
	}
}

func TestBusyStatsLeavesTokenUsageToStatus(t *testing.T) {
	m := statusModel()
	m.turnStart = time.Unix(100, 0)
	m.now = func() time.Time { return time.Unix(101, 0) }
	m.agent.AddUsage(models.Usage{PromptTokens: 12000, CompletionTokens: 800})

	got := m.busyStats()
	if strings.Contains(got, "tok") || strings.Contains(got, "%") || strings.Contains(got, "12.0k") {
		t.Fatalf("busy line should not render token usage: %q", got)
	}
	if !strings.Contains(got, "0:01") {
		t.Fatalf("busy line should retain elapsed time: %q", got)
	}
}

// The status box is the last content rows before the bottom padding, sitting
// below the input even when the esc/quit warnings or completion menu show.
func TestStatusLineBelowInputAndWarnings(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.escClr = true // draft-clear warning armed

	v := m.View()
	lines := strings.Split(strings.TrimRight(v, "\n"), "\n")
	var inputRow, statusRow int
	for i, l := range lines {
		if strings.Contains(l, "Ask ghg anything") {
			inputRow = i
		}
		if strings.Contains(l, "ctx 0") {
			statusRow = i
		}
	}
	if statusRow <= inputRow {
		t.Fatalf("status box should sit below the input (input=%d status=%d)\n%s", inputRow, statusRow, v)
	}
}

// Exactly one blank line separates the status box from whatever is above it,
// and the three-row box is the final content (no blank line below).
func TestStatusLineSpacing(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "m"
	m.provName = "p"

	lines := strings.Split(m.View(), "\n")
	var statusRow = -1
	for i, l := range lines {
		if strings.Contains(l, "ctx 0") {
			statusRow = i
		}
	}
	if statusRow < 1 {
		t.Fatalf("status box not found\n%s", m.View())
	}
	if lines[statusRow-2] != "" {
		t.Errorf("want one blank line above the status box, got %q", lines[statusRow-2])
	}
	for i, want := range []string{"┌", "│", "└"} {
		if !strings.HasPrefix(ansi.Strip(lines[statusRow-1+i]), want) {
			t.Errorf("status box row %d should start with %q, got %q", i, want, lines[statusRow-1+i])
		}
	}
	// the bottom border is the last row, with nothing below it
	if statusRow+1 != len(lines)-1 {
		t.Errorf("status box should be the last rows (bottom row %d of %d lines)", statusRow+1, len(lines)-1)
	}
}

// tailLines returns the last n lines of s, for failure output.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Cost appears in the context segment when the provider's catalog advertises
// pricing for the current model, and is hidden otherwise.
func TestStatusLineShowsCost(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.agent.Model = "priced"
	m.catalogs = map[string]config.Catalog{
		"p": {Models: []config.ModelInfoLite{{ID: "priced", InPrice: 1e-6, OutPrice: 5e-6, CacheReadPrice: 1e-7}}},
	}
	u := models.Usage{PromptTokens: 10000, CompletionTokens: 1000}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 8000}
	m.agent.AddUsage(u)

	// (10k-8k)*1e-6 + 8k*1e-7 + 1k*5e-6 = 0.0078
	if got := m.statusView(); !strings.Contains(got, "$0.0078") {
		t.Errorf("cost should show in the spend: %q", got)
	}
}

func TestStatusLineHidesCostWithoutPricing(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.agent.Model = "unpriced"
	m.catalogs = map[string]config.Catalog{
		"p": {Models: []config.ModelInfoLite{{ID: "unpriced"}}},
	}
	m.agent.AddUsage(models.Usage{PromptTokens: 10000, CompletionTokens: 1000})

	if got := m.statusView(); strings.Contains(got, "$") {
		t.Errorf("unpriced model should hide cost: %q", got)
	}

	// no catalog for the provider at all (startup fetch still in flight)
	m.catalogs = nil
	if got := m.statusView(); strings.Contains(got, "$") {
		t.Errorf("missing catalog should hide cost: %q", got)
	}
}

func TestFmtCost(t *testing.T) {
	if got := fmtCost(0.0134); got != "$0.0134" {
		t.Errorf("sub-dollar: %q", got)
	}
	if got := fmtCost(12.345); got != "$12.35" {
		t.Errorf("over a dollar: %q", got)
	}
}

// The /models fetch → catalog → cost pipeline keeps per-variant pricing
// distinct (kimi-k3-fast bills higher than kimi-k3) with nothing hardcoded:
// rates come from the provider's response body alone.
func TestSessionCostUsesFetchedPricing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
			{"id":"kimi-k3","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003"}},
			{"id":"kimi-k3-fast","pricing":{"prompt":"0.0000045","completion":"0.0000225","input_cache_read":"0.00000045"}}
		]}`))
	}))
	defer srv.Close()

	backend, err := models.NewBackend(models.Resolved{
		BaseURL:  srv.URL,
		Protocol: models.ProtocolOpenAIChatCompletions,
	}, models.BackendOptions{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := backend.(models.CatalogBackend).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lites := make([]config.ModelInfoLite, len(infos))
	for i, mi := range infos {
		lites[i] = config.ModelInfoLite{ID: mi.ID}
		if mi.Pricing != nil {
			lites[i].InPrice, lites[i].OutPrice, lites[i].CacheReadPrice = mi.Pricing.Rates()
		}
	}
	m := statusModel()
	m.provName = "inference"
	m.catalogs = map[string]config.Catalog{"inference": {Models: lites}}
	u := models.Usage{PromptTokens: 31100, CompletionTokens: 360}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 20700}
	m.agent.AddUsage(u)

	m.agent.Model = "kimi-k3-fast"
	fast, ok := m.sessionCost()
	if !ok {
		t.Fatal("fast variant should be priced")
	}
	m.agent.Model = "kimi-k3"
	std, ok := m.sessionCost()
	if !ok {
		t.Fatal("standard variant should be priced")
	}
	if fast <= std {
		t.Errorf("kimi-k3-fast cost %v should exceed kimi-k3 %v", fast, std)
	}
	// exact: (31100-20700)*4.5e-6 + 20700*4.5e-7 + 360*22.5e-6
	if want := 0.064215; fast != want {
		t.Errorf("kimi-k3-fast cost = %v, want %v", fast, want)
	}
}

func TestWorkerSnapshotContextTokensOverridesStaleShadowAgent(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	// Shadow agent has ~10k tokens worth of text
	m.agent.Messages = []models.Message{{Role: "system", Content: strings.Repeat("x", 40000)}}

	// Direct execution would say 10.0k
	if got, want := m.contextStatus(), "ctx 10.0k/128.0k"; got != want {
		t.Fatalf("pre-worker context status = %q, want %q", got, want)
	}

	// Apply worker snapshot with authoritative 80617 tokens
	m.applyWorkerSnapshot(workerwire.Snapshot{
		State:         "running",
		ContextTokens: 80617,
		ContextLimit:  128000,
	})

	// Should reflect authoritative worker count (~80.6k)
	if got, want := m.contextStatus(), "ctx 80.6k/128.0k"; got != want {
		t.Fatalf("worker snapshot context status = %q, want %q", got, want)
	}
}

func TestWorkerUsageAndCompactionSynchronizeContextTokens(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	m.workerState = "running"
	m.workerContextTokens = 1000

	if got, want := m.contextStatus(), "ctx 1.0k/128.0k"; got != want {
		t.Fatalf("initial context status = %q, want %q", got, want)
	}

	// Worker sends usage event for 50k prompt + 2k completion
	usageJSON, err := json.Marshal(models.Usage{PromptTokens: 50000, CompletionTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	m.workerEvent(workerEvent{Kind: "usage", Data: usageJSON})

	if got, want := m.contextStatus(), "ctx 52.0k/128.0k"; got != want {
		t.Fatalf("post-usage context status = %q, want %q", got, want)
	}

	// Worker completes compaction, returning compacted messages (~1k tokens)
	compactJSON, err := json.Marshal(workerwire.CompactResult{
		Messages: []models.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "summary"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.workerEvent(workerEvent{Kind: "compact_done", Data: compactJSON})

	// Context tokens recomputed from returned compacted messages
	if m.workerContextTokens >= 52000 || m.workerContextTokens == 0 {
		t.Fatalf("expected compacted tokens < 52k, got %d", m.workerContextTokens)
	}
	if got := m.contextStatus(); strings.Contains(got, "52.0k") {
		t.Fatalf("expected context status to decrease after compaction, got %q", got)
	}
}

// Tool results collapse to a preview; ctrl+e toggles the latest one and
// clicking the block expands it.
func TestToolExpand(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	result := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8"
	m.appendRaw(blockTool, result)

	// collapsed: 5 lines + hint
	out := ansi.Strip(m.blocks[0].render(m.width))
	if strings.Contains(out, "line8") || !strings.Contains(out, "… +3 lines") {
		t.Fatalf("collapsed render wrong: %q", out)
	}

	// ctrl+e expands the latest tool block
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = tm.(*model)
	out = ansi.Strip(m.blocks[0].render(m.width))
	if !strings.Contains(out, "line8") || strings.Contains(out, "…") {
		t.Fatalf("expanded render wrong: %q", out)
	}

	// and collapses back
	tm, _ = m.key(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = tm.(*model)
	if m.blocks[0].expanded {
		t.Fatal("second ctrl+e should collapse")
	}

	// click on the block row expands it
	m.refreshVP()
	y0 := m.blocks[0].y0 + m.contentPad()
	screenY := y0 - m.vp.YOffset + 2
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 5, Y: screenY})
	m = tm.(*model)
	tm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 5, Y: screenY})
	m = tm.(*model)
	if !m.blocks[0].expanded {
		t.Fatalf("click at screen Y=%d should expand the tool block", screenY)
	}
}

func TestToolCollapsedDiffCapped(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	var sb strings.Builder
	sb.WriteString("Replaced 50 occurrences in foo.go\n```diff\n--- foo.go\n+++ foo.go\n")
	for i := 0; i < 50; i++ {
		sb.WriteString(strings.Repeat("- old line\n+ new line\n", 1))
	}
	sb.WriteString("```")
	m.appendRaw(blockTool, sb.String())

	out := ansi.Strip(m.blocks[0].render(m.width))
	if strings.Contains(out, "--- foo.go") || strings.Contains(out, "+++ foo.go") {
		t.Fatalf("collapsed preview should skip diff headers, got:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 20 {
		t.Fatalf("collapsed preview should remain capped, got %d lines", len(lines))
	}
}

// A tool row renders the tool name and full arguments. On completion, it retains
// the name and arguments without leaking stdout/result into the viewport.
func TestToolRowDetailsOnly(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))

	m.Update(toolStartMsg{id: "c1", name: "read", args: `{"path":"internal/session/session.go","offset":700,"limit":100}`})
	if len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].kind != blockToolRun {
		t.Fatal("toolStart should append a running row")
	}
	row := m.blocks[len(m.blocks)-1]
	if !row.toolRunning {
		t.Fatal("row should be running")
	}
	got := ansi.Strip(row.render(m.width))
	if !strings.Contains(got, "⚒ read") || !strings.Contains(got, `{"path":"internal/session/session.go","offset":700,"limit":100}`) {
		t.Fatalf("running row should show tool name and full args, got %q", got)
	}

	m.Update(toolEndMsg{id: "c1", name: "read", result: "file body sensitive content\nline2\nline3"})
	row = m.blocks[len(m.blocks)-1]
	if row.toolRunning {
		t.Fatal("completion should stop the run state")
	}
	got = ansi.Strip(row.render(m.width))
	if !strings.Contains(got, "⚒ read") || !strings.Contains(got, `{"path":"internal/session/session.go","offset":700,"limit":100}`) {
		t.Fatalf("completed row should retain tool name and arguments, got %q", got)
	}
	if strings.Contains(got, "file body") || strings.Contains(got, "sensitive") {
		t.Fatalf("completed row must not leak tool output into viewport, got %q", got)
	}
}

// A failed tool row appends "— failed" without showing error bodies or stderr.
func TestToolRowFailureHidesErrorBody(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))
	m.Update(toolStartMsg{id: "c1", name: "bash", args: `{"command":"go test ./..."}`})
	m.Update(toolEndMsg{id: "c1", name: "bash", result: "Error: exit status 1\nsensitive stderr"})

	var run *block
	for i := range m.blocks {
		if m.blocks[i].kind == blockToolRun {
			run = &m.blocks[i]
		}
	}
	if run == nil || !run.toolFailed {
		t.Fatal("a failed tool should mark the row failed")
	}
	got := ansi.Strip(run.render(m.width))
	if !strings.Contains(got, "— failed") {
		t.Fatalf("failed row should contain '— failed', got %q", got)
	}
	if !strings.Contains(got, `{"command":"go test ./..."}`) {
		t.Fatalf("failed row should retain command arguments, got %q", got)
	}
	if strings.Contains(got, "exit status 1") || strings.Contains(got, "sensitive stderr") {
		t.Fatalf("failed row must not display error body or stderr, got %q", got)
	}

	m.Update(toolStartMsg{id: "c2", name: "bash", args: `{"command":"blocked"}`})
	m.Update(toolEndMsg{id: "c2", name: "bash", result: `Error: unknown tool "bash"`})
	var harnessFailure *block
	for i := range m.blocks {
		if m.blocks[i].kind == blockToolRun && m.blocks[i].toolID == "c2" {
			harnessFailure = &m.blocks[i]
			break
		}
	}
	if harnessFailure == nil {
		t.Fatal("expected harness-level bash failure row")
	}
	got = ansi.Strip(harnessFailure.render(m.width))
	if !strings.Contains(got, `unknown tool "bash"`) {
		t.Fatalf("harness-level bash failure should show its reason, got %q", got)
	}
}

func TestToolRowFailureShowsToolErrorReason(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 24))
	m.Update(toolStartMsg{id: "c2", name: "grep", args: `{"path":"src"}`})
	m.Update(toolEndMsg{id: "c2", name: "grep", result: "Error: pattern or patterns is required"})

	var run *block
	for i := range m.blocks {
		if m.blocks[i].kind == blockToolRun && m.blocks[i].toolID == "c2" {
			run = &m.blocks[i]
		}
	}
	if run == nil || !run.toolFailed {
		t.Fatal("a failed tool should mark the row failed")
	}
	got := ansi.Strip(run.render(m.width))
	if !strings.Contains(got, "— failed: pattern or patterns is required") {
		t.Fatalf("failed row should contain error reason, got %q", got)
	}
}
