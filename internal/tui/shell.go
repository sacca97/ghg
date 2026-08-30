package tui

// shell.go — the `!` shell escape and the /cd & /pwd directory commands.
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

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sacca97/ghg/internal/tools"
	"github.com/sacca97/ghg/internal/tools/bashrun"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

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
	if m.agent == nil {
		m.append(dimStyle.Render("shell output kept local — configure a provider with /auth before sending it to the model"))
		return
	}

	content := "$ " + msg.cmd + "\n" + msg.out
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

// cdCommand changes ghg's working directory for everything (bash tool,
// relative read/write/edit paths, @ file index). Bare prints it. A command
// already running under the old cwd keeps it (POSIX); ghg's next spawns —
// and the next session record — use the new one.
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
	if err := os.Chdir(arg); err != nil {
		m.append(errStyle.Render("/cd: " + err.Error()))
		return
	}
	m.append(dimStyle.Render("→ " + cwd()))
	// The worker owns the tools and sandbox; a TUI-side chdir alone leaves it
	// reading and editing the original workspace under the original policy.
	if m.workerClient != nil {
		if err := m.workerClient.Send(workerwire.CommandChdir, workerRequestID("chdir"), cwd()); err != nil {
			m.append(errStyle.Render("/cd: worker: " + err.Error()))
		}
	}
}
