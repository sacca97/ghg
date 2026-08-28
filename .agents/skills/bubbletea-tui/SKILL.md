---
name: bubbletea-tui
description: "Bubbletea/lipgloss TUI work in internal/tui — the Elm loop, tea.Cmd vs goroutine, sending into a running program without blocking, layout and resize, and testing a TUI with no tty. Use when changing internal/tui, adding a slash command or keybinding, or debugging a render, focus, or race bug."
user-invocable: true
license: MIT
compatibility: This repo (github.com/sacca97/ghg). Assumes charmbracelet/bubbletea v1.
metadata:
  emoji: "🖥️"
allowed-tools: Read Edit Write Glob Grep Bash(go:*)
paths:
  - "internal/tui/**"
---

# Bubbletea TUI

`internal/tui` is the largest and most concurrent package in this repo: one
`model`, one event loop, and a dozen goroutines (agent turns, background
subagents, MCP/LSP managers, the schedule ticker) that all want to talk to it.
Almost every bug here is one of the five below.

## The loop

`Init() tea.Cmd` → `Update(tea.Msg) (tea.Model, tea.Cmd)` → `View() string`,
serialized on one goroutine. `Update` is a **pointer method** on `*model`
(`tui.go:1429`), so there is no defensive struct copy between turns — a slice
you hand out is shared, and anything mutated from another goroutine is a race.

**The rule:** state changes happen in `Update`, never anywhere else. Work that
blocks goes into a `tea.Cmd` (returns a `tea.Msg`) or a goroutine that *sends*
a message back.

## 1. Never block the event loop

`p.Send` parks on the program's message channel. If the loop is not draining —
mid-turn, mid-render, wedged — a direct `Send` from a worker goroutine blocks
that worker forever. Detach it:

```go
func sendTaskMsg(p *tea.Program, msg taskEventMsg) {
    if p == nil {
        return // headless tests
    }
    go p.Send(msg)
}
```

(`tasks.go:42`; the regression test is `TestSendTaskMsgNeverBlocksWorker`.)
Every path that delivers into the program from a worker goes through a helper
like this. The `p == nil` guard is what makes headless construction possible —
see §5.

## 2. `tea.Cmd` vs goroutine

- **`tea.Cmd`** — bounded work whose single result is a message. The runtime
  runs it and feeds the result back. Preferred: no lifetime to manage.
- **Goroutine + `p.Send`** — long-lived or multi-event producers (a streaming
  turn, a subagent, a ticker). You own cancellation. Thread the `ctx` that the
  keypress created; ctrl+c must actually stop in-flight work.

Never call `p.Send` from inside `Update` — you are already on that goroutine
and will deadlock. Return a `tea.Cmd` instead.

## 3. Layout, resize, and the input box

`layout()` (`tui.go:1371`) recomputes viewport and input heights from
`m.width/m.height`; `growInput()` (`tui.go:1348`) resizes the textarea to its
content. Two traps:

- A component's **internal** scroll offset survives a resize. The `ctrl+j`
  regression came from the textarea keeping a `YOffset` computed at height 1,
  which scrolled the first line out of view after the box grew; the fix resets
  the scroll via `SetValue` after inserting the newline.
- `lipgloss.Height`/`Width` measure **rendered** strings including ANSI. Measure
  after styling, not before, and strip with `ansi.Strip` when asserting in tests.

Anything width-dependent (markdown, diffs, tool rows) re-renders on
`tea.WindowSizeMsg`. Cache by width if it is expensive.

## 4. Adding a command or keybinding

One registry drives the settings, slash completion, `/help`, and footer hints:
add the entry to `registry.go` and wire the handler in `m.command()` /
`m.key()`. Do not add a bare `case` in the key switch without a registry row —
the command becomes undiscoverable and the settings silently omits it.

## 5. Testing without a tty

Two modes, both used heavily in `internal/tui/*_test.go`:

- **Headless (default).** Build the model directly, leave `m.prog == nil`, and
  drive it with `m.Update(msg)` / `m.command("/x")` / `m.key(mkKey("enter"))`,
  asserting on `m.blocks`, `m.statusView()`, etc. Fast and deterministic. Code
  paths that would `Send` must be nil-guarded (§1) for this to work.
- **Live program**, only when the bug needs the real runtime (cursor blink,
  deferred rebuilds, scroll offsets):

  ```go
  p := tea.NewProgram(m, tea.WithOutput(nopWriter{}),
      tea.WithInput(strings.NewReader("")), tea.WithoutSignalHandler())
  ```

  **Never read model fields from the test goroutine** — that races the loop.
  Send a probe and read the answer off a channel:

  ```go
  ch := make(chan string, 1)
  p.Send(viewProbe{fn: func(m *model) { ch <- m.input.View() }})
  ```

  (`probe.go`, dispatched at `tui.go:1432`.) Always `defer func() { p.Kill(); <-done }()`.

Run `go test -race ./internal/tui` for anything touching a goroutine. Note that
`TestMain` points `HARNESS_HOME` at a scratch dir and pins the dark theme —
tests that persist through `config.Save()` rely on it; do not remove it.

## Anti-patterns

- Mutating `m` from a goroutine instead of sending a message.
- `p.Send` inside `Update`, or an undetached `p.Send` from a worker.
- Reading model state from a test goroutine while a program runs.
- A new keybinding with no `registry.go` row.
- `context.Background()` in a turn path — cancellation must reach the tools.
