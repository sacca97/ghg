# Harness fork: detach from whip and remove browser/computer capabilities

Branch: `main` (the repository is already pinned to the fork-base commit)
Status: COMPLETE — Phase 0 implemented; the race-suite caveat is recorded
below.

## What this does

Implements Phase 0 of the root [`plan.md`](../../../plan.md): turn the pinned
harness tree into the hard fork's `harness` project, retain the core coding-agent
surfaces, and remove browser/computer capabilities that are explicitly outside
the v1 scope.

## Goal

- Use the repository module path `github.com/sacca97/ghg` and build the CLI from
  `cmd/harness`.
- Use `~/.harness` and `HARNESS_*` for the fork's own persistent and child-process
  configuration surface.
- Preserve the pinned agent, session, MCP, LSP, skills, memory, schedule, todo,
  compaction, and workspace-snapshot behavior.
- Remove browser/computer packages, registrations, tests, drivers, docs, and
  their now-unused dependencies.
- Keep the upstream Apache-2.0 license and add clear source attribution.

## Non-goals

- Provider-neutral backends, provider profiles, artifacts, Anthropic support,
  planner/executor roles, native grep/glob, and project memory are later phases
  in the root plan.
- No compatibility alias for the old `whip` executable or `WHIP_*` environment
  variables; the hard fork owns its new surface.
- No changes to agent behavior beyond removing the out-of-scope capabilities and
  updating names/paths.

## Design and file surfaces

- Module and imports: `go.mod`, all Go imports, and package directories.
- CLI: move `cmd/whip` to `cmd/harness`, remove the browser subcommand, and
  update user-facing command/help/error text.
- Configuration and process markers: `internal/config`, `internal/tools`, and
  `internal/tui` retain only non-browser/computer settings and use `.harness`,
  `HARNESS_*`, and `harness` names.
- Capability removal: delete `internal/browser`, `internal/computer`, the Swift
  `driver`, browser/computer tool/TUI/agent tests, and associated workflow steps.
- Documentation: update README and `docs/` to describe the fork and remaining
  surfaces; remove browser/computer-only documents and stale plan folders.
- Attribution: add `NOTICE` with the upstream URL and pinned SHA; retain
  `LICENSE` unchanged.

## Verification

- `gofmt -w`/`gofmt -s` on changed Go files.
- `go mod tidy` after capability removal.
- `CGO_ENABLED=0 go build -trimpath -o /tmp/harness ./cmd/harness`.
- `go test ./...` and `go test -race ./...`.
- `rg -i 'harness|HARNESS_|browser|computer|chromedp|go-rod|gobwas'` over the
  runtime/source/docs tree; classify only license/attribution or intentional
  historical references.

## Tasks

1. [x] Confirm pinned base, destination module, and removal scope.
2. [x] Rename module, CLI directory, config home, environment markers, and
   user-facing command text.
3. [x] Remove browser/computer implementation, registrations, assets, tests,
   and dependency/workflow references.
4. [x] Update documentation/roadmap and add `NOTICE`.
5. [x] Prune superseded browser/computer plan folders.
6. [x] Run all verification gates and record deviations below.

## Deviations and breadcrumbs

- The repository remote is `origin git@github.com:sacca97/ghg.git`; that is the
  destination module used by this fork.
- The current HEAD is the exact SHA named by the root plan, so no clone/copy is
  needed in this workspace.
- The git index is read-only in this environment, so the CLI move used a
  filesystem rename; Git can recognize the rename when the resulting changes
  are staged in a normal checkout.
- `go mod tidy`, `gofmt -s`, `go vet ./...`, `go test ./...`, the headless
  binary smoke test, and all four `CGO_ENABLED=0` release-target builds pass.
- `go test -race ./...` reaches all packages but currently fails in the
  `internal/tools/TestBashSudoFastFail` environment check and reports races
  around the TUI background-task fixture (`TestScheduleFiresWakeup`,
  `TestCtrlTFocusesDockAndArrowsSelect`, and
  `TestEnterOpensTaskViewAndEscBacksOut`) that shares an Agent across
  goroutines; the Phase 0 diff only renames those tests' text/imports and
  removes browser capability code.
- The `task` executable is not installed here, so the Taskfile's format/vet/test
  checks were run as their equivalent Go commands.

Follow-on implementation work is tracked in
[`provider-substrate`](../provider-substrate/README.md), beginning with the
provider-neutral backend boundary and OpenAI adapter, followed by
[`provider-profiles`](../provider-profiles/README.md).
