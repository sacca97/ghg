# Phase 1 — Compaction and tool-result artifacts

Branch: main
Status: COMPLETE

## What this does

Preserve the evidence behind tool output when the model-facing preview is
bounded, so compaction and resume can keep a small prompt without losing the
ability to inspect the original result.

## Goal

Add one structured tool-result path, a bounded content-addressed artifact
store, session-scoped artifact metadata, and read-only `artifact_list` /
`artifact_read` tools. Keep compaction non-destructive and make disabled or
no-session behavior explicit.

## Provider boundary decision

The existing two-method `llm.Backend` seam, OpenAI adapter, optional catalog
capability, and legacy-compatible profile resolution are sufficient. Phase 1
does not broaden provider-neutral message types, add fallback routing, or
implement another wire protocol. Anthropic-specific request modeling and role
routing remain Phase 2, where fixtures can define the smallest useful shape.

## Non-goals

- Do not change provider retry behavior or add another provider adapter.
- Do not persist unbounded command, file, or MCP output.
- Do not expose filesystem paths as artifact input or allow cross-session reads.
- Do not replace the existing append-only compaction event model.

## Design

- `internal/artifact` owns bounded content-addressed files. It writes with
  0700 directories and 0600 files, hashes the retained payload, and returns a
  reference containing original/stored sizes and whether the whole result was
  retained.
- `internal/tools` keeps the string `Execute` compatibility wrapper but adds a
  structured result path. Built-in tools provide retained output before their
  50 KiB model preview; legacy/custom tools remain valid through the wrapper.
- `internal/agent` records an artifact before `OnToolEnd`, appends a stable
  reference to the model-facing preview, and carries the reference on the
  persisted tool message. A writer is injected per agent; no package-global
  artifact state is used.
- `internal/session` stores artifact references alongside tool-message rows and
  copies/deletes only references with their message sequence. Immutable payload
  files are shared by hash; garbage collection is reference-based.
- `artifact_list` and `artifact_read` validate the current session through the
  session metadata catalog, then read by artifact ID plus bounded offset/limit.
  They never accept a path.
- Compaction continues to operate on the raw log. The summary prompt receives a
  bounded tool ledger with artifact references, and the derived prompt includes
  only the summary, manifest, current state blocks, and safe recent tail.

## Prior art

- `plan.md` Phase 1 defines the structured result, 10 MiB per-result ceiling,
  session metadata, and atomic tool-call grouping.
- `docs/concurrency.md` requires one owner per goroutine and bounded buffers.
- The pinned session implementation already records compaction as summary plus
  cutoff; this phase extends evidence storage without rewriting that log.

## Test plan

- Unit-test deterministic head/tail retention, hashes, file permissions,
  cancellation, offset/limit reads, and disabled/no-session behavior.
- Test structured results for bash, read/search, MCP, and legacy custom tools.
- Test session metadata across save/load, fork, rewind/delete, resume, and
  cross-session rejection.
- Test compaction with artifact references and a large result without orphaned
  tool calls; run focused race tests for parallel tool calls.

## Documentation plan

Update `docs/features.md`, `docs/tools.md`, `docs/roadmap.md`, and
`docs/concurrency.md` with the shipped result/artifact behavior. Keep the
provider decision in `plan.md`; do not add a second provider design document.

## Tasks

- [x] Add bounded artifact storage and references.
- [x] Add structured tool results and agent persistence wiring.
- [x] Add session metadata, fork/delete handling, and artifact access tools.
- [x] Integrate artifact references with compaction and resume.
- [x] Update documentation and run focused/full verification.

## Verification

- Focused artifact, compaction, session, MCP, TUI, and CLI tests pass.
- Repository-wide tests pass with the environment-sensitive
  `TestBashSudoFastFail` excluded; that test can hang when the host exposes a
  controlling terminal despite the runner's no-tty path.
- `go vet`, a CGO-disabled build, module verification, and whitespace checks
  are part of the final Phase 1 handoff.
