package tui

import (
	"context"
	"fmt"
	"github.com/sacca97/ghg/internal/tools"
	"github.com/sacca97/ghg/internal/tools/bashrun"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// /report — a bug-report bundle: one transcript block with a clickable OSC 8
// link to a prefilled GitHub issue and a copy-pastable environment snippet.
// The audience is someone (often not the ghg developer) hitting a terminal
// rendering problem — wrong colors, mangled glyphs, tmux weirdness — so the
// bundle leads with terminal identity. Strict
// whitelist: only the env vars named below are read, never API keys/secrets,
// never conversation content. Live-only: nothing is persisted or submitted;
// the user clicks the link or pastes the snippet themselves.
//
// Version is the ghg build version, set by cmd/ghg (ldflags -X main.version)
// before tui.Run.
var Version = "dev"

const issueBase = "https://github.com/sacca97/ghg/issues/new"

// envRow is one line of the bundle: an aligned key/value pair.
type envRow struct {
	key, val string
}

// envReport is the collected bundle as pure data (testable), plus the two
// rendered forms (prefilled-issue URL, fenced snippet).
type envReport struct {
	rows    []envRow
	link    string
	snippet string
}

// envReport collects the environment bundle.
func (m *model) envReport() envReport {
	var r envReport
	add := func(k, v string) {
		if v != "" {
			r.rows = append(r.rows, envRow{k, v})
		}
	}

	// ghg
	add("ghg", Version)
	add("model", m.modelName)
	add("provider", m.provName)
	add("mouse", onOff(m.mouseOn))
	add("session", m.sessionID)

	// terminal
	add("TERM", os.Getenv("TERM"))
	if tp := os.Getenv("TERM_PROGRAM"); tp != "" {
		if v := os.Getenv("TERM_PROGRAM_VERSION"); v != "" {
			tp += " " + v
		}
		add("TERM_PROGRAM", tp)
	}
	add("COLORTERM", os.Getenv("COLORTERM"))
	add("COLORFGBG", os.Getenv("COLORFGBG"))
	if tm := os.Getenv("TMUX"); tm != "" {
		v := tm
		if out, err := exec.Command("tmux", "-V").Output(); err == nil {
			v = strings.TrimSpace(string(out))
		}
		add("tmux", v)
	}
	add("SHELL", os.Getenv("SHELL"))
	if lc := os.Getenv("LC_ALL"); lc != "" {
		add("locale", lc)
	} else {
		add("locale", os.Getenv("LANG"))
	}
	if m.width > 0 {
		add("size", fmt.Sprintf("%dx%d", m.width, m.height))
	}
	if os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		add("ssh", "yes")
	}

	// system
	add("os", runtime.GOOS+"/"+runtime.GOARCH)
	if out, err := exec.Command("uname", "-srm").Output(); err == nil {
		add("uname", strings.TrimSpace(string(out)))
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			add("macOS", strings.TrimSpace(string(out)))
		}
	}
	add("go", runtime.Version())

	r.snippet = r.snippetText()
	r.link = issueURL(r.snippet)
	return r
}

// snippetText renders the rows as an aligned list inside a fenced code block.
// No styling or hyperlinks: this is the copy-paste form and must survive any
// clipboard path verbatim.
func (r envReport) snippetText() string {
	w := 0
	for _, row := range r.rows {
		if len(row.key) > w {
			w = len(row.key)
		}
	}
	var b strings.Builder
	b.WriteString("```\n")
	for _, row := range r.rows {
		fmt.Fprintf(&b, "%-*s %s\n", w, row.key, row.val)
	}
	b.WriteString("```")
	return b.String()
}

// issueURL builds the prefilled new-issue URL: a fill-in skeleton on top and
// the environment bundle in a fenced code block at the bottom.
func issueURL(snippet string) string {
	body := "### What happened\n\n\n\n### Expected\n\n\n\n### Environment\n\n" + snippet + "\n"
	v := url.Values{}
	v.Set("title", "")
	v.Set("body", body)
	return issueBase + "?" + v.Encode()
}

// reportBlock renders the transcript block: a one-line intro, the clickable
// prefilled-issue link (OSC 8 — the terminal owns the click), then the
// snippet for copy/paste.
func (m *model) reportBlock() string {
	r := m.envReport()
	return "Bug report — " + hyperlink(r.link, "open a prefilled GitHub issue") +
		" (or paste the snippet below into an existing issue):\n\n" + r.snippet
}

// shell.go — the `!` shell escape.
//
// `!cmd` runs cmd locally with the same runner the agent's bash tool uses and
// lands the output in the transcript AND in the conversation (the model sees
// it at the next request) — opencode's `!` shell submit
// (packages/tui/.../prompt/index.tsx:1059 → session.shell), minus the
// shell-mode chrome (ponytail: border/placeholder swap, esc/backspace-at-0
// exit; the prefix at submit time covers the common case).
//
// Concurrency: the command runs on its own goroutine so a slow command never
// freezes the UI, and the conversation append happens on the TUI goroutine
// inside the shellDoneMsg handler — the turn goroutine owns Agent.Messages
// while busy, so mid-turn output goes through Agent.Steer (mutex-guarded,
// injected at the next loop boundary) instead of a raw append. Esc does NOT
// cancel a running `!` command (esc stays bound to interrupting the turn;
// the 120s cap bounds it). ponytail: a second cancel path if long-running
// escapes become a pattern.

// shellDoneMsg reports a finished `!` command: the transcript block and the
// conversation message are applied on the TUI goroutine.
type shellDoneMsg struct {
	cmd string
	out string // formatted like bash-tool output (truncated, exit markers)
}

// runShell starts a `!`-prefixed input line: echo to the transcript now, run
// the command on a goroutine, land the result on shellDoneMsg. Safe while a
// turn is running; queued `!` lines also execute when the queue drains rather
// than being submitted to the model.
func (m *model) runShell(text string) {
	m.startShell(text, true)
}

// runShellQueued runs a `!` line drained from the queue without re-echoing it
// (the queue view already rendered it, like submit for queued messages).
func (m *model) runShellQueued(text string) {
	m.startShell(text, false)
}

func (m *model) startShell(text string, echo bool) {
	cmdLine := strings.TrimSpace(strings.TrimPrefix(text, "!"))
	if cmdLine == "" {
		m.append(dimStyle.Render("(! <command> — run a shell command, output shared with the model)"))
		return
	}
	if echo {
		m.flushThink()
		m.flushCurrent() // don't split the in-flight assistant line with the echo
		m.append(youStyle.Render("❯ ") + text)
	}

	if m.prog == nil {
		// headless (tests): run inline and apply the result directly
		out := shellExec(cmdLine)
		m.applyShellDone(shellDoneMsg{cmd: cmdLine, out: out})
		return
	}
	p := m.prog
	go func() {
		p.Send(shellDoneMsg{cmd: cmdLine, out: shellExec(cmdLine)})
	}()
}

// shellExec runs one shell-escape command, formatting the result exactly like
// the bash tool does (tail truncation, exit/timeout markers).
func shellExec(cmdLine string) string {
	// context.Background is deliberate: the command is independent of any
	// turn, and esc stays bound to turn interruption. The 120s cap bounds it.
	res := bashrun.Run(context.Background(), bashrun.Options{Command: cmdLine})
	out := tools.TruncateTail(res.Output)
	if res.TimedOut {
		out += "\n(command timed out)"
	} else if res.Exit != "" {
		out = fmt.Sprintf("%s\n(%s)", out, res.Exit)
	}
	if strings.TrimSpace(out) == "" {
		out = "(no output)"
	}
	return out
}

// applyShellDone lands a finished command in the transcript and conversation.
func (m *model) applyShellDone(msg shellDoneMsg) {
	// transcript: a tool-style block (collapsed preview, ctrl+e/click expand)
	m.appendRaw(blockTool, msg.out)
	content := "$ " + msg.cmd + "\n" + msg.out
	if m.workerOnly {
		if m.workerClient == nil && !m.ensureWorker() {
			m.append(errStyle.Render("shell output not shared with the model: worker unavailable: " + m.workerStartError))
			return
		}
		if err := m.workerClient.Send(workerwire.CommandAppend, workerRequestID("append"), workerwire.AppendRequest{Content: content}); err != nil {
			m.append(errStyle.Render("shell output not shared with the model: " + err.Error()))
		}
		return
	}
	if m.agent == nil {
		m.append(dimStyle.Render("shell output kept local — configure a provider with /auth before sending it to the model"))
		return
	}

	if m.workerClient != nil {
		// The worker owns the durable conversation; the shadow agent's copy
		// stays render-only. The worker decides Steer-vs-append by its own
		// busy state, which is what actually gates the next request.
		if err := m.workerClient.Send(workerwire.CommandAppend, workerRequestID("append"), workerwire.AppendRequest{Content: content}); err != nil {
			m.append(errStyle.Render("shell output not shared with the model: " + err.Error()))
		}
		return
	}
	if m.busy {
		// mid-turn: the turn goroutine owns Messages; steer injects the output
		// at the next loop boundary, where OnSteer echoes it to the transcript.
		m.agent.Steer(content)
		return
	}
	// idle: non-authored so input-history recall skips it; the "$ " prefix
	// keeps resumed transcripts self-explanatory.
	m.agent.AppendUser(content)
	m.persist()
}

func (m *model) saveConfig() error {
	if m.cfg == nil {
		return nil
	}
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
		return err
	}
	return nil
}
