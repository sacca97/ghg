---
name: new-feature-development
description: "Design and implement a net-new ghg feature, tool, command, integration, or user-visible behavior. Use for multi-surface additions; not for a narrow bug fix, cleanup, or routine Go edit."
---

# New Feature Development

Build the smallest feature that fits ghg's current architecture and roadmap. Do not
introduce a second mechanism when an existing seam can carry the behavior.

## Establish the boundary

Read only what the feature needs:

- `plan.md` for phase ownership, gates, and deferred/cut decisions. A deferred item
  requires the user to reopen it before implementation.
- The relevant section of `docs/features.md` for shipped behavior and its code/tests.
- `docs/roadmap.md` and `docs/learnings/other-harnesses/` only when comparing or porting
  another harness.
- `docs/concurrency.md` before adding goroutines, channels, registries, or lifecycle
  state.

Clarify only an ambiguity that would materially change the result. For a broad or
multi-surface feature, keep a living plan under `.ai-docs/plans/<slug>/README.md`;
small, well-specified additions do not need ceremony.

## Fit the existing seams

Locate the owning surface before editing:

- Agent-loop behavior: `internal/agent`.
- Native tools and bounded `ToolResult` output: `internal/tools`.
- TUI rendering and commands: `internal/tui`.
- User configuration and provider routing: `internal/config` and `internal/provider`.
- Durable session/event state: `internal/session`; artifact payloads: `internal/artifact`.
- OS confinement and capability policy: `internal/sandbox` plus the per-agent tool
  runtime. New executable paths must use it rather than calling `os/exec` independently.

Preserve these invariants:

- Provider-neutral agent state; protocol-specific conversion stays in `internal/llm`.
- The raw session log remains authoritative; compacted prompts are derived views.
- Tool output is bounded, typed, marked untrusted where applicable, and recoverable
  through artifacts when bytes are omitted.
- Parallel mutations use the existing canonical-path/global locking and atomic
  publication paths.
- Cancellation reaches tools and long-lived components; every goroutine has an owner
  and an exit.
- TUI state changes land through Bubble Tea messages, not worker-goroutine mutation.
- Config/state writes use existing guarded atomic save paths.
- Child agents inherit or narrow capabilities and shared session state; they do not
  silently receive broader defaults.

Use `golang-concurrency`, `golang-context`, `golang-testing`, `golang-security`, or
`bubbletea-tui` only when that part of the feature genuinely needs the specialized
guidance. Do not load every Go skill pre-emptively.

## Implement and verify

- Prefer stdlib or an existing dependency. Justify a new dependency against the
  single-binary and `CGO_ENABLED=0` release constraints.
- Extend the current registry, dispatcher, runtime, or persistence shape instead of
  adding a parallel one.
- Test the risk introduced: pure unit tests for parsing/policy, fake-provider loop
  tests for agent behavior, headless model tests for TUI state, resume/fork/rewind tests
  for durable state, and `-race` for concurrency.
- Run the narrowest relevant tests while iterating, then `task check`. Run
  `go test -race` on affected concurrency-sensitive packages; run the repository-wide
  race suite only when the change justifies its cost.
- Update `docs/features.md` or `docs/roadmap.md` when the shipped behavior or roadmap
  disposition changes. Documentation churn is not required for an internal change
  already described accurately.

Before finishing, inspect the diff for duplicated mechanisms, unrelated edits,
unbounded output, missing cancellation, capability bypasses, and state that will not
survive resume. Report verification actually observed; never infer success.
