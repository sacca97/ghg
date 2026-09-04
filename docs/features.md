# Features

ghg is a minimal coding agent: an interactive bubbletea TUI driving an
LLM tool-use loop (bash / read / write / edit / grep / glob / find_files / lsp / lsp_rename / task) with provider-routable
models. This document is the map of what's shipped and where it lives. Each
section links the behavior to the code and its tests.

## The agent loop

`internal/agent/agent.go` — `Agent.Turn` is the loop: append the user message,
stream a completion, run any tool calls, append results, repeat until the model
stops calling tools. Steered messages (`Steer`) inject at loop boundaries,
never mid-generation.

### Parallel tool calls with per-path file locks

When the model emits several tool calls in one turn, `runTools` fans them out
to goroutines and collects results on a buffered channel, laid back out in
**call order** (the API matches tool results to call IDs). `OnToolStart` /
`OnToolEnd` fire per call as they run, so the UI shows each tool live.

`internal/agent/filelocks.go` — mutations to the same file serialize through a
**per-canonical-path channel semaphore** (a 1-capacity `chan struct{}` per
path: send to acquire, receive to release). Multi-file edits acquire all
canonical paths in sorted order, so overlapping calls cannot deadlock. Edits
to different files run in parallel; `bash` and `lsp_rename apply` take the
global write side of a read/write lock because their side effects cannot be
attributed to one path before validation. Reads and rename previews don't lock.

This is the Go-native port of pi's `withFileMutationQueue` (per-path promise
chains in TypeScript). In Go the lock is a buffered channel — no explicit
unlock bookkeeping.

Tests: `parallel_test.go` — `TestToolCallsRunInParallel` (overlap measured via
a concurrency counter), `TestSamePathEditsSerialize`, `TestToolMutationPath`,
`TestCanonicalPathKey`.

### Native grep, glob, and fuzzy path search

`internal/tools/search.go` provides read-only native `grep`, `glob`, and
`find_files` tools; `internal/tools/structural_search.go` adds bounded,
Go-only structural matching with metavariables. `grep` accepts a regular expression or an OR `patterns`
array, groups matching lines by file, ranks narrow/touched/modified paths, and
returns stable cursor pages with a small per-file cap. `glob` returns exact
relative-pattern matches; `find_files` uses the shared fuzzy path index and
scores every candidate before applying its result cap.

These tools default to the current working directory and accept an explicitly
selected existing file or directory. Directory searches use Go's `os.Root`,
never follow symlink entries, skip `.git` and non-regular files, and return absolute
paths only when the selected root is outside the working directory. Explicit
files are searched directly even when their parent directory is ignored.

`internal/tools/ignore.go` loads nested `.gitignore` files in ancestor order and
supports negation, leading-slash anchoring, basename patterns, and directory-only
rules. An ignored parent prevents a child negation from leaking files back into
the result; negating the directory itself reopens its subtree. Binary files are
skipped. Search pages default to 25 results and select complete rendered entries
under the model-facing 8 KiB ceiling. The cursor advances only past entries
actually displayed, so `search_displayed`, `search_remaining`, and later-page
counts cannot skip an unseen result; an entry that cannot fit leaves the cursor
unchanged and asks for a narrower search. Pages paginate over a bounded
immutable snapshot retained in the session/output path. Snapshots cap at
10,000 results and a 100,000-entry scan limit, with explicit displayed/remaining
and incomplete-retention metadata. All filesystem walks honor the call context.
The TUI's fuzzy `@` completion uses the same shared index, so strong matches
late in a tree are not lost to an early traversal cutoff.

Tests: `internal/tools/search_test.go` — `TestGrepTool`,
`TestGlobToolPatternsAndOrdering`, `TestGitignoreRules`,
`TestSearchLimitsCancellationAndInvalidArguments`,
`TestMalformedGitignore`, and `TestExplicitIgnoredFileIsSearchable`.

`internal/tools/phase25_test.go` and `internal/search/state_test.go` cover OR
patterns, stable cursors, noisy-file diversity, fuzzy late matches, long-result
byte ceilings, later-page accounting, and exploration redirects. The Phase 2.5
acceptance matrix also covers every observed edit operation, overlap/stale/
cross-session rejection, byte-limited reads, line-ending preservation, sorted
multi-file locking, publication rollback, and bounded readback; the LSP output
budget has a dedicated `internal/lsp/diagnostic_test.go` regression.

### Stateful observed edits

`internal/tools/read.go` records each bounded complete-line `read` as an
observation containing the exact bytes issued, canonical path, line range, and
continuation offset. A byte-limited observation still authorizes the complete
lines it returned; only lines outside that record require a narrower reread.
`internal/tools/edit.go` makes `mode: "observed"` with an `edits` array the
primary mutation shape. It authorizes only ranges from the same session and
path, uses same-position matching first and unique exact-byte relocation after
shifts, and rejects stale, ambiguous, intersecting, or unobserved ranges.
`mode: "exact"` is an explicit temporary compatibility mode.

Observations and search snapshots mirror into `sessions.db`; live registries
are shared by subagents and survive model switches. Multi-file edits preflight
all originals and permissions, stage same-directory files, publish atomically,
preserve modes/line endings, and return a compact diff, readback, and
diagnostics. `ToolTelemetry` reports preview/retained/original bytes,
truncation, and Bash exploration redirects to JSON consumers.

### LSP navigation, safe rename, and post-edit hooks

`internal/lsp/manager.go` owns one lazy language-server manager per TUI or
headless run. The same `tools.ToolRuntime` is inherited by Plan mode and
delegated agents, so server processes, document versions, sandbox policy, and
hook configuration are not duplicated. The manager synchronizes exact file
bytes before each request, advertises UTF-16 only, and warms covered files
asynchronously after a successful `read`.

The read-only `lsp` tool supports only `definition`, `references`,
`document_symbol`, and `hover`. Results are canonical, policy-authorized,
sorted, deduplicated, bounded, and marked untrusted: limits are 20
definitions, 100 references, 200 flattened symbols, and 8 KiB of hover text.
Plan mode can use `lsp`, but not `lsp_rename`.

`lsp_rename preview` validates a complete file-only `WorkspaceEdit`, converts
UTF-16 ranges at exact rune boundaries, and stores the original/updated bytes
behind a short `rn_...` id scoped to the current session. `lsp_rename apply`
uses that exact plan under the global mutation lock, rechecks authorization,
bytes, versions, and the normal permission gate, then publishes atomically;
success consumes the id and stale or restarted sessions require a new preview.
Server-driven `workspace/applyEdit` is rejected explicitly.

Root `postEdit` config entries are trusted argv arrays with optional normalized
extensions and a 1–60 second timeout. After a successful write, edit, or rename
publication, matching hooks receive sorted canonical paths directly under the
shared sandbox/runtime; ghg then rereads final bytes and runs diagnostics.
Hook failures never roll back the mutation or change its exit status, but
bounded redacted output is reported. `internal/lsp/navigation_test.go` and
`internal/tools/hooks_test.go` cover the main and failure paths.

### Project instructions and streamed tool output

`internal/config/project.go` loads a bounded `AGENTS.md` only from the current
trusted project root. Missing, unreadable, oversized, or symlinked files are
ignored; trusted instructions are inserted beside the user's `~/.ghg/me.md`
block in the system prompt. Interactive startup adds the block after the folder
trust prompt, so first-run acceptance applies immediately. Headless
`ghg run` is explicitly trusted automation and receives the same block.
Tests: `config/project_test.go` and `cmd/ghg/main_test.go`.

Long-running non-interactive bash calls emit accumulated output snapshots every
100ms through `tools.WithOnUpdate`. The callback is stored in the per-call
context, `agent.Events.OnToolOutput` includes the tool-call id, and the TUI
renders the last three lines under the running tool row. The final `OnToolEnd`
event remains authoritative and collapses that row. JSON headless output also
emits `tool_output` events. Tests: `bashrun/bashrun_test.go`,
`agent/agent_test.go`, and `tui/toolrun_test.go`.

### Compaction

When the conversation fills the context window, old turns fold into an
LLM-generated summary. Two triggers:

- **Proactive**: `maybeCompact` runs before each request once the latest
  successful request's provider-reported context size (`PromptTokens +
  CompletionTokens`) crosses the adaptive threshold — $\min(0.80 \times \text{window}, 400000, \text{window} - \text{reserve})$.
  When explicit `compactPct` is set in config (clamped 10–90; `Agent.CompactThreshold`
  holds the fraction), the chosen percentage is honored while respecting output reserves.
  The value is zero until the first successful response. Slide it in the settings's
  "Compaction level" row (←/→ steps ±10%).
- **Reactive**: if the provider still rejects a request with a context-limit
  error (`context_length_exceeded`, `prompt_too_long`, HTTP 413), `Turn`
  compacts once and retries. A `compacted` guard prevents retry loops.

`compact()` uses cumulative, dedicated summarization:
- **Cumulative Checkpoints**: reuses the prior checkpoint inside `<previous_checkpoint>`
  tags so the summarizer updates existing facts rather than re-folding from scratch.
- **Dedicated System Prompt**: runs under a focused prompt instructing the model to produce
  state checkpoints without attempting task completion or tool execution.
- **Truncated Summary Rejection**: responses truncated by token limits or returning empty
  checkpoints are rejected to safeguard context integrity.
- **Bounded Tail & Atomic Groups**: keeps the system prompt and a recent tail bounded at
  $\min(\text{ContextLimit}/4, 24000)$ tokens. Kept groups are selected atomically so tool
  results are never severed from their assistant calls.

The summary runs as a non-streaming `Complete` on the configured `tiny` role when a roles
block is present. A legacy config without roles uses the built-in
`deepseek-v4-flash-0731` (`config.DefaultCompactModel`). An explicit
`compactModel` / `compactProvider` remains the per-session override, and an
unavailable fallback leaves compaction on the conversation's own model.

Token bookkeeping: `models.Usage` (prompt/completion/cached) is read off the
terminal stream chunk (`stream_options: include_usage`) and folded into session
totals via `AddUsage`. `ContextTokens()` falls back to `EstimateTokens` when unmetered
or before the first provider response, so the status bar is never stuck at zero.
`ActiveTokens()` combines base token count with in-flight message/tool estimations,
triggering `maybeCompact()` proactively before dispatching bloated requests. If a stream
exceeds context limits mid-generation, an overflow watchdog aborts the stream, compacts,
and transparently retries the turn.

Commands: `/compact` (compact now), `/compact <model> [provider]` (pick the
summarizer), `/compact off` (restore the configured `tiny` role, or the legacy
built-in default). The settings's
"Compaction model" panel lists every configured model behind a
"default (…)" row that restores the default; "Compaction level" steps the
threshold ←/→.

### Plan runaway guard & per-turn tool lifecycle

- **Tool Freezing**: `AllTools()` and tool definitions are computed once at the start
  of `Turn()`, ensuring stable definitions across all tool rounds.
- **Plan Budget Ceilings**: in Plan mode, weighted token expenditure is tracked with a
  128 model-call ceiling. Reaching reserve disables tools and
  forces a final synthesis request for `<proposed_plan>`.
- **Sparse Events**: `FanIn` leaves unprovided callbacks `nil`, avoiding unnecessary
  JSON marshaling or token estimations for background workers.
- **Cheap Review Correction**: invalid code reviews trigger a bounded 2-round correction
  definition exposing only `submit_review` with the validation error.

### Session-scoped history search and recall

`internal/agent/history.go`, `internal/session/history.go` — historical conversation
evidence is stored in an append-only rebuildable SQLite FTS5 index. Compaction prunes the
active context window, but all previous turns remain queryable:

- `history_search` — searches prior user, assistant, and tool messages using full-text
  search, with optional role/epoch filters and opaque stable cursor pagination.
- `history_read` — retrieves exact, bounded ranges of raw historical messages formatted as
  plain untrusted evidence. Historical messages are never re-injected as live protocol messages.

Tests: `internal/agent/history_test.go` and `internal/session/history_test.go`.

### Recoverable tool-result outputs

`internal/tools/result.go`, `internal/session/` — tool
execution has a structured result path. Every result keeps a model-sized
`Preview`, bounded retained evidence, original/stored byte counts, completion
state, exit code, and source metadata. Bash, file reads, native search, MCP,
and output reads mark returned bytes as untrusted; the agent wraps those
bytes in one `<untrusted_tool_output>` block before sending them to a
provider. Direct legacy `tools.Execute` callers still receive the old plain
preview.

Retained evidence is capped at 10 MiB per result by default. Overflow keeps a
deterministic head/tail, hashes the retained bytes with SHA-256, and appends a
path-free recovery hint. Persistent runs store payloads under
`~/.ghg/outputs/sha256/<prefix>/<hash>` with private directory/file
permissions and index references in `sessions.db`; `--no-session` uses a
private temporary store removed on exit. Set `{"outputs":{"enabled":false}}`
to opt out; bounded previews remain available. `maxBytes` changes the
per-result retention ceiling. The legacy `artifacts` config key remains accepted.

The agent exposes `output_list` and `output_read` as session-scoped,
read-only operations. Listing is metadata-only and bounded; reading accepts
an output id plus a bounded byte range, never a path or another session id.
`ghg outputs gc --max-age … --max-bytes N` removes only unreferenced
payloads, so forks can share immutable content safely. Compaction preserves
the raw message log, keeps atomic tool-call groups, carries a metadata-only
output manifest for cited/recent references, and shrinks an oversized recent
result without dropping its recovery id.

The legacy `artifact_list`/`artifact_read` tool names and `ghg artifacts gc`
command remain accepted aliases.

### Background subagents

`internal/agent/background.go` — `task` with `background: true` launches a
subagent that runs **concurrently with the parent** instead of blocking the
turn. This is the channel-native port of opencode's `background-job.ts`
registry.

Each task is a `BackgroundTask` with a `Done chan struct{}`. When the subagent
settles, the registry records the final state, runs `OnChange` and `OnRecord`,
and then **closes `Done` once** — closing a channel broadcasts to every waiter
at once, so the tool caller, the TUI, and `/tasks` all wake after persistence
has completed (opencode needs a per-job `Deferred` for the same thing). On
settle the report fans back into the parent as a **steered message**, so the
model sees it on the next loop boundary.

- `Tasks().List()` / `Get(id)` / `Cancel(id)` — registry snapshot + cancel.
- `Tasks().OnChange` — the TUI installs a callback that sends a message to
  redraw live. `Tasks().OnRecord` — a second hook the TUI uses to upsert the
  task into the session store on start and settle.
- `/tasks` lists running/done subagents with report previews. The persistent
  dock strip above the input is mouse-clickable: `dockTop()` maps screen rows to task rows,
  skipping the focused hint row (`dockSkip`) so a click opens the row
  actually clicked.
- **Persisted across resume.** The session store's `tasks` table records
  every start/settle; `resume()` seeds the registry via `RestoreTask`
  (settled, `Done` pre-closed, marked `Restored`). A row still `running` on
  disk means the subagent died with the last process exit, so it comes back
  as `error` — "interrupted — ghg exited". Restored tasks are history:
  `/tasks` lists them with a `(restored)` marker; the dock never shows them.
  The dock itself shows running tasks plus ones settled within a one-minute
  grace window (`dockSettledGrace`) — long enough to notice the ✓, then the
  strip cleans itself.

Background tasks use a context **not** tied to the current turn — they outlive
it by design. Cancelling a task cancels its subagent's turn.

Tests: `TestBackgroundTaskDeliversReport`, `TestBackgroundTaskBroadcastsToManyWaiters`
(8 waiters all woken by one channel close), `TestBackgroundTaskCancel`;
persistence: `session.TestTaskRoundTrip`, `TestRestoreTaskSettledAndVisible`,
`TestResumeRestoresTasks`, `TestTaskPersistsOnStartAndSettle`, and
`TestTaskDoneWaitsForSettlementCallbacks`;
dock click hit-testing: `TestDockClickOpensClickedRow`,
`TestDockClickIgnoredWhilePaletteOpen`.

### Detachable live sessions (supervisor / worker)

`internal/worker/`, `cmd/ghg/worker_process.go` — interactive sessions run inside a
dedicated local worker process communicating over a per-session Unix domain socket
(`~/.ghg/run/<session-id>/worker.sock`) with an exclusive OS lifetime lock.

- `/detach` — gracefully disconnects the TUI interface after an atomic request/acknowledgment
  handshake. The worker process continues executing active model streams, tools, and background subagents.
- `ghg ps` — lists all active and idle detached sessions with uptime, model, and status.
- `ghg attach <id>` — reconnects to a running or idle detached session, reconstructing the full
  transcript snapshot, live output rings, active roles, and any pending permission approvals.
- `ghg stop <id>` — requests graceful cancellation and shutdown of a detached session.

Tests: `internal/worker/server_test.go` and `internal/worker/state_test.go`.

## Models & providers

`internal/config/config.go`, `internal/config/catalog.go` — models route to
providers; the provider's `GET /models` is the source of truth for
capabilities. Two distinct limits, both honored:

- **Context window (input)** — `Model.Context` (legacy `maxTokens` still
  parses via `ContextWindow()`), overridden by the provider's
  `context_length`, or filled from the matching models.dev `limit.context`
  record when neither is configured. Compared with the latest successful
  request's reported prompt-plus-completion usage to drive proactive
  compaction.
- **Output cap** — `Model.MaxOut`, else the provider's `max_completion_tokens`,
  else the context window. The selected adapter maps it to its wire field
  (`max_tokens` for Chat Completions/Messages or `max_output_tokens` for Responses).
- **Wire protocol** — an optional `Model.API` override wins over the selected
  provider profile route.

The catalog (`~/.ghg/models.json`) caches each provider's model list with a
24h TTL and refreshes in the background. Missing context lengths and reasoning
controls are enriched from the daily models.dev cache (`~/.ghg/models-dev.json`),
fetched lazily from its public `/api.json` catalog for the models currently
listed by ghg. That endpoint is one all-provider snapshot, so filtering happens
during normalization rather than in the URL; the normalized cache retains only
requested model IDs. Reasoning effort choices are therefore model-specific:
the picker can expose `max`, `off`/`on` for toggle-only models, or `off` alone
when models.dev explicitly advertises no caller control. Graded values use the
adapter's effort field; toggle metadata is lowered to the adapter's explicit
enable/disable representation where supported. When the provider advertises
per-token `pricing` (OpenAI-compatible catalog shape — `prompt`,
`completion`, `input_cache_read` decimal strings), the catalog caches the
parsed rates and the bottom status box appends the session's cumulative cost to the
context display (`ctx 31.1k/128k · $0.0134`): fresh input at the prompt
rate, cached input at the cache-read rate (full prompt rate when none is
advertised), output at the completion rate — `models.SessionCost`. Providers
without pricing hide the segment entirely. Tests: `models/openai_test.go`
(`TestSessionCost`, pricing unmarshal), `config/catalog_test.go`,
`tui/status_test.go` (`TestStatusLineShowsCost`, `TestStatusLineHidesCostWithoutPricing`).

`internal/models/backend.go` — the agent-facing `Backend` contract is deliberately
smaller than a provider client: `Stream` accepts a request-local `EventSink`
and returns the assembled assistant `Message` plus usage; `Complete` returns a
message plus usage for one-shot work such as compaction. The protocol adapters
implement `Backend` directly, while `NewBackend` selects the compiled adapter from the provider protocol
(`openai-completions` remains a compatible legacy spelling). Retry callbacks
supplied by a turn stay in the request-local sink, so foreground and background
subagents can share a backend without mutating a client hook. `CatalogBackend`
is an optional capability: a configured local endpoint can work without
implementing `/models`.

`internal/models/profile.go` — declarative provider profiles are strict YAML
metadata with embedded, user, and trusted-project precedence. Config entries
keep credentials and can override a profile's URL or protocol; legacy entries
without `profile` become anonymous in-memory profiles. URLs normalize trailing
slashes and require HTTPS unless they target `localhost`, `127.0.0.1`, or
`::1`. Profile auth supports bearer, raw header, and none; optional
`auth.env_var`, `docs.keys_url`, and `catalog.public` metadata drive generalized
authentication. Ordered `routes` use `path.Match` globs to override only
protocol, auth mode/header, and default headers for a model; first match wins.
Default headers are static and never resolve secrets. The OpenAI-compatible,
OpenAI Responses, and Anthropic Messages adapters honor that auth/header policy.
Tests: `models/profile_test.go`, `models/backend_test.go`, `models/responses_test.go`,
`models/anthropic_test.go`.

### Model roles

`internal/config/roles.go` and the existing TUI/CLI builders provide four model
roles: `default`, `smart`, `fast`, and `tiny`. A role resolves to its configured
model/provider, then the configured `default` role, then legacy
`defaultModel/defaultProvider`; an explicitly configured invalid route is an
error. Acting sessions default to `fast`, planning sessions to `smart`, while
compaction and foreground/background `task` calls use `tiny`. The TUI bottom
status bar exposes clickable `execute`/`plan` modes (`plan` maps to `smart`) and
a role-first model settings flow (`default`, `plan`, `fast`, `tiny` → one-line
`models/model` routes). Routes from providers without a configured
credential are omitted. `ghg run --role` selects a role for a headless run, and
explicit model/provider flags remain route overrides. Tests:
`config/roles_test.go`, `tui/provider_route_test.go`, `tui/mode_test.go`,
`agent/agent_test.go` (`TestTaskUsesTinyRoleFactory`), and
`tui/compact_cmd_test.go` (`TestCompactModelUsesTinyRole`).

`internal/models/openai.go` — the streaming client. Typed `HTTPError` (keeps the
`<status>: <body>` shape), `IsContextLimit()` classifies context-overflow
errors for the compaction retry, `Stream` returns the message + usage, and
`Complete` is the non-streaming round-trip used by compaction.

`internal/models/anthropic.go` — the native Anthropic Messages adapter. It maps
top-level system prompts, multimodal content, tools and grouped tool results,
preserves signed thinking blocks for follow-up turns, assembles fragmented
SSE events, applies stable-prefix prompt-cache breakpoints, maps Anthropic
usage/model metadata, and shares the existing retry/cancellation/error
boundaries.

Transient request failures — 429, any 5xx (e.g. a gateway's 524), and
transport errors — retry with exponential backoff (1s→2s→4s… capped 20s,
+25% jitter, ctx-cancellable). Budget: `maxRetries` in config (default
`models.DefaultMaxAttempts` = 8, `1` disables). A streaming attempt is only
retried before the first visible delta reaches the UI — after that a retry
would replay rendered text, so the error surfaces instead. Mid-stream
provider `error` chunks and 4xxs (including context-limit, which the
compaction path must see immediately) are never retried. Each retry posts a
`⚠ request failed (…) — retrying in Ns (attempt N/M)` line via the
request-local backend event sink (the legacy `Client.OnRetry` hook remains for
direct client users). Tests: `models/retry_test.go`, `models/backend_test.go`.

`internal/auth/auth.go`, `cmd/ghg/auth.go`,
`internal/config/provider_key.go`, and `internal/tui/auth_cmd.go` provide
profile-driven provider onboarding. `ghg auth <id> [--env] [<key>]` and
`/auth <id> [<key>]` resolve IDs from the loaded YAML profile set; bare `/auth`
lists every profile with its configured status, and provider-only TUI auth uses
the existing masked prompt. Profile metadata supplies the endpoint, protocol,
auth header, environment variable, and setup URL, so adding a provider such as
`opencode` is a YAML-only change.

Credentials are validated before config is written. Catalog-capable profiles
with private catalogs use the single `GET /models` response both for validation
and catalog seeding; public catalogs are fetched for a real model ID and then
validated with an authenticated probe that recognizes `error.type: AuthError`.
Catalog-less profiles use an authenticated probe and do not create a catalog.
A backend with neither capability requires explicit confirmation before a key
is stored as **unvalidated**. Literal and environment modes are mutually
exclusive, and `auth:none` profiles refuse key entry. Keys never enter the
TUI transcript/history/queue or event log; validation errors redact response
body echoes. Tests: `auth/auth_test.go`, `config/provider_key_test.go`,
`cmd/ghg/auth_test.go`, `tui/auth_cmd_test.go`, plus the provider metadata
tests. On an interactive first run with no usable credential, the TUI remains open
with a nil agent and an actionable `/auth` note; agent-dependent commands are
guarded, and a successful auth builds the first agent in place. Headless
`ghg run` remains fail-fast. Tests: `tui/auth_cmd_test.go` cold-start cases.

## The TUI

`internal/tui/tui.go` — bubbletea fullscreen alt-screen. Highlights:

- **ctrl+c is a two-stage key.** While busy it interrupts the turn (first press
  arms, second cancels). While idle it quits — but only on a **second press
  within a 2-second window**, so a stray ctrl+c can't nuke the session. The
  hint `press ctrl+c again to quit` shows while armed.
  Tests: `quit_confirm_test.go`.
- **Collapsible tool results.** Tool results store raw output in a `blockTool`
  transcript block and render collapsed to 5 lines with a `… +N lines` hint.
  `ctrl+e` toggles the most recent; clicking a block expands/collapses it
  (each block tracks its rendered line range `y0`/`y1` so the click row maps
  through the viewport offset). Blocks re-render at the current width on
  terminal resize. Tests: `tool_expand_test.go`, `resize_test.go`.
- **Markdown.** Assistant messages render through glamour; streamed in-flight
  text stays plain and renders on flush. `markdown.go`.
- **Clickable links (OSC 8).** URLs and existing local file paths in the
  transcript are terminal hyperlinks — cmd/ctrl-click opens them, no mouse
  plumbing in ghg (the terminal owns the click). `links.go` runs two passes
  over glamour's output: `hyperlinkGlamourLinks` rewires rendered
  `[label](url)` links so the href atom becomes the OSC 8 target on the
  label instead of a second visible copy (bare autolinks become clickable in
  place), and `linkifyRenderedFilePaths` wraps bare `path/to/file[:N]` tokens
  in `file://` links — gated on the file existing on disk, resolved against
  the process CWD. User-input echoes (submit, resume replay, steer) get the
  same file linkification on the raw text. Unsupported terminals ignore
  OSC 8 and show the underlined text as before; copy/selection strips the
  sequences. Tests: `links_test.go` (ref regex, target gating, glamour
  rewiring incl. wrap-split links, end-to-end renderMarkdown, user echo).
- **Command settings** (ctrl+p) with sub-panels for role-first model settings,
  mode/effort/goal/compaction, plan/execute actions, and ←/→ steppers for the
  compaction level — `settings.go`.
- **Mouse**: `/mouse [on|off]` controls capture; with capture off the terminal's
  native selection works, with it on left-clicks activate the visible controls
  and shift-drag still selects. `"mouse": false` in config disables capture at
  startup; run `/mouse on` to re-enable it for the current session.
- Queueing (enter while busy), steering (empty enter), history recall (↑/↓),
  `@file` mentions, `$skill` invocation, structured `/goal` lifecycle with
  explicit `/goal resume` after restart, `/resume` session picker, `--continue`,
  and `/effort` reasoning levels — see the roadmap for the full list.
- **Settings commands run mid-turn.** `/theme`, `/mouse`, `/effort`, `/tasks`,
  `/help`, `/cd`, `/pwd`, and the non-submitting `/goal` forms (bare, `clear`,
  `rounds`) execute immediately while busy instead of queueing — queued text
  is sent to the model verbatim after the turn, which is nonsense for a
  settings change. The `busyCmd` allow-list gates this; everything else
  (`/model`, `/goal <text>`, plain messages) still queues. These commands only
  touch TUI-local state or fields read at the *next* request, and their
  confirmation notes append as transcript blocks safely behind the streaming
  one. Tests: `queue_test.go` (`TestBusyCmdAllowList`, `TestEnterWhileBusy*`).
- **`!` shell escape, `/cd`, `/pwd`** — `shell.go`. An input starting with
  `!` runs locally via the same `bashrun` runner as the agent's bash tool
  (120s cap, `tools.TruncateTail`, `(exit …)` markers) — no model turn, no
  busy state, runs immediately even mid-turn, and queued `!` lines execute
  when the queue drains instead of being submitted to the model. The command
  runs on a goroutine and lands via `shellDoneMsg` (the UI never blocks), the
  output lands in the transcript as a collapsed tool block **and** in the
  conversation so the model sees it at the next request: idle via
  `Agent.AppendUser` (non-authored `$ <cmd>` user message), mid-turn via
  `Agent.Steer` (mutex-guarded, injected at the next loop boundary with the
  usual `(steered)` echo) — the turn goroutine owns `Agent.Messages` while
  busy. Esc stays bound to the turn; a running escape isn't cancellable (the
  120s cap bounds it). `/cd [dir]` changes ghg's process cwd (an in-flight
  command keeps its already-resolved cwd, POSIX; the next spawns, relative
  tool paths, and the `@` index follow); bare prints it, `~` expands. `/pwd`
  prints it. Port of opencode's `session.shell` minus the shell-mode chrome —
  see the `ponytail` note in `shell.go`.
  Tests: `shell_test.go` (output/message routing idle+busy, queue-drain,
  truncation, echo rules, cd/pwd incl. `~` and bad dirs).
- **`/goal-from-context [n]`** distills the last *n* conversation messages
  (default 8, clamped to the available history) into a detailed goal — a
  concrete outcome line plus a bullet list of checkable completion criteria —
  with one non-streaming call on the current model (the compact-model override
  is deliberately ignored), then sets it exactly like `/goal <text>` and starts
  the structured goal lifecycle. The transcript note states the exact window used. Prompt
  building is pure (`agent.BuildGoalFromContextPrompt` over the window from
  `agent.GoalFromContextMessages`); the TUI command mirrors `/compact`'s
  goroutine + `goalFromContextMsg` pattern, refusing while busy and running
  inline when headless. Tests: `internal/agent/goal_test.go` (`TestGoalFromContextPrompt`)
  and `internal/tui/commands_test.go` (`TestGoalFromContextMsgHandler*`).
- **`/plan [goal]` / `/execute [plan]`** are an explicit two-step workflow. `/plan`
  enters persistent read-only Plan mode on the `smart` role, where ordinary turns
  can inspect the repository and either continue conversationally or finish with a
  Markdown `<proposed_plan>` block. `/execute` runs that Markdown proposal, or
  supplied plain text, through the `fast` role; the ordinary executor owns its
  `todowrite` checklist. Plan mode is never invoked for ordinary chat. The command
  settings is bounded to the terminal height and scrolls with
  ↑/↓ or the mouse wheel. The bottom status box's separate `(effort)` control
  cycles through off and the available effort levels.
- **`/ask <question>`** answers directly. It can investigate repository questions
  with read-only tools, but cannot edit files, run commands, spawn tasks, or mutate state.
- The bottom status box keeps the active model/provider, a separate `(effort)` indicator,
  and `plan`/`execute` mode visible. Clicking the model cycles the routes already
  selected for the `smart`/`plan`, `default`, `fast`, and `tiny` roles without changing
  mode; clicking `(effort)` cycles the available reasoning levels; clicking the mode
  cycles `plan`/`execute` and enforces the matching `smart`/`fast` role. The role-first
  model settings remain available from `ctrl+p` or `/model`. The model segment reserves
  the longest selected role-model width and truncates only on narrow terminals. The
  top line contains only `ghg · skills: N loaded`; it omits the redundant model,
  working directory, goal, scroll progress, reasoning control, and token usage.
  Token usage remains in the
  bottom status box as `↓incoming/↑outgoing tok`; the box is delimited on all four
  sides, with vertical separators between each field. The transcript viewport
  retains a fixed height while scrolling, keeping the prompt and status box
  below the divider stationary.

### Declarative agent definitions and headless planning

`internal/agent/definitions.go` loads trusted project definitions from
`.agents/*.md` and user definitions from `~/.ghg/agents/*.md`. Each file has strict
frontmatter (`name`, `description`, `role`, `tools`, and `max_rounds`) followed by
its Markdown prompt. Project definitions take precedence over same-named user
definitions; unknown tools and malformed files fail loading. The built-in
`reviewer` definition remains available and cannot be shadowed.

The TUI `/plan` and headless `ghg run --plan-only` use the same ordinary Plan-mode
turn loop. `ghg run --plan` emits the Markdown proposal and then executes it with
the `fast` role. `--plan-only` never starts an executor and does not create a
session. A missing proposal block is reported with a bounded response preview;
there is no autonomous replan loop.

Headless JSON output includes `model_call_start` and `model_call_end` events for
planning, acting, and compaction calls. Each event identifies the role, provider,
wire adapter protocol, model, latency, finish reason, and provider-reported usage.
Compaction reports the configured `tiny` route rather than the primary route;
title generation, `/goal-from-context`, and declarative one-shot calls use the
same route-aware telemetry wrapper.

## Conversation time travel

`internal/tui/rewind.go` — **double-esc while idle** opens the rewind picker:
the conversation's authored user messages, newest first, with the transcript
**live-scrolling** to the selected message as you browse (opencode's
`dialog-timeline.tsx` `onMove`, and `msgBlock` maps conversation index →
transcript block so the jump is direct). enter rewinds to just before the
selected message: `Agent.Messages` is truncated, the clipped tail becomes an
in-memory **redo stack** (`m.future`, oldest first), the DB rows are deleted
(`Store.DeleteFrom`), the transcript is rebuilt via `seedTranscript`, and the
rewound message's text lands back in the input for editing (opencode's undo:
"the input restore is what makes it feel good"). Cuts sit at user-message
indices, so a tool_call is never orphaned from its results.

**Forward travel:** reopening the picker while rewound lists the clipped
messages dimmed, marked `(rewound)`; enter on one pulls the tail back in and
re-saves it. Submitting new input while rewound discards the redo stack.
Compaction also drops it (a stale redo would resurrect summarized history).
esc cancels and restores the scroll position. The redo stack is in-memory
only by design: quitting while rewound leaves the DB at the rewound point.

`internal/tui/fork.go` — **`/fork [name]`** copies the conversation into a
**new** session (one `INSERT…SELECT` in `Store.Fork`; the original is
untouched and stays under `/resume`) and switches to it. Bare `/fork` opens an
inline name prompt prefilled with `<title> (fork #N)` (`Store.ForkTitle`
increments past existing forks and unwraps nested suffixes, opencode's
`getForkedTitle`). **`f` in the rewind picker** forks from the selected
message instead — one picker, two destinations. Forking while rewound pulls
the redo stack up to the picked point into the copy. **`/rename [title]`**
retitles the current session (`Store.SetTitle`); bare opens the same inline
prompt prefilled with the current title. Both prompts stash and restore any
in-progress draft. All three refuse to run mid-turn. settings entries:
"Rewind conversation", "Fork session", "Rename session" under Session.

Tests: `rewind_test.go` — double-esc opens/cancels, busy esc still
interrupts, truncation + input restore + DB rows deleted, forward travel,
partial-rewind DB prefix, tool-call-pair safety, stale esc-arm across modal
dismiss, draft preservation, resume-after-rewind. `fork_test.go` (session) —
prefix/full copy, fork-title numbering, rename, DeleteFrom. `fork_test.go`
(tui) — fork with arg, bare prompt suggestion + cancel, fork from the picker,
fork while rewound into the redo stack, rename both paths.

## MCP

`internal/mcp/` — ghg is an MCP client (stdio + streamable HTTP) and, via
`ghg mcp serve`, an MCP server. Three sources of server config merge with
ghg's own on top (per-name, whole entry): a project `.mcp.json`
(claude-style: `{"mcpServers": {name: {type, command, args, env, url,
headers}}}`), `~/.codex/config.toml` `[mcp_servers.*]` (codex-style), and the
`"mcp"` block in `~/.ghg/config.json`. Claude `type: sse` imports as
disabled-with-note (legacy transport); `${VAR}` references in env/headers
expand from ghg's environment.

- **Manager** (`manager.go`) — one lifecycle goroutine per server; a
  `ready chan struct{}` closes once on first settle (the BackgroundTask
  close-to-broadcast pattern), so tool calls block only on *their* server and
  startup never waits. Statuses: connecting → ready/failed (plus disabled);
  a dropped session flips to failed via a generation-guarded watcher
  (opencode's client-identity check, `mcp/index.ts:443`). Connect/list bounded
  by `startupTimeout` (default 30s — opencode's DEFAULT_TIMEOUT).
- **Tool bridge** — listed tools become agent tools named
  `mcp__<server>__<tool>` (claude-code convention; double underscores keep
  the split unambiguous since tool names contain `_`). Unsafe server-name
  chars get an fnv hash suffix so sanitized names can't collide (an opencode
  weakness). Calls serialize per server (1-cap channel — many stdio servers
  are single-request), run under `toolTimeout` (default 60s), and respect
  ctrl+c via ctx. Results flatten to text: images/audio/binary resources →
  placeholders, `structuredContent` → JSON when there's no text, `IsError` →
  `"Error: …"` fed back to the model — a broken MCP tool never kills a turn.
  Output capped at the shared 50KB truncation. MCP tools take no file locks
  and run in parallel with everything.
- **Late arrivals** — `Manager.SetOnChange` pushes refreshed tool sets into
  `Agent.SetMCPTools` (mutex-guarded; a settle mid-turn can't race the slice
  a request reads), so a server connecting after turn 1 appears without a
  restart.
- **TUI** — `/mcp` shows the status table (`● N tools` / `✗ err` /
  `○ disabled` / `◌ connecting…`); `/mcp <name> reconnect|enable|disable`
  reconnects live or persists a toggle through the guarded `Config.Save`.
- **CLI** — `ghg mcp list` (merged view with per-name source labels —
  `ghg config` / `.mcp.json` / `codex config` — and a `blocked` state),
  `ghg mcp add <name> -- <cmd...>` / `--url`, `ghg mcp remove`,
  `ghg mcp import [--dry-run]` (materializes imported servers into ghg's
  config; `--dry-run` prints the JSONC fragment without writing; blocked
  servers are never imported). `ghg mcp serve` (`serve.go`) exposes ghg's
  read/bash/edit/write as an MCP stdio server for other harnesses.
- **Import gating** — the `"mcpImport"` block in `~/.ghg/config.json`
  (`{"claude": …, "codex": …}` per source: `enabled`, `only` allowlist,
  `exclude` denylist — exclude wins over only; absent block imports both
  sources, the pre-gating behavior). Filtered-out imports land in the
  discovery result's `Blocked` map as disabled+noted copies
  (`LoadMergedFiltered`), stay visible in `/mcp` and `ghg mcp list`
  (`○ disabled — blocked by mcpImport config`), and `/mcp <name> enable` on a
  blocked name refuses with a pointer at the config instead of silently
  shadowing. This is the fix for third-party apps writing MCP entries into
  `~/.codex/config.toml` (e.g. the ChatGPT desktop app's `node_repl`) that
  ghg would otherwise pick up wholesale.
- **Shutdown** — `Manager.Close()` runs before `bashrun.KillAll()`; stdio
  children spawn in their own process group, and the SDK terminates them
  (stdin close → SIGTERM → SIGKILL after 3s).

Polish (the "never stuck, always know why" pass):

- **Fail-fast calls** — a call to a failed/disabled server returns instantly
  with an actionable message (`/mcp <name> reconnect|enable`); a
  still-connecting server caps the wait at a 5s grace then returns "retry in
  a moment". No turn parks on a 30s startup timeout.
- **Did-you-mean** — `tools.Suggester` (installed by `Agent.SetMCPTools`)
  runs an early-exit Levenshtein over live tool names, so a stale/typo'd
  `mcp__` call gets `did you mean mcp__docs__greet?` instead of a dead end.
- **First-settle notes** — each server's first settle lands one transcript
  line (`⚡ mcp: docs ready (4 tools)` / `✗ mcp: x failed: …`); later
  transitions stay quiet.
- **Auto-reconnect** — a dropped session retries in the background with
  backoff (1s/2s/4s, cap 3), guarded against close/disable/dupes; manual
  `/mcp reconnect` stays unlimited.
- **Server instructions** — initialize-result instructions render into an
  `<mcp_instructions>` block appended to the system prompt every turn
  (alongside skills), tracking live sessions.
- **`ghg mcp test <name>`** — the doctor: connect + list + timing + tool
  names, stderr tail on failure, non-zero exit — CI-checkable `.mcp.json`.

Tests: `config_test.go` (claude/codex parsing incl. a real-world codex
config, merge precedence, discovery errors, tool-name round-trips, import
policy filtering — blocked-in-`Blocked`, exclude-beats-only, ghg-name
shadow protection — and the blocked node_repl scenario at the manager
level), `manager_test.go` (connect/call, error-as-output, structured+media
flattening, dead-server degradation, reconnect, parallel calls under `-race`,
ctx cancel mid-connect), `loop_test.go` (model→MCP→model round trip against
a fake provider; stale def on a dead server returns `"Error: …"` and the turn
completes), `selfhost_test.go` (`ghg mcp serve` end-to-end, gated on
`GHG_TEST_SELFHOST=1`), `tui/mcp_test.go` (status view incl. blocked rows,
toggle persistence round-trip, enable-on-blocked refusal),
`config/config_test.go` (mcpImport JSONC round-trip, clobber recovery
preserving the block), `cmd/ghg/mcp_import_test.go` (import dry-run vs
apply, idempotence, blocked servers never imported).

## Process safety

`internal/tools/bashrun/bashrun.go` — every command the agent runs is tracked
in a process registry (`track`/`untrack`). On exit (`tui.Run` returning — quit,
`/quit`, or a signal), `KillAll()` SIGKILLs every tracked **process group** and
waits briefly for reaping, so an agent-started server or watcher never outlives
ghg.

The non-interactive path captures via explicit `StdoutPipe`/`StderrPipe` and
closes the read ends the moment the process exits, so a detached grandchild
(`nohup`, `sleep 30 &`, a daemonized server) holding the write end can't hang
the agent on pipe EOF. The interactive path runs in a PTY for sudo/ssh-style
prompts, killed after 15s of no input.

Tests: `killall_test.go` — `TestKillAllReapsChildren` (kills a live `sleep 60`),
`TestBackgroundGrandchildDoesNotHang`.

## Execution policy and sandboxing

`internal/tools.ToolRuntime` is the per-agent execution boundary shared by native file/search
tools, Bash, local MCP stdio servers, LSP, TUI agents, headless runs, and delegated agents.
Canonical workspace/read/write roots reject symlink and nonexistent-target escapes; `.git`,
`.ghg`, and configured ghg state remain protected from native writes. Child processes receive
only a minimal runtime environment, with provider keys and secret-resolver inputs removed from
Bash/LSP environments.

Restricted subprocesses use macOS `/usr/bin/sandbox-exec` or Linux bubblewrap. Linux starts from
an empty mount root and trusted bubblewrap discovery rejects user-writable executables. The
modes are `read-only`, `workspace-write` (default), and explicit `danger-full-access`; network
is `deny` by default or explicitly `host`. Missing or untrusted backends fail closed and their
status plus recent denials appear in `/context-doctor`. Local MCP and LSP processes use the same
boundary, with private temp and canonical build/package caches injected into children.

Exceptional command approvals are separate from containment. Simple commands retain useful
arity rules, but compound commands are fully classified and stored as exact normalized rules.
Path-aware destructive removal keeps broad targets hard-denied and gives external roots only to
the active human-approved call. `ask` uses the human prompt, `never` denies escalation, and opt-in `auto-review` calls the configured `tiny` role
once with no tools; the reviewer cannot approve broad/destructive, privileged, credential,
policy, external-root, protected-metadata, global-install, persistent, or opaque shell
operations. Explicit shell redirections outside configured roots and Git metadata writes use
an exact, human-only one-shot grant; the grant is applied only to that command.
Approval requests, reviewer telemetry, and audit history structurally redact secret-bearing
flags, assignments, headers, URLs, and configured secret-name patterns; opaque shell syntax is
represented by a fingerprint rather than retained verbatim.

The CLI accepts one-shot `--sandbox`, `--network`, and `--approval` overrides. Headless runs
fail closed unless `--approval auto-review` (or the equivalent trusted execution config) is
explicitly selected. See [the Phase 3 implementation plan](../.ai-docs/plans/phase-3-execution-policy/README.md).

## LSP diagnostics

`internal/lsp/` — a stdlib-only LSP client over stdio (JSON-RPC +
`Content-Length` framing; no new dependencies) that feeds language-server
diagnostics back into the model's `write`/`edit` tool results, so the model
sees and fixes breakage in the same turn instead of spending a `go build`
round-trip. Ported from opencode's diagnostics flow
(`packages/opencode/src/lsp/`, research in
`docs/learnings/other-harnesses/opencode/lsp.md`) with two widenings:
sibling-file errors (opencode renders only the touched file) and wait-free
wakeup (a per-file channel close instead of polling timeouts).

- **Tool output** — after a successful `write`/`edit`, the tool result gains
  a `<diagnostics file="…">ERROR [l:c] msg</diagnostics>` block (format
  ported verbatim from opencode's `lsp/diagnostic.ts`): errors+warnings for
  the edited file (max 20), errors-only for up to 5 sibling files in the
  same directory, with a "this edit introduced errors in other files" note.
  Injection is via the per-run `ToolRuntime.LanguageService`; nil service = unchanged output.
- **Manager** (`manager.go`) — the registry is data: `gopls` built-in (root =
  nearest `go.work`/`go.mod`/`go.sum`, found by walking up from the file);
  the `"lsp"` block in `~/.ghg/config.json` (same shape as the `mcp`
  block: `command`, `extensions`, `rootMarkers`, `env`, `enabled`) adds
  servers or disables the built-in. Servers spawn lazily on first covered
  file touch; concurrent touches dedup through a close-to-broadcast channel,
  failed spawns (binary not on PATH, initialize error) are remembered per
  (server, root) so a broken server is a permanent no-op, never a retry
  storm. The wait for diagnostics is capped at 1.5s and honors the tool
  call's ctx (ctrl+c cancels); timeout = no block appended, the tool result
  is never delayed further or failed.
- **Client** (`client.go`) — one reader goroutine parses frames and routes
  responses by id into cap-1 pending channels; writes funnel through a
  buffered channel drained by one writer goroutine (no locks). Frames are
  capped before allocation. Supported server requests receive typed responses:
  configuration gets `[]`, progress/capability registration gets `null`, and
  `workspace/applyEdit` is explicitly rejected in favor of `lsp_rename`.
  Unknown requests receive JSON-RPC method-not-found. Shutdown is polite
  `shutdown`/`exit` then SIGKILL of the process group; `Manager.Close()` runs
  next to `mcpMgr.Close()` on exit.
- **TUI** — `/lsp` prints per-server rows (`● connected (root: …)` /
  `○ not started` / `✗ err`); the manager is built in the same startup block
  as MCP and installed on the shared `ToolRuntime`.

Tests: `internal/lsp/client_test.go` (frame parsing incl. split/garbage,
request routing, ctx-cancel on unanswered requests, server-request acks),
`manager_test.go` (in-process fake LSP server over pipes — no real gopls:
edited-file blocks, sibling blocks, didOpen→didChange versioning, timeout,
cancel, broken-spawn caching, config merge, root walk),
`concurrency_test.go` (spawn dedup across 8 concurrent touches, parallel
waiter wake with goroutine-leak check, publish-before-wait interleaving),
`internal/tools/lsp_test.go` (block appended to write/edit output, nil hook,
failure never fails the tool), `internal/agent/lsp_test.go`
(`TestLSPDiagnosticsReachModel`: fake provider receives the diagnostics
block in the tool result on the next call), `internal/tui/lsp_test.go`
(`/lsp` status view).

Out of scope (breadcrumbs in `.ai-docs/plans/lsp-diagnostics/README.md`):
@-mention symbol-range expansion (Linear INF-4991), pull diagnostics, and
auto-installing servers.

## Skills

`internal/skills/skills.go` — scans `.agents/skills/*/SKILL.md` (project) and
`~/.ghg/skills/` (user) for a name+description frontmatter block, injected
into the system prompt as an `<available_skills>` catalog in the Agent Skills
spec format (`<skill><name>/<description>/<location>`, XML-escaped). The model
reads a SKILL.md with its own read tool when relevant. Skills re-index every
turn, so new ones load without restarting.

**Spec compliance** (agentskills.io, matching pi's `core/skills.ts`): name
validated (≤64 chars, lowercase a-z/0-9/hyphens, no leading/trailing/double
hyphens), description ≤1024 chars (a *validity* ceiling, not a prompt budget),
`disable-model-invocation: true` skills excluded from the catalog but still
invocable via `$name`. Violations load with a `Warning` (surfaced in the
startup report), never silently disappear. The header shows the number of
successfully loaded skills. Tests: `skills/spec_test.go`.

**`/context-doctor` (alias `/context-doctor`)** — fresh-session context audit: every
automatic injection source with its estimated token cost (base system prompt,
trusted `AGENTS.md` project instructions, skills block with the 5 biggest
offenders, per-server MCP tool schemas, server instructions, built-in tool
schemas, conversation history, and actual session spend once requests have run),
a TOTAL line, and trim pointers. Built for
users arriving from heavier harnesses whose first call silently carries tens
of thousands of tokens of skill/MCP bloat. Tests: `tui/context_doctor_test.go`.

**`/report`** — bug-report bundle for terminal/rendering issues: one
transcript block pairing a clickable OSC 8 link (opens a prefilled
`sacca97/ghg` issue with a What-happened/Expected skeleton + the
environment bundle in a fenced block) with the same bundle as a
copy-pastable fenced snippet. Strict env whitelist (ghg version/model/
provider, theme + *how it was detected* — captured at startup, never
re-queried, mouse, session id; TERM/TERM_PROGRAM/COLORTERM/COLORFGBG, tmux +
`tmux -V`, SHELL, locale, window size, ssh flag; OS/arch, uname, sw_vers, Go
version) — no secrets, no conversation content. Nothing is submitted or
persisted: the user clicks or pastes. Version is plumbed from `main.version`
via `tui.Version`. Tests: `tui/report_cmd_test.go` (whitelist, no-secret
leak, issue URL round-trip, fenced snippet, busy-safe).

**Startup resource report** — the header names the successfully loaded skills:
`skills: N loaded`. The startup report keeps one `⚠` line per degraded skill
(description over maxDesc → truncated in the prompt) or unparseable SKILL.md
(pi's [Skill conflicts] lesson — a broken skill is never silent), and one
`mcp:` line with per-server status glyphs (`✓ N tools` / `✗` / `○ disabled` /
`◌ connecting`). Skipped on resume.
Tests: `tui/startup_report_test.go` (warnings, MCP glyphs, silence when empty).

The repository ships a small project-focused catalog: Bubble Tea, Go benchmarking,
CLI, concurrency, context, SQLite, gopls, security, testing, new-feature development,
and the ponytail minimalism/review workflows. Generic Go references and personal
output preferences normally belong in `~/.ghg/skills/`; `i-have-adhd` is retained as
an explicit-only project skill because it is part of this repository's dogfooding set.
It and `ponytail-review` remain invocable as `$i-have-adhd` and `$ponytail-review`
without taxing the automatic catalog.

## Scope note

Browser automation and desktop computer-use are intentionally outside this
fork's initial scope. The MCP boundary remains the extension point for
specialized integrations, while the core agent, session, TUI, LSP, skills,
memory, scheduling, and built-in file/shell tools remain part of ghg.
