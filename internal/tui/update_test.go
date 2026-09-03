package tui

import (
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Same ctrl+j regression but with a populated transcript (the resume case):
// the viewport has content, so layout computes a non-trivial height and the
// input sits below it.
func TestCtrlJFirstLineVisibleWithTranscript(t *testing.T) {
	m := compactCmdModel()
	m.queueSel = -1
	m.agent.Messages = append(m.agent.Messages,
		models.Message{Role: "user", Content: "earlier question", Authored: true},
		models.Message{Role: "assistant", Content: "earlier answer with several lines\nof content\nright here"},
	)
	m.height = 24
	m.layout()
	t.Logf("layout: vp.Height=%d inputHeight=%d", m.vp.Height, m.input.Height())
	_ = lipgloss.Height // keep import

	p := tea.NewProgram(m, tea.WithOutput(nopWriter{}), tea.WithInput(strings.NewReader("")), tea.WithoutSignalHandler())
	done := make(chan struct{})
	go func() { p.Run(); close(done) }()
	defer func() { p.Kill(); <-done }()
	time.Sleep(100 * time.Millisecond)

	for _, r := range "hello first line" {
		p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		time.Sleep(30 * time.Millisecond)
	}
	p.Send(tea.KeyMsg{Type: tea.KeyCtrlJ})
	time.Sleep(100 * time.Millisecond)
	p.Quit()
	<-done
	v := m.input.View()
	if !strings.Contains(v, "hello first line") {
		t.Fatalf("REPRODUCED with transcript: %q", strings.Split(v, "\n"))
	}
	t.Logf("view ok: %q", strings.Split(v, "\n"))
}

// Regression: typing a first line then hitting ctrl+j must keep the first
// line visible. Under the live program the textarea's internal viewport kept
// the scroll offset it computed at height 1 (YOffset=1), and the deferred
// growInput rebuild inherited it — the first line scrolled out of view. The
// handler now resets the scroll (SetValue) after inserting the newline.
//
// The bug only reproduces under a REAL tea.Program (its cursor-blink command
// delivery is what leaves the stale scroll offset; synchronous Update replays
// never blanked the view), so this drives one.
func TestCtrlJFirstLineStaysVisible(t *testing.T) {
	m := compactCmdModel()
	m.queueSel = -1
	p := tea.NewProgram(m, tea.WithOutput(nopWriter{}), tea.WithInput(strings.NewReader("")), tea.WithoutSignalHandler())
	done := make(chan struct{})
	go func() { p.Run(); close(done) }()
	defer func() { p.Kill(); <-done }()
	time.Sleep(100 * time.Millisecond) // program started, first frame rendered

	for _, r := range "hello first line" {
		p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		time.Sleep(30 * time.Millisecond)
	}
	p.Send(tea.KeyMsg{Type: tea.KeyCtrlJ})

	time.Sleep(100 * time.Millisecond)
	p.Quit()
	<-done
	v := m.input.View()
	if !strings.Contains(v, "hello first line") {
		t.Fatalf("after ctrl+j the first line vanished from the input view: %q", strings.Split(v, "\n"))
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// ctrl+k clears the conversation exactly as if /clear ran — messages reset to
// the system prompt, transcript blocks dropped, session detached — and the
// textarea's default delete-after-cursor binding must not shadow it.
func TestCtrlKClear(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(80, 30))
	m.appendRaw(blockText, "hello world")
	if got := len(m.agent.Messages); got != 1 {
		t.Fatalf("expected just the system prompt, got %d messages", got)
	}
	m.agent.Messages = append(m.agent.Messages,
		models.Message{Role: "user", Content: "hi", Authored: true})

	// a draft in the input box must survive — ctrl+k clears the CHAT
	m.input.SetValue("draft")

	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlK})
	m = tm.(*model)

	if got := len(m.agent.Messages); got != 1 {
		t.Fatalf("ctrl+k should reset messages to the system prompt, got %d", got)
	}
	// the old transcript blocks are gone; only the cleared notice remains
	if m.msgBlock != nil {
		t.Fatal("ctrl+k should drop the pending message block")
	}
	if len(m.blocks) != 1 || !strings.Contains(ansi.Strip(m.blocks[0].render(m.width)), "(conversation cleared)") {
		t.Fatalf("expected only the cleared notice block, got %d blocks", len(m.blocks))
	}
	if m.sessionID != "" {
		t.Fatalf("ctrl+k should detach the session, got %q", m.sessionID)
	}
	if got := m.input.Value(); got != "draft" {
		t.Fatalf("ctrl+k must not delete-after-cursor in the input, got %q", got)
	}
	if out := ansi.Strip(m.View()); !strings.Contains(out, "(conversation cleared)") {
		t.Fatalf("missing cleared notice in transcript: %q", out)
	}
}

// TestMain is a safety net: several TUI code paths persist through
// config.Save() (setEffort, switchModel, compactCommand). Without
// isolation those writes land in the REAL ~/.ghg/config.json — this exact
// bug corrupted the config twice. Point the whole test binary at a scratch
// GHG_HOME so even a future test that forgets t.Setenv cannot clobber the
// user's setup. Per-test overrides still apply on top.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghg-test-home")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv("GHG_HOME", dir)
	os.Exit(m.Run())
}

// fakeClock is a deterministic time source for key-repeat timing tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// navModel builds a model with a width-wide-enough input and a couple of
// submitted inputs in the history buffer, the way the live session does. The
// returned clock lets tests control the key-repeat window deterministically.
func navModel(history ...string) (*model, *fakeClock) {
	m := newGrowModel()
	m.hist = append([]string{}, history...)
	m.histIdx = len(m.hist) // not navigating
	clk := &fakeClock{t: time.Now()}
	m.now = clk.now
	return m, clk
}

func TestUpDownMovesWithinMultilineInput(t *testing.T) {
	m, clk := navModel("older", "newer")
	m.input.SetValue("first\nsecond")
	m.input.CursorEnd()

	if got := m.input.LineCount(); got != 2 {
		t.Fatalf("setup: want 2 logical lines, got %d (%q)", got, m.input.Value())
	}

	// cursor is on the last line; ↑ should move up within the input, not history
	startIdx := m.histIdx
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.input.Line() != 0 {
		t.Fatalf("↑ from the last line should move up within the input, got line %d (%q)", m.input.Line(), m.input.Value())
	}
	if m.histIdx != startIdx {
		t.Fatalf("↑ within the input must not walk history, histIdx %d→%d", startIdx, m.histIdx)
	}

	// now on the first line; a DELIBERATE ↑ (after a pause) rolls over to history
	clk.advance(500 * time.Millisecond)
	tm, _ = m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.histIdx != 1 {
		t.Fatalf("↑ on the first line should recall history, want histIdx 1, got %d (value=%q)", m.histIdx, m.input.Value())
	}
	if m.input.Value() != "newer" {
		t.Fatalf("expected history recall to load 'newer', got %q", m.input.Value())
	}
}

func TestDownOnLastLineRecallsNewerHistory(t *testing.T) {
	m, _ := navModel("older", "newer")
	// sitting on a recalled single-line history entry; ↓ should walk forward,
	// loading the next entry ("newer"), since the cursor is on its last row
	m.input.SetValue("older")
	m.histIdx = 0
	m.input.CursorEnd()

	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	if m.histIdx != 1 {
		t.Fatalf("↓ should recall newer history, want histIdx 1, got %d (value=%q)", m.histIdx, m.input.Value())
	}
	if m.input.Value() != "newer" {
		t.Fatalf("expected history recall to load 'newer', got %q", m.input.Value())
	}
}

func TestUpOnFirstLineOfSingleLineInputRecallsHistory(t *testing.T) {
	m, _ := navModel("solo")
	m.input.SetValue("editing")
	m.input.CursorEnd()

	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.input.Value() != "solo" {
		t.Fatalf("↑ on a single-line input should recall history, got %q", m.input.Value())
	}
}

func TestDownOnLastLineOfSingleLineInputOutsideHistoryIsNoop(t *testing.T) {
	m, _ := navModel("solo")
	m.histIdx = len(m.hist) // at the newest edge, nothing newer to recall
	m.input.SetValue("editing")
	m.input.CursorEnd()

	startVal := m.input.Value()
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	if m.input.Value() != startVal {
		t.Fatalf("↓ past the newest history entry should leave input unchanged, got %q", m.input.Value())
	}
}

// A long line that soft-wraps to two rows should let ↑/↓ move between the
// rows before rolling over to history, just like explicit newlines do.
func TestUpDownSoftWrapRowsCountAsLines(t *testing.T) {
	m, _ := navModel("hist")
	// one logical line, but wide enough to wrap to ≥2 visual rows
	m.input.SetValue(wrapString(m.input.Width() - 2))
	m.input.CursorEnd()

	// cursor is on the last wrapped row; ↑ should move up visually,
	// not recall history
	startIdx := m.histIdx
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.histIdx != startIdx {
		t.Fatalf("↑ within a soft-wrapped line must not walk history, histIdx %d→%d", startIdx, m.histIdx)
	}
	li := m.input.LineInfo()
	if li.RowOffset >= li.Height-1 {
		t.Fatalf("↑ should have moved off the last visual row (RowOffset=%d Height=%d)", li.RowOffset, li.Height)
	}
}

// Cross-session recall: a fresh session seeded with global user history (from
// every folder) must let ↑ walk back through all of it, newest first, exactly
// as session-local recall does.
func TestUpCyclesGlobalCrossSessionHistory(t *testing.T) {
	// hist holds the global seed oldest→newest (the TUI reverses the store's
	// newest-first UserHistory into this order at startup)
	m, clk := navModel("oldest across sessions", "from another folder", "most recent")
	m.input.SetValue("")
	m.input.CursorEnd()

	var got []string
	for i := 0; i < 3; i++ {
		tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
		m = tm.(*model)
		got = append(got, m.input.Value())
		clk.advance(500 * time.Millisecond) // deliberate presses, not a held key
	}
	want := []string{"most recent", "from another folder", "oldest across sessions"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("↑ press %d: got %q, want %q (full walk: %v)", i+1, got[i], want[i], got)
		}
	}
	// a 4th ↑ at the oldest entry is a no-op (stays put)
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.input.Value() != "oldest across sessions" {
		t.Fatalf("↑ past the oldest entry should stay, got %q", m.input.Value())
	}
}

// Regression: holding ↑ (key auto-repeat) past the top of a multi-line message
// must NOT walk back through history — the user is just trying to reach the
// start of the current message. Only a deliberate ↑ after a pause recalls.
func TestHeldUpStaysOnCurrentMessage(t *testing.T) {
	m, clk := navModel("older", "newer")
	m.input.SetValue("line one\nline two\nline three")
	m.input.CursorEnd()

	// hold ↑: repeats arrive 40ms apart. The cursor climbs to the top line…
	for i := 0; i < 2; i++ {
		tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
		m = tm.(*model)
		clk.advance(40 * time.Millisecond)
	}
	if m.input.Line() != 0 {
		t.Fatalf("held ↑ should have climbed to the first line, got line %d", m.input.Line())
	}
	// …and keeps going: every repeated press must be swallowed, never recall
	for i := 0; i < 10; i++ {
		tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
		m = tm.(*model)
		clk.advance(40 * time.Millisecond)
	}
	if m.histIdx != len(m.hist) {
		t.Fatalf("held ↑ must not walk history, histIdx=%d (value=%q)", m.histIdx, m.input.Value())
	}
	if m.input.Value() != "line one\nline two\nline three" {
		t.Fatalf("held ↑ must keep the current message, got %q", m.input.Value())
	}

	// releasing and deliberately pressing again DOES recall history
	clk.advance(500 * time.Millisecond)
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(*model)
	if m.input.Value() != "newer" {
		t.Fatalf("deliberate ↑ after a pause should recall history, got %q", m.input.Value())
	}
}

// wrapString builds a single string of spaces-separated words long enough to
// wrap to at least two rows at the given content width.
func wrapString(width int) string {
	if width < 4 {
		width = 4
	}
	w := []byte{}
	for len(w) < width*2+4 {
		w = append(w, 'w', 'o', 'r', 'd', ' ')
	}
	return string(w)
}

// busyQueueModel builds a model that is busy with a populated queue.
func busyQueueModel(queue ...string) *model {
	m := &model{
		input:    newInput(),
		agent:    &agent.Agent{},
		busy:     true,
		queue:    queue,
		queueSel: -1,
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	// a real busy model has a cancel func (set by submitTurn); the test
	// fixture needs one too so the empty-enter steer path can call it
	_, m.cancel = context.WithCancel(context.Background())
	return m
}

func press(t *testing.T, m *model, msg tea.KeyMsg) *model {
	t.Helper()
	tm, _ := m.key(msg)
	return tm.(*model)
}

func TestQueueNavigateAndSelect(t *testing.T) {
	m := busyQueueModel("first", "second", "third")

	// ↑ from the input selects the newest queued message
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.queueSel != 2 {
		t.Fatalf("↑ should select newest (index 2), got %d", m.queueSel)
	}
	// ↑ again moves older
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.queueSel != 1 {
		t.Fatalf("second ↑ should move to index 1, got %d", m.queueSel)
	}
	// ↓ moves back newer
	m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.queueSel != 2 {
		t.Fatalf("↓ should move to index 2, got %d", m.queueSel)
	}
	// ↓ off the end deselects
	m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.queueSel != -1 {
		t.Fatalf("↓ past the end should deselect, got %d", m.queueSel)
	}
}

func TestQueueDeleteSelected(t *testing.T) {
	m := busyQueueModel("first", "second", "third")

	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // select "third" (idx 2)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // select "second" (idx 1)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyDelete})
	if len(m.queue) != 2 || m.queue[0] != "first" || m.queue[1] != "third" {
		t.Fatalf("after deleting idx1: queue=%v", m.queue)
	}
	// selection clamps to the new last element
	if m.queueSel != 1 {
		t.Fatalf("selection should clamp to last index 1, got %d", m.queueSel)
	}
	// delete again removes "third", leaving "first"
	m = press(t, m, tea.KeyMsg{Type: tea.KeyDelete})
	if len(m.queue) != 1 || m.queue[0] != "first" {
		t.Fatalf("after second delete: queue=%v", m.queue)
	}
	if m.queueSel != 0 {
		t.Fatalf("selection should be 0, got %d", m.queueSel)
	}
	// delete the last one clears the queue and deselects
	m = press(t, m, tea.KeyMsg{Type: tea.KeyDelete})
	if len(m.queue) != 0 || m.queueSel != -1 {
		t.Fatalf("queue should be empty and deselected: %v sel=%d", m.queue, m.queueSel)
	}
}

func TestQueueBackspaceAlsoDeletes(t *testing.T) {
	m := busyQueueModel("only")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	if len(m.queue) != 0 {
		t.Fatalf("backspace should remove the selected queued message: %v", m.queue)
	}
}

func TestQueueNavOnlyWhenInputEmpty(t *testing.T) {
	m := busyQueueModel("queued")
	m.input.SetValue("typing something")
	m.input.CursorEnd()
	sel := m.queueSel
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.queueSel != sel {
		t.Fatalf("with text in the input, ↑ should edit history not the queue (sel %d→%d)", sel, m.queueSel)
	}
}

func TestQueueSelResetsOnSteer(t *testing.T) {
	m := busyQueueModel("a", "b")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.queueSel < 0 {
		t.Fatal("expected a selection")
	}
	// empty enter cancels the turn; the queue drains in turnDoneMsg
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.cancel == nil {
		t.Fatal("expected the turn to be canceled for immediate steering")
	}
	if len(m.queue) == 0 {
		t.Fatal("queue should persist until turnDoneMsg drains it")
	}
}

// TestBusyCmdAllowList pins which commands run mid-turn (and which /goal
// forms do) — anything else must queue as a message.
func TestBusyCmdAllowList(t *testing.T) {
	runs := []string{
		"/help", "/effort", "/effort high",
		"/tasks", "/tasks abc123", "/goal", "/goal clear", "/goal rounds 5",
		"/cd", "/cd /tmp", "/pwd",
	}
	for _, c := range runs {
		if !busyCmd(c) {
			t.Errorf("%q should run mid-turn", c)
		}
	}
	queues := []string{
		"/goal resume", "/goal ship the release", "/model", "/model x",
		"/compact", "/clear", "/fork", "/rename", "/resume", "/quit",
		"/bogus", "hello",
	}
	for _, c := range queues {
		if busyCmd(c) {
			t.Errorf("%q should queue, not run mid-turn", c)
		}
	}
}

func TestEnterWhileBusyRunsSettingsCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep cfg.Save() away from the real config
	m := busyQueueModel()
	m.cfg = &config.Config{}
	m.input.SetValue("/effort high")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.queue) != 0 {
		t.Fatalf("/effort should run now, not queue: %v", m.queue)
	}
	if m.agent.Effort != "high" {
		t.Fatalf("effort should have changed to high, got %q", m.agent.Effort)
	}
	for _, b := range m.blocks {
		if strings.Contains(b.text, "⚡ effort:") {
			t.Fatalf("effort changes should not append routine notes, got %v", m.blocks)
		}
	}
	if m.hist[len(m.hist)-1] != "/effort high" {
		t.Fatalf("the command should be in history: %v", m.hist)
	}
}

func TestEnterWhileBusyQueuesOtherCommands(t *testing.T) {
	m := busyQueueModel()
	m.input.SetValue("/model gpt-5")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 1 || m.queue[0] != "/model gpt-5" {
		t.Fatalf("/model should still queue while busy: %v", m.queue)
	}
}

func TestEnterWhileBusyQueuesGoalSubmits(t *testing.T) {
	m := busyQueueModel()
	m.input.SetValue("/goal ship it")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 1 || m.queue[0] != "/goal ship it" {
		t.Fatalf("/goal <text> submits a turn and must queue: %v", m.queue)
	}

	m = busyQueueModel()
	m.input.SetValue("/goal resume")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 1 || m.queue[0] != "/goal resume" {
		t.Fatalf("/goal resume submits a turn and must queue: %v", m.queue)
	}
}

func TestEnterWhileBusyRunsGoalSettings(t *testing.T) {
	m := busyQueueModel()
	m.goal = "old goal"
	m.input.SetValue("/goal clear")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.goal != "" {
		t.Fatalf("/goal clear should run now, goal=%q", m.goal)
	}
	if len(m.queue) != 0 {
		t.Fatalf("/goal clear should not queue: %v", m.queue)
	}
}

func TestEscInterruptsMidResponse(t *testing.T) {
	m := &model{input: newInput(), agent: &agent.Agent{}, busy: true}
	m.width = 80
	m.input.SetWidth(78)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel

	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if ctx.Err() != context.Canceled {
		t.Fatalf("esc should cancel the in-flight turn, ctx.Err=%v", ctx.Err())
	}
}

func TestEscDoesNotInterruptWhenIdle(t *testing.T) {
	m := &model{input: newInput(), agent: &agent.Agent{}, busy: false}
	m.width = 80
	m.input.SetWidth(78)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel // set but not busy

	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if ctx.Err() == context.Canceled {
		t.Fatal("esc while idle should not cancel")
	}
}

// stubLLM answers chat completions with an immediate empty SSE stream so a
// drained queue can submit without touching the network.
func stubLLM() models.Backend {
	backend, err := models.NewBackend(models.Resolved{
		BaseURL:  "http://stub",
		Protocol: models.ProtocolOpenAIChatCompletions,
	}, models.BackendOptions{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
	})
	if err != nil {
		panic(err)
	}
	return backend
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEmptyEnterSteerDrainsQueue proves the reported regression end to end:
// queue a message while busy, empty-enter to steer (cancels the turn), then
// the turn ends with the wrapped cancellation error an http client actually
// returns ("Post ...: context canceled"). The queue must still drain — the
// queued message submits as the next turn.
func TestEmptyEnterSteerDrainsQueue(t *testing.T) {
	m := busyQueueModel()
	m.agent.Backend = stubLLM()
	m.agent.Messages = []models.Message{{Role: "system", Content: "sys"}}

	// first enter while busy: the typed message queues
	m.input.SetValue("thanks we're all done")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 1 || m.queue[0] != "thanks we're all done" {
		t.Fatalf("typed text should queue while busy: %v", m.queue)
	}

	// second enter on the empty input: force-steer cancels the in-flight turn
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// the canceled http request reports a *url.Error wrapping context.Canceled,
	// not the sentinel itself
	wrapped := fmt.Errorf("Post %q: %w", "https://api.example/v1/chat/completions", context.Canceled)
	tm, _ := m.Update(turnDoneMsg{err: wrapped})
	m = tm.(*model)

	if len(m.queue) != 0 {
		t.Fatalf("the canceled turn should drain the queue, still queued: %v", m.queue)
	}
	if !m.busy {
		t.Fatal("the queued message should have submitted as the next turn (busy)")
	}
	if !hasUserMsg(t, m, "thanks we're all done") {
		t.Fatalf("the queued message should be submitted to the model, got %+v", m.agent.Messages)
	}
}

// hasUserMsg reports whether the conversation holds the given user message.
// submitTurn appends it from a goroutine, so read via the agent's published
// snapshot (never the live slice) and poll briefly rather than assert on the
// first read.
func hasUserMsg(t *testing.T, m *model, content string) bool {
	t.Helper()
	for range 100 {
		for _, msg := range m.agent.MessagesSnapshot() {
			if msg.Role == "user" && msg.Content == content {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestEmptyEnterSteerDrainsQueueOnSentinel covers the unwrapped sentinel too
// (agent.go's post-tool `return ctx.Err()` path).
func TestEmptyEnterSteerDrainsQueueOnSentinel(t *testing.T) {
	m := busyQueueModel()
	m.agent.Backend = stubLLM()
	m.agent.Messages = []models.Message{{Role: "system", Content: "sys"}}
	m.queue = []string{"follow up"}

	tm, _ := m.Update(turnDoneMsg{err: context.Canceled})
	m = tm.(*model)

	if len(m.queue) != 0 {
		t.Fatalf("the canceled turn should drain the queue, still queued: %v", m.queue)
	}
	if !m.busy {
		t.Fatal("the queued message should have submitted as the next turn (busy)")
	}
}

// TestEmptyEnterIdleDrainsStuckQueue is the recovery path for the stuck state
// the bug left sessions in: idle with a stranded queue, empty enter does
// nothing. It must submit the head of the queue.
func TestEmptyEnterIdleDrainsStuckQueue(t *testing.T) {
	m := busyQueueModel("stranded")
	m.busy = false // idle — the turn already ended without draining
	m.agent.Backend = stubLLM()
	m.agent.Messages = []models.Message{{Role: "system", Content: "sys"}}

	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.queue) != 0 {
		t.Fatalf("empty enter while idle should drain the stuck queue: %v", m.queue)
	}
	if !m.busy {
		t.Fatal("the stranded message should have submitted (busy)")
	}
	if !hasUserMsg(t, m, "stranded") {
		t.Fatalf("the stranded message should be submitted, got %+v", m.agent.Messages)
	}
}

func TestQueueViewShowsSelection(t *testing.T) {
	m := busyQueueModel("first", "second")
	m.queueSel = 1
	m.agent.Model = "m"
	m.provName = "p"
	view := m.View()
	if !strings.Contains(view, "del to remove") {
		t.Errorf("selected queued message should show a delete hint:\n%s", view)
	}
	if !strings.Contains(view, "↑/↓ select") {
		t.Errorf("queue footer should advertise navigation:\n%s", view)
	}
}

// A long queued message must render as ONE line, not wrap to full height —
// otherwise a couple of long queued messages crowd out the transcript.
func TestQueueRendersOneLineEach(t *testing.T) {
	long := strings.Repeat("a very long queued message that would wrap to many lines ", 6)
	m := busyQueueModel("short", long, "another "+strings.Repeat("word ", 30))
	view := m.View()

	for _, q := range m.queue {
		// count rendered lines containing this message's content
		needle := q
		if len(needle) > 20 {
			needle = needle[:20] // a prefix survives truncation
		}
		count := 0
		for _, line := range strings.Split(view, "\n") {
			if strings.Contains(ansi.Strip(line), ansi.Strip(needle)) {
				count++
			}
		}
		if count > 1 {
			t.Errorf("queued message rendered on %d lines (want 1): %.40q…", count, q)
		}
	}
}

// idleModel builds a model that is not busy (no agent turn running).
func idleModel() *model {
	m := &model{
		input: newInput(),
		agent: &agent.Agent{},
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	return m
}

// A single ctrl+c at idle must NOT quit; it arms a confirm window. The second
// ctrl+c within the window quits. After the window expires (quitArmMsg), the
// next press re-arms instead of quitting.
func TestCtrlCRequiresDoublePressToQuit(t *testing.T) {
	m := idleModel()

	// first press: arms, no quit
	tm, cmd := m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(*model)
	if !m.quit1 {
		t.Fatal("first ctrl+c should arm quit1")
	}
	if cmd != nil {
		// the cmd is the arm-window ticker, not tea.Quit
		if _, isQuit := cmd().(tea.QuitMsg); isQuit {
			t.Fatal("first ctrl+c must not quit")
		}
	}

	// second press within the window: quits
	tm, cmd = m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(*model)
	if cmd == nil {
		t.Fatal("second ctrl+c should produce a quit command")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatalf("second ctrl+c should quit, got %T", cmd())
	}
	if m.quit1 {
		t.Fatal("quit1 should clear after the confirming press")
	}
}

// After the arm window expires, a lone ctrl+c re-arms rather than quitting.
func TestCtrlCArmExpires(t *testing.T) {
	m := idleModel()
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(*model)
	if !m.quit1 {
		t.Fatal("first press should arm")
	}

	// simulate the 2s window elapsing
	tm2, _ := m.Update(quitArmMsg{})
	m = tm2.(*model)
	if m.quit1 {
		t.Fatal("quitArmMsg should disarm quit1")
	}

	// now a single press re-arms (no quit)
	tm, cmd := m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(*model)
	if !m.quit1 {
		t.Fatal("post-expiry press should re-arm, not quit")
	}
	if cmd != nil {
		if _, isQuit := cmd().(tea.QuitMsg); isQuit {
			t.Fatal("post-expiry single press must not quit")
		}
	}
}

// The busy path is unchanged: first ctrl+c arms the interrupt, second cancels
// the in-flight turn — it never quits the program outright.
func TestCtrlCBusyInterruptsNotQuits(t *testing.T) {
	m := idleModel()
	m.busy = true
	cancelled := false
	m.cancel = func() { cancelled = true }

	_, cmd := m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.interrupt1 {
		t.Fatal("busy first press should arm interrupt1")
	}
	if cancelled {
		t.Fatal("busy first press must not cancel yet")
	}
	if cmd != nil {
		if _, isQuit := cmd().(tea.QuitMsg); isQuit {
			t.Fatal("busy path must never quit")
		}
	}

	_, cmd = m.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !cancelled {
		t.Fatal("busy second press should cancel the turn")
	}
	if cmd != nil {
		if _, isQuit := cmd().(tea.QuitMsg); isQuit {
			t.Fatal("busy path must never quit")
		}
	}
}

// A terminal resize re-renders the whole transcript: assistant markdown
// reflows, status lines re-wrap, nothing stays at the stale width.
func TestResizeE2E(t *testing.T) {
	m := compactCmdModel()
	m.Update(mkWinSize(100, 30))
	m.appendAssistant("Here is a paragraph long enough to wrap differently at different widths, plus a fence.\n\n```go\nfunc example() { fmt.Println(\"" + strings.Repeat("wide", 12) + "\") }\n```")
	m.append(dimStyle.Render("◎ compacted — summarized 42 msgs, 12 kept"))

	vpLines := func() []string {
		return strings.Split(ansi.Strip(m.vp.View()), "\n")
	}
	wide := vpLines()

	// shrink: every viewport line must fit the new width
	tm, _ := m.Update(mkWinSize(48, 30))
	m = tm.(*model)
	narrow := vpLines()
	for _, l := range narrow {
		if w := ansi.StringWidth(l); w > 48 {
			t.Fatalf("viewport line exceeds 48 cols after resize: %q", l)
		}
	}
	// heights must differ — content actually reflowed
	if strings.Join(wide, "\n") == strings.Join(narrow, "\n") {
		t.Errorf("viewport content identical after shrink — no reflow")
	}
}

type signalTestModel struct {
	ctrlCs chan struct{}
	count  int
}

func (m *signalTestModel) Init() tea.Cmd { return nil }

func (m *signalTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || key.Type != tea.KeyCtrlC {
		return m, nil
	}
	m.count++
	if m.count == 1 {
		close(m.ctrlCs)
		return m, nil
	}
	return m, tea.Quit
}

func (*signalTestModel) View() string { return "signal test" }

func TestForwardSignalsPreservesDoubleCtrlC(t *testing.T) {
	m := &signalTestModel{ctrlCs: make(chan struct{})}
	p := tea.NewProgram(m,
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithoutSignalHandler(),
	)
	stopSignals := forwardSignals(p)
	defer stopSignals()

	done := make(chan struct{})
	var (
		final  tea.Model
		runErr error
	)
	go func() {
		final, runErr = p.Run()
		close(done)
	}()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.ctrlCs:
	case <-time.After(time.Second):
		p.Kill()
		t.Fatal("first interrupt was not forwarded")
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		p.Kill()
		t.Fatal("second interrupt did not quit the program")
	}
	if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) {
		t.Fatalf("double ctrl+c should quit gracefully, got %v", runErr)
	}
	if got := final.(*signalTestModel).count; got != 1 {
		t.Fatalf("forwarded ctrl+c count = %d, want 1 (second interrupt executes immediate emergency kill)", got)
	}
}
