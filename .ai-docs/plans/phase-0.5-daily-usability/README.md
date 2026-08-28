# Phase 0.5 — Daily usability

Status: COMPLETE

## Goal

Finish the small daily-use improvements that the fork needs before the larger
provider and artifact work: trusted project instructions, visible progress from
long-running bash calls, and a one-flag session continuation path.

## Scope

- Read `AGENTS.md` only from the trusted project root and append it beside the
  existing user `me.md` block in the system prompt. Missing files are normal;
  unreadable or oversized files must not make startup fail or enter the prompt.
- Add `--continue` to the interactive command. It resolves the newest session
  with persisted messages for the current working directory and passes its
  concrete id to the existing resume path. An empty match is an actionable
  error; it must not silently create a new session.
- Add accumulated bash-output snapshots, throttled to 100ms per invocation.
  Carry the callback through a per-call context value, preserve tool-call ids
  in agent events, and show only the last three lines beneath a running TUI
  tool row. Completion remains the source of truth and collapses the row.

## Non-goals

- Do not change production retry defaults.
- Do not port upstream bash output spill; Phase 1's artifact store owns that
  behavior.
- Do not introduce a second session format, a global output callback, or a
  parallel memory/instruction system.

## Design notes

- Project instructions are user-authored input and are gated by the same
  absolute working-directory trust record used by the TUI. The loader is
  bounded and treats missing/unreadable input as absent.
- The session query belongs on `internal/session.Store`, so the CLI and future
  callers share the same definition of “recent” as the picker.
- `internal/tools.WithOnUpdate` carries one callback in the context. The bash
  runner owns a ticker and synchronization for each process; no package-level
  mutable callback exists, so parallel tool calls cannot cross wires.
- TUI messages are the concurrency boundary: agent callbacks only enqueue
  messages, and the model alone mutates rendered tool rows.

## Verification

- Focused tests for trusted instruction loading, current-directory continuation,
  bash update throttling, agent event routing, and TUI tail rendering.
- `gofmt` on changed Go files.
- Focused package tests and race tests on the changed concurrency paths pass.
- `go test ./... -skip TestBashSudoFastFail`, `go vet ./...`,
  `CGO_ENABLED=0 go build ./...`, `go mod verify`, and `git diff --check` pass.
  The full TUI race run still exposes the pre-existing Chroma initialization
  race in `TestScheduleFiresWakeup`; it is outside this phase's changes.

## Checklist

- [x] Implement and test `AGENTS.md` loading.
- [x] Implement and test `--continue`.
- [x] Implement and test streamed tool output.
- [x] Update `docs/features.md`, `docs/tools.md`, `docs/roadmap.md`, and
  `plan.md`.
- [x] Run verification and mark this plan complete.
