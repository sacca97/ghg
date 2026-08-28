# Node-free coding agent — hard fork of whip

## Context

Goal: a Pi/omp-class coding agent that ships as a **single binary with no Node
runtime** in this repository (`github.com/sacca97/ghg`).

Four findings reshape the original sketch:

**1. Eliminating Node is nearly free; matching omp is not.** The agent loop is a
weekend. omp's value is *tool depth* — 31 tools, LSP, DAP, headless Chromium, 60+
providers, memory backends. Notably, omp itself vendored ~80k lines of Rust to do
grep/shell/AST in-process precisely because JS was the wrong place for that work.
Its remaining Node dependency is the thin layer. So the instinct is right, but the
win is *packaging and startup*, not performance — say that honestly up front.

**2. The JS question is already settled by the scope choice.** Of the three options
(drop Pi-extension compat / embed QuickJS / optional Node bridge), take **option 1**.
No JS engine, no Node bridge, ever.

**3. The "language-neutral stdio plugin protocol" already exists — it's MCP.**
A JSON-RPC-over-stdio subprocess contract is literally what MCP stdio servers are.
Do **not** invent a second one. Extensibility boundary = built-in tools + MCP + Markdown skills.
ghg never requires Node; an optional MCP server may require whatever
external runtime that server chooses, just as DAP later relies on external adapters.

**4. whip is already substantial** — the pinned revision has 244 Go files, Apache-2.0,
Go 1.27, and critically **already CGO-free** (`modernc.org/sqlite`,
`chromedp`/`go-rod` are pure Go). It ships: agent loop with parallel tool calls +
per-canonical-path channel-semaphore file locks, proactive & reactive compaction,
background subagents with persisted history across resume, SQLite sessions with fork/rewind/timeline,
MCP (stdio + HTTP, `.mcp.json` + codex config import, `whip mcp test` doctor), LSP
diagnostics wired into `write`/`edit`, Markdown skills (`SKILL.md` scan + `$skill` invoke),
permission prompts with command-prefix arity, secrets-by-reference, bubbletea TUI with
command settings, `whip run` one-shot + `--format json`.

Its `docs/roadmap.md` explicitly tracks parity against pi and opencode. That is our
target's own lineage. **Decision: hard fork whip** — rename the module, never merge
back, full design control. **v1 scope: core + code intelligence.**

**Fork base:** pin `context-labs/whip` commit
`5b8b9d8297184cf69ca34ccd62a4be91457a8bbc` (2026-08-26). Never implement against a
moving `main`. Record the SHA and upstream URL in `UPSTREAM.md`; every item below is
relative to that revision. That revision already contains persisted `todowrite` and
Markdown-backed `remember`/`forget`, so v1 retains them instead of rebuilding them.

## Scope

**In:** `read` `write` `edit` `grep` `glob` `bash` `task`, the existing `todowrite`,
`web_fetch` `web_search`, MCP, Markdown skills, Markdown memory, `AGENTS.md` project
instructions, recoverable tool-result artifacts, LSP, model roles, declarative
provider profiles, profile-driven `/auth` for any provider, declarative agent
definitions, post-edit hooks, and Anthropic Messages API support.

**Not scheduled:** DAP / `debug` — see Phase 4. Designed for now so Phase 3's LSP
work doesn't foreclose it, but gated on daily use rather than on a phase completing.

**Out entirely (for now):** `computer` (desktop control), `browser`,
`ast_grep`/`ast_edit`, `generate_image`, `tts`, `github`, collab relay, 60-provider
catalog, 23 search backends.

`ast_*` is deferred for a concrete reason: go-tree-sitter bindings are CGO, which
would break the `CGO_ENABLED=0` single-binary story that is the whole point of this
project. If AST tools become necessary, the escape hatch is an optional external
`ast-grep` binary, not linking C.

## Phase 0 — Fork and detach

- Clone `https://github.com/context-labs/whip`, checkout the pinned fork-base SHA
  above, copy it into this repo, and record the source in `UPSTREAM.md`. Preserve the
  imported history if convenient, but do not retain a configured `upstream` remote or
  plan future merges; this is a source-auditable hard fork.
- Rewrite module path `github.com/context-labs/whip` → the new path across `go.mod`
  and all Go imports (`go mod edit -module` plus a repository-aware import rewrite;
  `gofmt -r` does not rewrite import strings). Name and document the destination
  module before doing this.
- Rename the binary and the **entire** config/env surface: `cmd/whip` → `cmd/ghg`,
  `~/.whip/` → `~/.ghg/`, and every `WHIP_*` marker → `GHG_*`, including
  `HOME`, `SESSION_ID`, `MODEL`, `PID`, `THEME`, test switches, docs, installer,
  release workflows, messages, and fixtures. Finish with `rg -i 'whip|WHIP_'` and
  classify every intentional remaining attribution reference.
- **Preserve Apache-2.0 attribution**: keep the upstream `LICENSE`, add a `NOTICE`
  crediting context-labs/whip. Non-negotiable.
- Strip browser/computer as complete capabilities, not just packages. Remove:
  `internal/{browser,computer}`, their tool implementations/tests, `cmd/whip/browser.go`,
  agent registration and flags, TUI commands/settings/state/tests, config blocks/env
  handling, embedded driver assets, docs, and release/build references. Then run
  `go mod tidy`; this should drop `chromedp`, `cdproto`, `go-rod`, and `gobwas/ws`.
  Keep `internal/schedule` for now (cheap, and useful later).
- Preserve the pinned revision's `todowrite`, compaction-event storage, workspace
  snapshots, and Markdown `remember`/`forget`. Do not create parallel replacements.
- Gate: `CGO_ENABLED=0 go build ./...` and `go test ./...` green, binary runs a turn.

### Rename harness to ghg ✅

## Phase 0.5 — Make it usable daily ✅

## Phase 1 — Compaction, artifacts, and the missing local tools ✅

## Phase 2 — Anthropic API + model roles

### Anthropic wire adapter ✅

### Generalized `/auth` — any profile, not just OpenRouter ✅

### ✅ Roles, agent definitions, and the planner/executor workflow

- ✅ JSONC roles and routing (`default`, `smart`, `fast`, and `tiny`).
- ✅ Interactive `/plan` → `/execute` workflow with structured validation and todo seeding.
- ✅ Role-aware TUI model, mode, reasoning, and mouse controls.

**Agent definitions are the mechanism; the planner is the first one.**

A planner with a fixed role, a fixed read-only tool set, a fixed prompt and a fixed
round budget is a config file with the config removed. Build the declarative form
instead — it costs perhaps 20% more than the special case, removes a later migration,
and closes the inherited roadmap's open "custom agent definitions" item for free.

1. ✅ Load declarative agent definitions from `.agents/*.md` (trusted project) and
   `~/.ghg/agents/*.md` (user), with frontmatter for `name`, `description`, `role`,
   `tools`, and `max_rounds`, plus the Markdown body as the prompt. Reuse the skills
   scanner's discovery and precedence shape. Unknown tools are load errors.
2. ✅ Run the built-in planner through the same definition mechanism with the `smart`
   role, a bounded round budget, read-only tools (`read`, `grep`, and `glob`; Phase 3
   may add read-only LSP operations), and `submit_plan` as its structured terminal
   tool. Do not grant `bash`, mutation tools, `task`, or arbitrary MCP tools.
3. ✅ Add headless `ghg run --plan` for plan-then-execute and
   `ghg run --plan-only` for plan-and-exit. Keep replanning explicit; do not create an
   autonomous planner/executor loop. The existing TUI `/plan` is already plan-only,
   so do not add a redundant `/plan-only` command.

- `@agent` mentions to target a definition from the prompt are **deferred** — see the
  end of this document. The definitions are the valuable half; the mention syntax is
  surface.

### ✅ Model-call observability

4. ✅ Emit structured `model_call_start` / `model_call_end` events containing role,
   provider instance, model, adapter protocol, latency, finish reason, and usage. Keep
   this independent of agent-definition loading; it verifies routing and makes planner
   and executor calls distinguishable in JSON output.

## Phase 2.5 — Search quality and stateful edits (Phase 3 checkpoint pending)

The bounded search and observation-authorized editing features are implemented. The
stabilization gate below is complete in code and tests; create the reviewable
checkpoint before beginning Phase 3. The implementation remains based on fork
commit `5b8b9d8297184cf69ca34ccd62a4be91457a8bbc`.

### ✅ Confirmed implemented

- ✅ Fork/detach, the `ghg`/`.ghg`/`GHG_*` rename, attribution, browser removal, and the CGO-free build are complete.
- ✅ Provider-neutral turns, compaction, auth, subagents, and all three adapters: `openai-chat-completions`, `anthropic-messages`, and `openai-responses`.
- ✅ Strict declarative provider profiles, ordered routing, profile-driven auth, catalog discovery, models.dev fallback, reasoning controls, and session usage tracking.
- ✅ Native tools, bounded structured results, artifacts, compaction recovery, raw history, fork/rewind cleanup, and the tool ledger.
- ✅ Roles `default`/`smart`/`fast`/`tiny`, explicit `/plan` → `/execute`, and role-aware TUI controls; planning uses `smart`, execution uses `fast`, and compaction/subagents use `tiny`.
- ✅ Declarative agent definitions, the bounded read-only planner, headless `--plan`/`--plan-only`, and route-aware model-call telemetry.
- ✅ Structured GOAL lifecycle with persisted IDs, accounting, checkpoints, request-scoped context, validated `update_goal`, explicit resume, and six lifecycle states.

### ✅ Delivered

- ✅ Search routing and bounded context: native search, stable ranked cursors, complete-entry
  8 KiB pages, artifacts, telemetry, and deterministic pagination tests.
- ✅ Stateful edits: byte-limited complete-line observations, exact-byte relocation, sorted
  atomic multi-file publication, preserved modes/line endings, bounded output, and diagnostics.
- ✅ Verification: `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`,
  changed-file `gopls check`, and `CGO_ENABLED=0 go build` pass.

### ⬜ Phase 3 entry gate

Do not begin Phase 3 until the implementation items and checkpoint below are complete:

1. ✅ **Byte-honest search pagination.** Complete rendered entries determine cursor,
   displayed, and remaining metadata; long-line and ungrouped later-page regressions cover it.
2. ✅ **Route-correct model-call telemetry.** Compaction uses tiny route metadata, and title,
   goal-from-context, and other one-shot calls use the shared wrapper.
3. ✅ **Phase 2.5 acceptance matrix.** Edit operations, conflicts, observations, publication,
   locking, preservation, bounded output, diagnostics, search limits, and task settlement ordering
   have deterministic tests.
4. ✅ **Observation range granularity.** Returned complete lines authorize edits even when the
   read hit its byte ceiling; unchanged targets relocate, while stale/ambiguous targets fail.
5. ⬜ **Reviewable checkpoint commit.** Checkpoint the verified Phase 2/2.5 implementation
   separately before expanding the tool, trust, or network surface.

The code/test work for items 1–4 is complete. Item 5 remains intentionally pending until the
reviewable commit is created; Phase 3 must remain closed in the meantime.

### Phase 3 boundary

Post-write/edit LSP diagnostics, Markdown installation/session memory, Markdown skills,
MCP, trust/permission handling, and durable tool results are foundations to extend.
The safe web tools, LSP navigation and atomic rename, shared `internal/wire` framing,
read-triggered LSP warm-up, and trusted-project memory below remain genuinely
unimplemented Phase 3 work.

## Phase 3 — Network tools, code intelligence, and memory

### Safe web tools

- **`web_fetch`.** Support only `http` and `https`; default-deny loopback, private,
  link-local, multicast, Unix-socket, and cloud-metadata targets. Resolve DNS before
  connecting and revalidate every redirect so a public hostname cannot redirect or
  rebind to a private address. Apply connect/overall timeouts, redirect and body-size
  limits, bounded decompression, content-type allowlisting, cancellation, and shared
  output truncation. Private/LAN access requires an explicit config opt-in and the
  normal permission prompt. Extend the gate with a typed `web_fetch:<host>:<port>`
  rule; do not feed URLs through the shell-command arity parser. Return source URL,
  final URL, status, and media type, and mark the body untrusted through the shared
  `ToolResult` boundary from Phase 1 — do not define a second delimiting scheme here.
- **`web_search`.** Ship one backend: Brave Web Search
  (`GET https://api.search.brave.com/res/v1/web/search`), configured by
  `BRAVE_SEARCH_API_KEY` through the existing secret resolver. Hide the tool with an
  actionable startup note when no key is configured. Put it behind a small search
  interface, but do not build a generic 23-provider search framework. Return bounded,
  normalized title/URL/snippet results and preserve URLs for citation.
- Test fetch against an in-process server, including redirect-to-private, DNS/IP policy,
  oversized/compressed bodies, unsupported schemes/types, timeout, and cancellation.
  Test search with recorded JSON fixtures; keep live calls opt-in.

### LSP navigation and safe rename

- Lift the existing `Content-Length` framing into `internal/wire` first, with no
  behavior change and all current LSP tests green. This creates the later DAP seam.
- Extend the stdlib LSP client with read-only definition, references,
  `documentSymbol`, and hover operations. Expose them through an `lsp` tool and add
  them to the planner's read-only tool set.
- Treat **rename as a separate mutating operation**, not navigation. V1 first requests
  and previews the `WorkspaceEdit`; applying it requires the normal permission gate.
  Support both `changes` and versioned `documentChanges`, UTF-16 position conversion,
  edits across multiple files, and server-requested `workspace/applyEdit`. Reject
  create/rename/delete resource operations in v1 with an actionable message.
- Apply multi-file text edits under canonical-path locks acquired in sorted order;
  validate all document versions and original ranges before writing anything. Stage
  changes in memory, write atomically, and roll back already-written files on failure.
  Run diagnostics after success. Tests cover overlapping edits, stale versions,
  UTF-16, lock ordering, partial write failure, rejection, and preview-only behavior.
- **Read warm-up:** fork a cancellable `touchFile` on `read` so first-edit diagnostics
  are instant. Bound server startup and ensure warm-up failure never fails `read`.

### Memory and skills

- Retain the pinned revision's Markdown `remember`/`forget` implementation and its
  installation/session scopes; do not move memory into SQLite. Markdown is the stable,
  user-editable source of truth and SQLite remains session/event storage.
- Add a project scope at `.ghg/memory.md`, created only when first used in a trusted
  project. Extend the existing tools with `scope: project`, keep the same entry/length
  caps, and inject only open entries. Document that users may commit or ignore the file.
- Add `memory_edit` only if real use shows that remember+forget+direct Markdown editing
  is insufficient. Skip embeddings, `reflect`, `learn`, and `manage_skill` in v1.
- **Skills stay Markdown.** The existing `internal/skills` behavior remains; rename the
  user path to `~/.ghg/skills` and keep project `.agents/skills` compatibility.

### Hooks — closing the edit loop

`write`/`edit` already run LSP diagnostics after writing; formatting is the other half
of the same loop and is missing entirely (no formatter reference anywhere in
`internal/tools/` or `internal/lsp/`). Without it the model spends a `bash gofmt` call
per edit, or writes unformatted code that the next diff makes noisy.

- Add a `"hooks"` block to the JSONC config mapping an event to a shell command:
  `post-edit` first (receives the changed paths), then `post-turn` and `pre-tool` if
  real use asks for them. Resolve and run under the existing bash machinery with a
  bounded timeout.
- **A hook failure never fails the operation it hooks.** A non-zero `post-edit` exit
  surfaces as a transcript note; the edit still stands. Hooks run only in a trusted
  project, and a project-configured hook is a command the trust prompt already covers.
- This does not violate finding #3 (no second plugin protocol). MCP adds *capabilities
  the model calls*; hooks add *policy the ghg enforces regardless of the model*.
  Different axis — a formatter is not something the model should have to remember.

### A spend ceiling that pauses

`/goal` caps rounds (20) but not cost, the scheduler fires unattended turns, and
background subagents run concurrently. Cost is already computed from
provider-advertised pricing and shown in the status line — only the comparison is
missing.

- `maxSpend` per session and per `/goal` loop, configurable, off by default.
- On reaching it, **pause with a resume hint** the way the round cap already does;
  never kill a turn mid-flight and never drop the goal.
- Surface remaining budget in the same status-line slot as session spend, and hide it
  when the provider advertises no pricing (the existing degradation rule).

## Phase 4 (not scheduled) — DAP debugging

**Gate:** revisit after ~30 days of daily use of the ghg, and only with a concrete
list of debug operations actually reached for in that time. Until then this section is
analysis, not scheduled work — the `internal/wire` extraction in Phase 3 is a free
byproduct that keeps the option open, and is the only part of this with a due date.

omp's `debug` tool exposes 28 DAP operations against lldb, dlv, and debugpy. This is
the one "expensive" omp feature worth keeping on the roadmap, because the fork gives
us most of it for free:

**Why it's cheaper here than it looks.** DAP's base protocol uses the *same*
`Content-Length: N\r\n\r\n<json>` framing as LSP. whip's `internal/lsp/client.go`
already implements that framing stdlib-only — `readFrame`, `send`, `writeLoop`,
`readLoop`. Lift those into a shared `internal/wire` package during Phase 3 (a pure
refactor, covered by the existing `internal/lsp` tests), and `internal/dap` starts
with its transport already written and tested.

**What is genuinely new**, and where the work actually is:

- *Message envelope.* DAP is not JSON-RPC. Messages carry `seq` and
  `type: request | response | event`; responses reference `request_seq` and carry
  `success`. So the dispatch table in `readLoop` is new even though the framing isn't.
- *Event-driven, not request/response.* The interesting state arrives as **events**
  (`stopped`, `terminated`, `output`, `breakpoint`). A tool call like "step over"
  returns almost nothing useful — the answer comes on the next `stopped` event. The
  client needs an event bus the tool layer can await on.
- *Session lifetime outlives a turn.* Unlike every current tool, a debug session is
  long-lived, stateful, and cancellable. Reuse the lifecycle pattern in
  `internal/agent/background.go`: a registry keyed by id, cancellation, status/listing,
  TUI callbacks, and a terminal `Done` broadcast. Add a separate multi-event stream for
  repeated `stopped`/`output` events. A live adapter process is **same-process only** in
  v1: after `--resume`, restore the record as interrupted history. Relaunch/re-attach
  semantics are a later feature, not something the background-task registry provides.
- *Launch configuration is the real hard part*, not the protocol. Something must know
  how to start the debuggee. Read `.vscode/launch.json` when present and fall back to a
  `debug` block in the existing JSONC config. Validate and preview the resolved command,
  working directory, environment, and adapter before launch.

**Scope when it lands:** one adapter first (`dlv dap` for Go — the ghg is Go, so
it dogfoods itself), then `debugpy`, then `lldb-dap`. Operations in dependency order:
`launch`/`attach` → `setBreakpoints` → `continue`/`next`/`stepIn`/`stepOut` →
`stackTrace`/`scopes`/`variables` → `evaluate`. That subset is most of the daily value;
the remaining ~15 ops are long-tail.

**Adapters stay external binaries** (`dlv`, `debugpy`, `lldb-dap`) discovered on PATH.
They are not linked in, so the `CGO_ENABLED=0` single-binary property is unaffected —
the ghg degrades to "no debugger available" when they're absent.

## Post-Phase 4 (not scheduled) — Native indexed and structural search

Do not add CGO, FFF, tree-sitter, or AST tools during Phases 2–4. First implement and
measure the native-Go search-quality slice in Phase 2.5: bounded grouped results,
ranking, stable cursor pagination, artifact recovery, prompt/tool routing, and strict
context budgets. Revisit a native dependency only if repository-scale benchmarks show
that traversal/indexing or structural queries remain a material bottleneck after those
changes.

If CGO becomes an acceptable build and distribution tradeoff, the preferred design is
not FFF *or* AST; they serve separate purposes:

- Integrate FFF first as a thin backend for indexed path and text discovery. Reuse its
  frecency, git-aware ranking, watcher, grouped matches, pagination, and multi-pattern
  search rather than reimplementing its Rust internals. Keep the Go-facing search
  interface, tool schemas, permissions, cursor/artifact policy, and result formatting
  independent of the backend.
- Add `ast-grep` later as a separate structural engine for syntax-aware search and
  previewed rewrites. Do not build directly on raw tree-sitter unless a narrow use case
  cannot be served by an existing library. AST matching does not replace LSP for
  type-aware definitions, references, and rename.
- If both engines land, expose them through one small, versioned Rust C ABI (for
  example, an internal `ghg-native` library) instead of maintaining two unrelated CGO
  layers. Keep text/path and structural operations as distinct Go services and tools.
  The boundary must define ownership/free functions, contain Rust panics, support
  cancellation, and avoid callbacks into Go from native watcher threads.
- Preserve preview-first, permission-gated, atomic application for structural rewrites,
  followed by diagnostics and relevant tests. Start AST support read-only with
  `ast_search` or outline operations before permitting mutation.

Before committing, spike lifecycle, concurrency, cancellation, workspace switching,
startup time, memory, and the noisy-`TODO` evaluation. Prove the intended release
matrix too. FFF currently exposes a C ABI through a dynamic Rust library; retaining a
single executable would likely require a pinned patch/fork that also builds a static
archive, plus a Rust/C linker toolchain for every target. If that packaging and crash
surface is not justified by measured gains, keep the Go implementation or use an
optional sidecar for stronger failure isolation.

## Deferred, cut, and scoped down

Every open item inherited from [docs/roadmap.md](docs/roadmap.md) is triaged here so
none of it gets re-decided. `docs/roadmap.md` points at this section rather than
carrying a second, drifting copy.

**Deferred — real, discretionary, not scheduled.** Revisit only when daily use makes
one of them the obvious next annoyance:

- Queue management: edit/remove queued messages before they send.
- External editor for long prompts (`$VISUAL || $EDITOR`). Cheap: `/me`
  (`internal/tui/registry.go:37`) already has the suspend → edit → resume plumbing.
- Desktop notification/sound when a turn finishes and the terminal is blurred.
- `@` mention fuzzy picker + frecency ranking.
- `@file.go#N` symbol-range expansion via `documentSymbol` — Phase 3 adds the LSP call
  anyway, so this is the consumer that would justify it.
- `@agent` mentions targeting a Phase 2 agent definition.
- Provider failover at the Phase 1 factory seam (the seam is designed for it now).
- MCP `ToolListChanged` → live re-list; MCP resources/prompts (synthetic
  `read_mcp_resource` tool + prompts-as-slash-commands); LSP pull diagnostics
  (`textDocument/diagnostic`) for servers without push.
- Render tool calls as they stream, before execution starts.

**Cut.** Nothing downstream depends on these and each is pure surface area:
toast notifications, transcript export with an options dialog, the generic
fuzzy-select widget shared by every picker, JSON themes / a "system" theme built from
the terminal settings, the KV table for settings-toggleable UI prefs, MCP overlay config
entries.

**Scoped down.** MCP OAuth for remote servers → ship a `needs_auth` status only.
Upstream's own estimate is that this carries most of the value for a fraction of the
~800 lines; the full in-memory-buffer-then-commit credential flow is not worth it
before there is a remote server anyone here actually uses.

**Superseded.** Upstream's bash output spill (post-pin) → Phase 1 artifacts.

## Critical files

| Concern | Path |
|---|---|
| Agent loop, compaction, subagents | `internal/agent/{agent,task,background,filelocks}.go` |
| Existing todo + Markdown memory integration | `internal/agent/{todo,memory}.go`, `internal/memory/` |
| Tool registry + permission gate | `internal/tools/tools.go`, `permission.go` |
| Read observations + range-authorized unified edits | `internal/tools/`, `internal/artifact/`, `internal/session/`, `internal/agent/filelocks.go` |
| System-prompt assembly (`me.md`, `AGENTS.md`, block order) | `cmd/ghg/main.go`, `internal/agent/agent.go` |
| Agent definitions (`.agents/*.md`) — reuse the skills scanner | `internal/skills/`, `internal/agent/task.go` |
| Post-edit hooks + spend ceiling | `internal/config/`, `internal/tools/`, `statusView` in `internal/tui/tui.go` |
| Streamed tool output (`OnUpdate` port) | `internal/tools/bashrun/`, `internal/agent/agent.go`, `internal/tui/tui.go` |
| Tool-result artifacts + compaction view | `internal/artifact/`, `internal/agent/agent.go`, `internal/session/` |
| Neutral LLM types + wire adapters | `internal/llm/{backend,openai,anthropic}.go` |
| Provider profile parser/factory | `internal/provider/`, `internal/provider/profiles/*.yaml` |
| Config instances, roles, secrets | `internal/config/`, `secret.go` |
| `/auth` + `ghg auth` (generalize off the profile set) | `internal/tui/auth_cmd.go`, `cmd/ghg/auth.go`, `internal/config/provider_key.go` (the former OpenRouter-only file was removed) |
| Profile-driven catalog fetch (the pattern `/auth` should copy) | `fetchCatalogs` in `internal/tui/tui.go:543` |
| Native grep/glob + web tools | `internal/tools/` |
| Session store (SQLite) | `internal/session/` |
| LSP client (+ framing to extract for DAP) | `internal/lsp/client.go`, `manager.go`, `diagnostic.go` |
| Shared LSP/DAP framing | `internal/wire/` |
| Long-lived lifecycle pattern (reuse for DAP) | `internal/agent/background.go` |
| MCP client | `internal/mcp/` |
| Skills | `internal/skills/` |
| Upstream feature map (read first) | `docs/features.md`, `docs/roadmap.md` |

`docs/features.md` maps every shipped behavior to its code *and its tests* — read it
before touching anything, it is the fastest way into the codebase.

## Verification

1. **Fork/rename audit:** the recorded upstream SHA matches the imported tree;
   `rg -i 'whip|WHIP_'` contains only attribution/migration references reviewed by a
   human; no browser/computer registrations, assets, config, or dependencies remain.
2. **Single binary:** `CGO_ENABLED=0 go build -trimpath -o ghg ./cmd/ghg`
   succeeds. `ldd`/`otool -L` shows only expected platform linkage. Cross-build the
   intended Linux/macOS amd64/arm64 release matrix, and smoke-test an artifact in an
   environment with no Node executable installed.
3. **Regression:** `go test ./...` stays green throughout the fork. Run `go test -race`
   on `internal/agent`, `internal/mcp`, `internal/lsp`, and `internal/wire` for the
   concurrency-sensitive changes.
4. **Phase 0.5:** an `AGENTS.md` in a trusted project shows up in `/context-doctor`'s
   injection audit and does not in an untrusted one; a long `bash` command shows a live
   output tail that collapses on completion, with `-race` on `internal/agent` clean
   while several parallel `bash` calls stream at once; `--continue` lands in the most
   recent session for the working directory and gives a clear note when there is none.
5. **Profiles:** table tests cover embedded/user/project precedence, strict YAML,
   unknown schema/protocol, secret non-leakage, legacy JSONC normalization, catalog-less
   local providers, HTTPS policy, and factory selection. Instantiate two different
   OpenAI-compatible providers from YAML without adding Go code.
6. **Prompt-cache ordering:** assert on the assembled request that every mutable
   per-round block (todo, goal, memory, artifact manifest) follows the stable prefix,
   and that mutating one leaves the prefix byte-identical — before and after a
   compaction.
7. **Compaction/artifacts:** fake tools return small, oversized, parallel, and
   over-hard-limit results. Compact immediately afterward and prove the prompt has no
   orphaned tool IDs, artifact refs survive resume/fork, current-session list/read can
   recover retained chunks, disabled persistence is explicit, and cleanup never removes
   a still-referenced payload. Output from `web_fetch`, an MCP server, and `read` all
   carry the same untrusted marking.
8. **Read/edit observations:** partial reads issue only complete visible lines, a bounded
   continuation, and a session/path-bound observation ID that survives compaction and
   resume. Tests prove same-position edits, exact unique relocation after preceding
   insertions, insert/replace/delete, disjoint batches, sorted locks, CRLF/final-newline
   preservation, and compact diffs. Changed interiors, duplicate candidates, unseen or
   truncated ranges, overlaps, stale observations, cross-session reuse, permission
   failure, and injected partial-write failures must leave files unchanged or report
   partial publication and recovery precisely. Compare observation ranges with and
   without internal keyed BLAKE3; visible per-line tags ship only if model evals beat the
   lower-token observation-ID form.
9. **Roles/agents/planning:** role-routing tests cover `default`, `smart`, `fast`, and
   `tiny`, including acting→`fast`, planning→`smart`, compaction/task→`tiny`, fallback,
   and invalid-role behavior. The TUI `/plan` → `/execute` path now validates and hands
   off a plan through `smart` then `fast`. The declarative and CLI path is covered by
   fake OpenAI-compatible servers backing `smart`, `fast`, and `tiny`:
   `--plan-only` never calls `fast`, and `--plan` hands the accepted plan to `fast`.
   A user-authored `.agents/*.md` definition loads through the same path as the
   built-in planner, an unknown tool in its allowlist is a load error, and project
   definitions are skipped in an untrusted project.
10. **Model-call observability:** `ghg run --plan ... --format json` emits ordered
   `model_call_start`/`model_call_end` pairs with the expected role, provider, model,
   protocol, latency, finish reason, usage, and no duplicated accounting.
11. **Anthropic:** fixture tests cover streaming text/thinking/tools/cache/error cases.
   An opt-in live test runs the same tool prompt against one OpenAI-compatible model and
   one Anthropic model and confirms valid tool history plus non-zero usage.
12. **Generalized `/auth`:** against a fake OpenAI-compatible server, `/auth <id>` and
   `ghg auth <id>` validate, upsert, and seed the catalog for a profile that is not
   openrouter — with **no Go change**, only a YAML file. An unknown id lists the
   available ids. A `catalog.kind: none` profile still authenticates via the probe path;
   a provider that can neither list nor probe stores only after confirmation and is
   reported as unvalidated. Env mode records the profile's `auth.env_var` and errors when
   the profile declares none. Assert the key appears in neither the transcript, the
   event log, nor any error string, and that re-running the other mode leaves exactly one
   of `apiKey`/`apiKeyEnv` set. **A profile whose catalog is `public: true` must reject
   a garbage key** — the regression this guards is a fake server whose `/models` answers
   200 without a credential, where a catalog-only check reports success — and the probe
   must use a **real** model id, since an unknown one 401s regardless of the key.
   **Per-model routing:** a fake server exposing `/chat/completions` and `/messages` on
   one base URL proves a **single** `opencode` entry and a **single** `/auth` run
   reach both — a route-matched model sends `x-api-key` to `/messages`, an unmatched one
   sends `Authorization: Bearer` to `/chat/completions`, first-match-wins holds with
   deliberately overlapping globs, a model-level `api` override beats the route table,
   and a route naming a protocol with no adapter fails by name at selection time instead
   of as a wire error. A route that tries to override `base_url`, `env_var`, `docs`, or
   `catalog` is a load error.
   **Cold start:** with `GHG_HOME` pointed at an empty directory the TUI opens
   instead of exiting 1, names what is missing, keeps `/help`, `/model`, `/auth` and the
   settings usable, appends the actionable note (not a panic) when a turn is submitted
   with no agent, and reaches a working session via `/auth` with no restart. A key that
   resolves to an *error* — unset `$VAR`, failing `!cmd` — still fails hard and names
   the cause, and `ghg run` still fails fast.
13. **Web:** local adversarial tests prove SSRF/redirect/size/timeout/content-type policy;
   recorded Brave fixtures prove stable normalized output. A live Brave call is opt-in.
14. **LSP:** fake-server tests cover navigation, read warm-up, UTF-16 rename preview,
   gated multi-file apply, stale versions, rollback, and post-write diagnostics.
15. **Hooks/budget:** a `post-edit` hook runs with the changed paths and reformats the
   file; a hook that exits non-zero or hangs past its timeout leaves the edit standing
   and surfaces a note; hooks do not run in an untrusted project. A `maxSpend` reached
   mid-`/goal` pauses with a resume hint instead of killing the turn or dropping the goal.
16. **CLI/MCP:** `./ghg run "read README.md and summarize it" --format json` works,
   and `./ghg mcp test <server>` proves the MCP path survived the rename.
17. **Dogfood:** point the binary at this repo and have `--plan` make and verify a real
    multi-file change end to end; inspect the emitted role/provider trace and workspace
    diff before calling v1 usable.

## Honest expectations

Phase 0 is mechanical but broad. Phase 0.5 is an evening, and it is first on purpose:
the rest of this document is months of work, and a plan whose later gates depend on
"are you using it daily?" has to make daily use possible before it collects the
answer. Phase 1–2 is several weeks of evenings because the provider-neutral message
model and Anthropic history conversion are correctness work, not wrappers. At the end
of Phase 2 the ghg should already be genuinely useful and new OpenAI-compatible
providers should be YAML-only additions. Phase 3 is another chunk, and lands roughly
at omp's daily 80% — without `browser`, `computer`, or the 60-provider catalog.

Phase 4 (DAP) is deliberately *after* that line, not on it — and now explicitly
unscheduled rather than "post-v1", which was a due date in disguise. It is the single
largest remaining piece and the one most likely to eat a month if started early: the
protocol is a week, the event model and launch configuration are the rest. It stays in
this document rather than being cut because Phase 3's framing extraction makes it
tractable later. Do not start it until you are using the ghg daily and can say
concretely which debug operations you actually reach for.

The additions in Phases 0.5–3 are deliberately small and mostly seam-locked: block
ordering, the untrusted-content boundary, model-call telemetry, and agent definitions
are each near-free at the moment the surrounding type or schema is written, and each is
a cross-cutting retrofit afterwards. They are in the plan for their *timing*, not their
size. Everything else from the inherited roadmap is triaged above and should not be
reopened item by item.

Full omp parity is not a solo project, and pretending otherwise is how this stalls at 40%.
