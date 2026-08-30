# ghg roadmap

**[plan.md](../plan.md) is the plan. This file is the shipped-feature checklist.**
It records what the fork has, inherited from upstream whip's parity tracking against
[pi](file:///home/abe/code/pi) and
[opencode](file:///home/abe/code/coding-harnesses/opencode). Every unshipped item
below carries its disposition — the phase in [plan.md](../plan.md) that owns it, or
`deferred` / `cut` / `scoped down` from that document's
[triage section](../plan.md#deferred-cut-and-scoped-down). Nothing here is an open
question; if you want to reopen one, do it in `plan.md`, not by editing a checkbox.

Full exploration reports: [learnings/other-harnesses/opencode/](learnings/other-harnesses/opencode/),
[learnings/other-harnesses/exo.md](learnings/other-harnesses/exo.md) (durable state, self-modification, scheduler/adapters).

**Reference docs:** [features.md](features.md) (what's shipped, where it lives,
its tests) and [concurrency.md](concurrency.md) (the channel patterns behind
parallel tool calls and background subagents).

**Post-pin upstream drift:** upstream shipped two agent-loop items after the pinned
SHA in [UPSTREAM.md](../UPSTREAM.md). Streamed partial tool output shipped in Phase
0.5; bash output spill is superseded by Phase 1's artifact store and will not be
ported. This fork does not merge from upstream — see `UPSTREAM.md`.

## Fork baseline

- [x] Phase 0 detachment: the pinned upstream revision is recorded in
  [UPSTREAM.md](../UPSTREAM.md), the module/CLI/config surface is
  `github.com/sacca97/ghg`/`ghg`,
  and browser/computer automation plus its Swift driver are outside this fork's
  initial scope. See the [living implementation plan](../plan.md).

## Table of contents

- [Fork baseline](#fork-baseline)
- [Input & editing](#input--editing)
- [Transcript & rendering](#transcript--rendering)
- [Sessions](#sessions)
- [Agent loop](#agent-loop)
- [Skills & subagents](#skills--subagents)
- [Models & providers](#models--providers)
- [MCP](#mcp)
- [LSP](#lsp)
- [Safety & permissions](#safety--permissions)
- [Theming & config](#theming--config)
- [CLI surface](#cli-surface)
- [Autonomy & durability](#autonomy--durability) (exo)

## Input & editing

- [x] Queue messages while busy (enter, codex-style multiple), force-steer queue into the running turn (empty enter, grok-style), auto-send queued as follow-up turns
- [x] Explicit interruption: double ctrl+c while busy (cf. opencode's triple-escape with 5s reset — `packages/tui/src/routes/session/index.tsx:1388`)
- [ ] Queue management: edit/remove queued messages before they send (opencode `<leader>q`, `runtime.queue.ts`) — **deferred**
- [x] Multiline input (grow textarea; opencode binds newline to `shift+enter,ctrl+enter,alt+enter,ctrl+j` because terminals disagree — `keybind.ts:161`)
- [x] `!` prefix shell escape: output lands in transcript (tool-style block) and in the conversation as a non-authored `$ <cmd>` user message the model sees next turn (opencode `prompt/index.tsx:815`, `:1059`). Shipped as a submit-time prefix, not a mode; mode chrome and a real tool-role result remain deliberately out of scope.
- [x] `@` file mentions, pointer-style: tag any file, any path (relative/absolute/`~`), `@file#10-40` line ranges, tab-completion — a pointer note is appended to the user message, contents never inlined; the model probes with its own tools (Abe's design; alternative documented in [learnings/other-harnesses/opencode/at-mentions.md](learnings/other-harnesses/opencode/at-mentions.md))
- [ ] `@` mention fuzzy picker + frecency ranking (opencode `prompt/frecency.tsx`, `prompt/autocomplete.tsx`) — **deferred**
- [ ] External editor for long prompts: `$VISUAL || $EDITOR`, suspend renderer → edit temp .md → resume (opencode `editor.ts:26-53`; pi setting `externalEditor`) — **deferred** — `/me` already has the suspend/resume plumbing
- [x] Paste handling: collapse big pastes (≥3 lines) into a `[Pasted ~N lines]` placeholder expanded on submit (opencode `prompt/index.tsx:1149`) — opt-in via config `collapsePaste`, OFF by default (a paste you can't see is a paste you can't trust)
- [x] Persist prompt input history to disk, restore across sessions; up/down only navigate history when cursor is at offset 0 (opencode `prompt/history.tsx`)

## Transcript & rendering

- [x] Markdown rendering for assistant messages (glamour, hardcoded dark style — no OSC background query; finalized segments + resumed transcripts render rich, in-flight streaming stays plain text; right-padding stripped, body aligned under the "● " marker)
- [x] Diff view for `edit` tool results (pi edit tool returns `details: {diff, patch, firstChangedLine}` — `packages/agent/src/harness/tools/edit.ts`; opencode picks split vs unified by terminal width >120)
- [x] Tool rows: icon + present-participle verb while running ("Reading file…"), collapse to one line on completion, red + expandable on failure (opencode `routes/session/index.tsx:1836`, `util/collapse-tool-output.ts` — 19 lines)
- [ ] Render tool calls as they stream, before execution starts (pi: `message_update` spawns `ToolExecutionComponent` keyed by tool-call id) — **deferred**
- [x] Spinner with elapsed time + token count in the bottom status box (opencode `routes/session/footer.tsx`) — input/output use `↓`/`↑`; cost is shown when the provider advertises pricing; the box is bordered with separators between fields and transcript scrolling leaves the prompt/footer fixed
- [ ] Toast-style transient notifications for command success/failure (opencode `ui/toast.tsx` — 102 lines) — **cut**
- [ ] Desktop notification/sound when a turn finishes and the terminal is blurred (opencode `attention.ts` — "when: blurred" is the detail that makes it not-annoying) — **deferred**

## Sessions

- [x] SQLite session store with `--resume` / `/resume` picker
- [x] `--continue` resumes the newest session with persisted messages for the current working directory without opening the picker — `session.Store.MostRecentForCWD`, with a clear error when none exists (plan.md Phase 0.5)
- [x] Session titles: auto-generate a short title from the first exchange
- [x] `/rename` a session (opencode: ctrl+r prompt dialog) — `/rename [title]`, bare opens an inline prompt prefilled with the current title, draft preserved
- [x] `/fork` a session (pi: tree-structured JSONL entries with `parentId` — `docs/session-format.md`; opencode forks from any message via a per-message action menu) — `/fork [name]` copies the conversation to a new session with an auto-suggested `(fork #N)` name; `f` in the rewind picker forks from any message
- [x] Timeline: jump-to-message picker that live-scrolls the transcript as you browse (opencode `dialog-timeline.tsx`) — the rewind picker (idle esc esc) does this and rewinds/forwards too
- [x] Undo last message (conversation half): rewind restores the prompt text into the input for editing (opencode `routes/session/index.tsx:615`); file-change revert (opencode `revert.ts` git snapshots) is NOT done — conversation-only by design
- [x] Compaction: summarize old turns when context fills (pi settings: `compaction: {reserveTokens, keepRecentTokens}`; opencode `/compact`) — `/compact` manually; auto-compacts proactively at a configurable % of the provider-advertised context_length (GET /models, cached in ~/.ghg/models.json; missing context falls back to the daily models.dev cache in ~/.ghg/models-dev.json, fetched lazily only for listed models that still lack a context window; default 50%, `compactPct`, slidable ←/→ in the ctrl+p settings) plus retries once when the provider errors with context_length_exceeded; the configured `tiny` role supplies the summarizer, with `/compact <model> [provider]` as a per-session override (legacy configs retain the built-in fallback); kept tail never orphans a tool_call from its result; raw history is retained behind a recorded event and artifact references survive the prompt fold
- [x] Token/cost tracking per session (pi models.json carries `cost: {input, output, cacheRead, cacheWrite}`) — latest context usage/window in the bottom status box; cost computed from provider-advertised `pricing` in GET /models (cached in ~/.ghg/models.json), cached input billed at the cache-read rate; hidden when the provider doesn't advertise prices
- [ ] Export transcript to markdown with include-options dialog (opencode `/export`, `ui/dialog-export-options.tsx`) — **cut**

## Agent loop

- [x] Structured `/goal <text>` lifecycle: a persisted goal ID tracks status, rounds,
  provider accounting, progress, and blockers; the request-local `update_goal` tool
  records verified completion or genuine blocking; restart pauses active work until
  `/goal resume`, `/goal clear` records an explicit drop, and the round cap is only a
  circuit breaker (`active`, `paused`, `blocked`, `usage-limited`, `budget-limited`,
  `complete`)

- [x] Parallel tool-call execution with per-path file mutation lock (pi: `withFileMutationQueue`, `executeToolCallsParallel`) — `agent.runTools` fans a tool-call batch out to goroutines; same-file mutations serialize, multi-file edits lock sorted canonical paths, bash takes the global lock; results land in call order
- [x] Retry with backoff on provider errors (pi settings: `retry: {maxRetries, baseDelayMs}`) — transient failures (429/5xx/transport) retry with exponential backoff (1s→2s→4s… capped 20s, jittered), configurable via `maxRetries` (default 8, 1 disables); streaming retries stop once visible text has been emitted so the transcript never double-prints, and context-limit errors pass straight through to the compaction retry
- [x] Native `grep`, `glob`, and `find_files` — bounded grouped/OR text search, exact and fuzzy path search, stable cursors, byte-honest 8 KiB pages whose displayed/remaining metadata matches rendered entries, ranking, per-file caps, deterministic ordering, binary/symlink policy, cancellation, and nested `.gitignore` matching
- [x] Stateful observed edits — bounded read observations authorize explicit range operations, including complete lines returned before a byte ceiling; same-session exact-byte relocation, sorted multi-file locks, permission-first atomic publication, mode/line-ending preservation, compact diff/readback, diagnostics, and session persistence
- [x] Tool-output telemetry and exploration redirects — per-tool preview/retained/original byte accounting, truncation metadata, route-correct model-call telemetry (including tiny compaction/title/goal calls), and conservative non-executing redirects for simple recursive inspection commands
- [x] Streamed partial tool output — per-call context callback, 100ms accumulated snapshots, tool-id events, and a last-three-lines TUI tail that collapses on completion (plan.md Phase 0.5)
- [ ] Spill truncated bash output to a temp file and mention the path (pi bash tool) — **superseded** by plan.md Phase 1 artifacts — do not port
- [x] Recoverable tool-result artifacts — structured bounded results, deterministic head/tail retention up to 10 MiB, SHA-256 content-addressed payloads, session-scoped `artifact_list`/`artifact_read`, fork/rewind-safe metadata, explicit opt-out, no-session cleanup, untrusted-output delimiters, and `ghg artifacts gc` — plan.md Phase 1
- [x] Inject `GHG_SESSION_ID` / `GHG_MODEL` env into bash children (pi injects `PI_*`) — shipped: `bashrun.SetMarkers` stamps `GHG=1`, `GHG_SESSION_ID`, `GHG_MODEL`, `GHG_PID` on every child env (`internal/tools/bashrun/markers.go`, wired from `tui.go` on session create/resume); the checkbox was stale

## Skills & subagents

- [x] Trusted project instructions from `AGENTS.md` — bounded, symlink-rejecting load after the folder trust gate, injected beside `~/.ghg/me.md` (plan.md Phase 0.5)
- [x] Skills: scan `.agents/skills/*/SKILL.md` (project) and `~/.ghg/skills/` (user), inject name+description into the system prompt as an `<available_skills>` block; the model reads a SKILL.md with its own read tool when relevant (pi's approach — no skill tool needed, `packages/coding-agent/src/core/skills.ts`)
- [x] Subagents: a `task` tool that runs a self-contained prompt in a fresh `tiny`-role agent with the same tools (minus `task` — no recursion) and returns its final report; role-less legacy configs clone the parent route
- [x] `$skill-name` invocation (codex-style) with live completion dropdown; skills re-indexed every turn and every `$` keystroke, so new skills load without restarting the ghg
- [x] Custom agent definitions (`.agents/*.md` and `~/.ghg/agents/*.md` with strict `name`, `description`, `role`, `tools`, `max_rounds` frontmatter and Markdown prompts) — project-over-user precedence, unknown-tool load errors, and the reserved built-in planner; `/plan`, `ghg run --plan`, and `ghg run --plan-only` use the same bounded read-only planner runner — **plan.md Phase 2**
- [x] Parallel/background subagents (pi streams tool `onUpdate`; opencode `background-job.ts`) — `task` with `background:true` runs concurrently and reports back via a steered message; a `taskRegistry` keyed by id holds a `Done` channel whose single close follows the final `OnChange`/`OnRecord` callbacks and broadcasts persisted completion to every waiter; `/tasks` lists them and updates live via `OnChange`; tasks persist in the session store and are restored on `--resume` (a stale "running" row comes back as interrupted-error)
- [ ] `@agent` mentions to target a named subagent (opencode autocomplete) — **deferred**

## Models & providers

- [x] Model → provider routing in config (switch providers without touching models)
- [x] Provider-neutral backend boundary with compiled OpenAI-compatible Chat Completions, OpenAI Responses, and Anthropic adapters plus optional model catalog capability
- [x] Declarative YAML provider profiles with embedded/user/trusted-project precedence, strict validation, and anonymous legacy compatibility
- [x] Profile-driven `/auth` and `ghg auth` for every loaded provider, with masked TUI input, validation-before-save, catalog seeding/probes, YAML-only custom profiles, a single-profile ordered route table for multi-protocol providers, and a degraded cold start that promotes the acting `fast` role when available (otherwise the first catalog model) in place — **plan.md Phase 2**
- [x] `anthropic-messages` API style alongside `openai-completions` (pi: `packages/ai/src/api/`) — native Messages adapter with tools, vision, thinking, cache usage, retries, and model discovery; **plan.md Phase 2**
- [x] `openai-responses` API style — native `/responses` adapter with flattened function tools, streamed text/reasoning/tool-call events, preserved output-item history, usage, retries, probing, and model discovery; **plan.md Phase 2**
- [x] Model roles: JSONC `roles` accepts only `default`, `smart`, `fast`, and `tiny`; acting defaults to `fast`, planning defaults to `smart`, compaction and delegated tasks select `tiny`, and each role resolves through the profile factory with default/legacy fallback. The TUI exposes cycling bottom-bar `execute`/`plan` mode and model controls plus a role-first, API-key-filtered model selector (`plan` maps to `smart`) — **plan.md Phase 2**
- [x] Context-window metadata fallback: when a provider catalog omits `context_length`, lazily fetch the matching `limit.context` from the daily models.dev provider/model snapshot; only listed model IDs are retained locally, and profile aliases cover gateways whose runtime ID differs from its public metadata ID
- [x] `"$VAR"` / `"!cmd"` resolution for apiKey/header values in config (pi models.json value resolution) — shipped with secrets-by-reference (internal/config/secret.go), resolved at point of use
- [x] Reasoning effort: `/effort` (bare opens the selector), tab-completes, and the clickable `(effort)` control use the selected model's models.dev/provider-advertised options, including `max`, toggle-only `off`/`on`, and explicit off-only models; graded values and adapter-supported toggle state are sent per request, inherited by subagents, and survive model switches
- [ ] Per-model sampling params in config (`samplingParams: {temperature, top_p}`) — **plan.md Phase 2** — added with roles and the provider-specific request shape

## MCP

Improvement plan with per-item checkboxes: [`.ai-docs/plans/mcp-polish/`](../.ai-docs/plans/mcp-polish/README.md).

- [x] MCP client: stdio + streamable HTTP servers; config merges claude-style `.mcp.json` and codex-style `~/.codex/config.toml [mcp_servers]` under ghg's own `"mcp"` block (opencode's status model `mcp/index.ts:83-106`, name sanitization + tool bridging `mcp/catalog.ts:47-90,117-119` — with the sanitize-collision fixed via hashed server keys; claude-code's `mcp__server__tool` naming kept). Lazy-with-kickoff connects (close-to-broadcast `ready` chan), per-server call serialization, 30s startup / 60s call timeouts, errors as tool output, `/mcp` status + reconnect/enable/disable, `ghg mcp add|list|remove|serve`
- [ ] MCP resources/prompts (opencode: synthetic `read_mcp_resource` tools + prompts-as-slash-commands) — **deferred**
- [ ] MCP OAuth for remote servers (opencode `oauth-provider.ts` — buffer creds in memory, commit on success; ~800 lines, a `needs_auth` status covers most of the value first) — **scoped down** — ship a `needs_auth` status only
- [ ] `ToolListChanged` notification → live re-list (opencode `mcp/index.ts:462-471`; needs the standalone SSE stream on remote transports) — **deferred**
- [x] Fail-fast MCP calls (connecting server can't park a turn) + did-you-mean on unknown mcp__ tools + first-settle transcript note — the "never stuck, always know why" pass
- [x] Auto-reconnect with backoff on dropped sessions (gen-guard makes it safe; manual `/mcp reconnect` stays as override)
- [x] MCP server instructions injected into the system prompt (opencode `session/system.ts:119-135`)
- [x] `ghg mcp test <name>` (the doctor: connect + list + timing + stderr tail, non-zero exit on failure — CI-checkable `.mcp.json`)
- [x] `ghg mcp import [--dry-run]` (materialize claude/codex imports into ghg's config)
- [x] MCP import source gating: `"mcpImport"` block (`enabled`/`only`/`exclude` per claude/codex source); blocked imports stay visible in `/mcp` and `ghg mcp list` instead of vanishing — stops third-party codex-config entries (e.g. ChatGPT app's `node_repl`) from being picked up wholesale
- [ ] Overlay config entries (`"overlay": true` patches `enabled` over imports instead of copying definitions) — **cut**

## LSP

- [x] LSP diagnostics in `write`/`edit` tool output — stdlib-only client (`internal/lsp/`), gopls built-in + user servers via the `"lsp"` config block, capped 1.5s wait, sibling-file errors included (opencode `src/lsp/` diagnostics flow, research in `docs/learnings/other-harnesses/opencode/lsp.md`); plan: [`.ai-docs/plans/lsp-diagnostics/`](../.ai-docs/plans/lsp-diagnostics/README.md) (Linear INF-4989)
- [ ] `@file.go#N` symbol-range expansion via `documentSymbol` (Linear INF-4991; deferred from the at-mentions port — see `docs/learnings/other-harnesses/opencode/at-mentions.md`) — **deferred** — plan.md Phase 3 adds `documentSymbol` anyway
- [ ] Read warm-up (forked `touchFile` on read so first-edit diagnostics are instant — opencode `tool/read.ts:119`) — **plan.md Phase 3**
- [ ] Pull diagnostics (`textDocument/diagnostic`) for servers without push — **deferred**
- [ ] Navigation tool (definition/references/symbols) if cross-file diagnostics prove insufficient — **plan.md Phase 3** — scheduled outright, not conditional

## Safety & permissions

- [x] Permission prompt: Allow once / Allow always / Reject, where "always" previews the exact rule it installs and "reject" takes a free-text redirect message back to the model (opencode `routes/session/permission.tsx`)
- [x] Command-prefix arity for simple "allow always" rules: `git checkout branch` → rule for `git checkout`; compound commands use exact normalized rules and cannot reuse a first-command approval (opencode `permission/arity.ts`)
- [x] Project trust prompt on first run in a directory (pi: `trust.json`, `defaultProjectTrust: "ask"`) — `internal/tui/trust.go` + `~/.ghg/trusted.json`, plain-terminal prompt before the TUI starts, piped stdin declines safely
- [x] Secrets as references, never values: `"$VAR"`/`"!cmd"` (or `${ENV_VAR}`-style) indirection in config and MCP/tool init, resolved host-side at point of use so raw keys never enter the event log or model context (exo `crates/exoharness/src/secrets.rs` — AES-GCM at rest with keychain/file master key is the full version; the indirection alone is most of the safety)
- [x] Initial shared execution policy and OS sandbox substrate: canonical native roots, protected metadata, minimal child environments, fail-closed macOS Seatbelt/Linux bubblewrap wrappers, explicit sandbox/network modes, local MCP/LSP process wiring, and headless/TUI runtime inheritance — plan.md Phase 3; backend-denial retry remains open
- [x] Optional `auto-review`/`approve-for-me`: one tool-less bounded `tiny` decision for the deterministic ambiguous middle, strict structured output, human fallback in interactive mode, fail-closed headless behavior, one-shot network grants plus human-only external-root/protected grants, in-flight deduplication, and separate reviewer audit telemetry — plan.md Phase 3

## Theming & config

- [x] ctrl+p command settings (opencode-style): modal dialog (own filter line, esc pops one level, ↑/↓ wraps), category headers, "Suggested" group pinned when the filter is empty, dimmed keybind/slash hints teach shortcuts, cheap subsequence fuzzy filter; fully interactive — left-clickable rows show live state badges, ←/→ step reversible settings (effort, thinking, mouse) in place, and enter drills into sub-panels (role-first model settings with one-line routes, mode/effort levels, compaction model, inline goal editor) that apply real changes without leaving the settings
- [x] Single keybind+command registry: settings, slash commands, help, and footer hints all derived from one table (opencode `config/keybind.ts` — the highest value-per-line idea in that repo)
- [ ] One generic fuzzy-select widget reused by every picker: model, session, theme, timeline (opencode `ui/dialog-select.tsx`) — **cut**
- [ ] KV table in sessions.db for settings-toggleable UI prefs — no config ceremony per toggle (opencode `context/kv.tsx` pattern) — **cut**
- [ ] Theme support: JSON themes with named defs + `{dark, light}` variant pairs; a "system" theme built from the terminal's real settings (opencode `theme/index.ts`) — **cut**
- [x] `"mouse": false` config escape hatch so native terminal selection works (opencode `app.tsx:196`) — also runtime `/mouse [on|off]`; with capture on, left-click controls work and hold shift to select text in the transcript

## CLI surface

- [x] Non-interactive one-shot mode: `ghg run "prompt"` — reads piped stdin too, `--format json` emits the raw event stream for scripting (opencode `cli/cmd/run.ts`)
- [x] `ghg sessions` list subcommand
- [x] `ghg artifacts gc` — age/size cleanup of unreferenced retained tool-result payloads; referenced payloads are never removed
- [x] Env markers in child processes (`GHG=1`, `GHG_SESSION_ID`) so scripts can detect they run under the agent (opencode sets `AGENT=1`, `OPENCODE_PID`)

## Autonomy & durability

From [exo](learnings/other-harnesses/exo.md). Every item in this section shipped; the
original "do now / high value, cheap / later" ordering has been dropped, because
sequencing now lives in [plan.md](../plan.md) and two competing orderings is how items
get lost. The historical notes below are kept because they record *why* each design
came out the way it did.

- [x] `todowrite` planning tool (the biggest gap): conversation-scoped store, full-list rewrite each call, exactly one item in_progress, injected back each round so the plan survives long tool loops and compactions; caps ~50 items × 300 chars (exo `exo/tools/todo-tools.ts` is ~100 lines; the claude/opencode pattern) — `internal/agent/todo.go`, persisted on the sessions row, restored on resume
- [x] Explicit TUI planning workflow: `/plan <goal>` proposes a validated structured plan with `smart`, and `/execute [plan]` runs the latest or supplied plan with `fast`, seeding `todowrite` for structured plans — `internal/agent/plan.go`, `internal/tui/plan.go`; CLI planner flags and declarative agent definitions remain in Phase 2
- [x] Synthesize error tool-results for dangling tool calls when materializing a crashed/interrupted turn on resume — correctness fix, not a feature: one interrupted turn can otherwise produce an API-rejected history (exo `flushDanglingToolResults`, `exoharness/typescript/harness/index.ts:786-804`) — `answerDanglingToolCalls` at the `session.Load` boundary, synthetic result appended right after its assistant message
- [x] Compaction as a recorded event, not `DELETE FROM messages`: store summary + cutoff seq and derive the prompt view; the raw log stays queryable so a bad compaction is inspectable and retryable. The thin end of the event-sourcing wedge without a store rewrite (exo spec.md: "the durable conversation does not have to equal the prompt") — `compactions` table (append-only summary+cutoff), `Load` derives the view, `/compact log` inspects, `/compact retry` undoes the latest and recompacts from the raw log
- [x] Workspace rewind: git-snapshot the working tree per turn (or on demand) so file changes can be rolled back, and record the rollback in the session — "rewind does not erase history": rolling back the world must not delete the memory of what was tried (exo `rewind_sandbox` appends `SandboxStarted{snapshot_id}`; opencode `revert.ts` is the same idea) — pre-turn snapshot pinned under `refs/ghg/snapshots/`, keyed by turn index in a `snapshots` table that `DeleteFrom` trims with the messages; `applyRewind` restores via `checkout <ref> -- .` and notes "⟲ workspace rewound — N file(s) restored"; untracked files never touched

- [x] `remember`/`forget` memory tools: plain markdown files (`~/.ghg/memory.md` installation scope + `~/.ghg/sessions/<id>.memory.md` session scope), checkbox bullets the user can edit by hand; `forget` strikes rather than deletes; always-inject with a hard cap (50 × 300 chars — the cap is the retrieval strategy, no embeddings); `/memory` lists both scopes numbered and marks entries done from the TUI (exo `exo/tools/memory-tools.ts`, redesigned to markdown after the opencode finding: opencode has no memory tool, its answer is AGENTS.md — files you own and diff)
- [x] Stealable `me.md` operating rules for the system prompt: "the tool set changes turn to turn — never assume a tool exists because it did earlier"; "after ~3 failed attempts on the same blocker, escalate plainly instead of looping"; git hygiene ("never `git add .`, review staged diff for secrets, never force-push") (exo `exo/prompts/me.md`) — shipped in `cmd/ghg/main.go`'s system prompt, plus a remember/forget pointer

- [x] Minimal scheduler + generic wakeup channel: `@every 10m` / `@at <rfc3339>` tasks firing machine-authored user-message turns; grid-anchored fires (slow runs don't drift), one-shot completion stays listed as (fired), fires defer while busy without drifting the grid. Cron syntax deliberately cut (two forms cover the use); the record-then-deliver outbox and `reportPrompt` routing remain future work if external channels land (exo `scheduler_runtime.rs`, `conversation_wakeup.rs`) — `internal/schedule` (parser, ~70 lines), `schedules` table in sessions.db, 5s ticker in the TUI, `/schedule @every|@at <prompt> | list | cancel <n>`, ⏰ transcript marker

**Deliberately cut** (exo needs them because it's long-running and edits itself in production; a coding TUI doesn't):

- ~~Full event-sourced store rewrite~~ — too big once compaction-as-event lands; keep custom-kind discipline inside the messages-table world instead
- ~~`/events` introspection tool~~ — pays off with adapters/restarts ghg doesn't have; cost already lives in the status line
- ~~`rebuild_and_restart_harness` + SELF.md self-map~~ — a ghg rebuilt by hand between sessions doesn't need to restart itself mid-conversation
