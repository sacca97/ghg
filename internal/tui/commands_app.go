package tui

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	workerwire "github.com/sacca97/ghg/internal/worker"
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

// shell.go — the `!` shell escape. The worker executes the command through
// the normal bash tool so policy, approval, and sandboxing stay in one place.

// shellDoneMsg reports a finished `!` command for the transcript.
type shellDoneMsg struct {
	cmd string
	out string // formatted like bash-tool output (truncated, exit markers)
}

// runShell starts a `!`-prefixed input line. Queued `!` lines are sent when the
// queue drains rather than being submitted to the model.
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
	if m.workerClient == nil && !m.ensureWorker() {
		m.append(errStyle.Render("shell unavailable: worker unavailable: " + m.workerStartError))
		return
	}
	if err := m.workerClient.Send(workerwire.CommandShell, workerRequestID("shell"), workerwire.ShellRequest{Command: cmdLine}); err != nil {
		m.append(errStyle.Render("shell failed: " + err.Error()))
	}
}

// applyShellDone lands a finished command in the transcript and sends its
// output to the worker-owned conversation.
func (m *model) applyShellDone(msg shellDoneMsg) {
	// transcript: a tool-style block (collapsed preview, ctrl+e/click expand)
	m.appendRaw(blockTool, msg.out)
	content := "$ " + msg.cmd + "\n" + msg.out
	if m.workerClient == nil && !m.ensureWorker() {
		m.append(errStyle.Render("shell output not shared with the model: worker unavailable: " + m.workerStartError))
		return
	}
	if err := m.workerClient.Send(workerwire.CommandAppend, workerRequestID("append"), workerwire.AppendRequest{Content: content}); err != nil {
		m.append(errStyle.Render("shell output not shared with the model: " + err.Error()))
	}
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
