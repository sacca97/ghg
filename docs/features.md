# Features

ghg is a minimal coding agent: an interactive bubbletea TUI driving an
LLM tool-use loop (bash / read / write / edit / grep / glob / task) with provider-routable
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
path: send to acquire, receive to release). Two edits to `foo.go` can't
interleave; edits to different files run truly in parallel. `bash` takes a
global lock because a command's side effects aren't attributable to one path.
Reads don't lock.

This is the Go-native port of pi's `withFileMutationQueue` (per-path promise
chains in TypeScript). In Go the lock is a buffered channel — no explicit
unlock bookkeeping.

Tests: `parallel_test.go` — `TestToolCallsRunInParallel` (overlap measured via
a concurrency counter), `TestSamePathEditsSerialize`, `TestToolMutationPath`,
`TestCanonicalPathKey`.

### Native grep and glob

`internal/tools/search.go` provides read-only native `grep` and `glob` tools.
`grep` searches text files with a regular expression (or `literal: true`) and
returns stable `path:line:match` rows; `include` applies a slash-aware glob
filter and `case_sensitive: false` enables case-insensitive matching. `glob`
returns regular files matching a relative pattern, with `**` spanning path
separators. A pattern without a slash stays at the selected directory level;
`**` makes recursion explicit.

Both tools default to the current working directory and accept an explicitly
selected existing file or directory. Directory searches use Go's `os.Root`,
never follow symlink entries, skip `.git` and non-regular files, and return absolute
paths only when the selected root is outside the working directory. Explicit
files are searched directly even when their parent directory is ignored.

`internal/tools/ignore.go` loads nested `.gitignore` files in ancestor order and
supports negation, leading-slash anchoring, basename patterns, and directory-only
rules. An ignored parent prevents a child negation from leaking files back into
the result; negating the directory itself reopens its subtree. Binary files are
skipped. Results are bounded by the shared 50,000-byte tool budget, a default
1,000-result limit (maximum 10,000), and a 100,000-entry scan limit, with an
explicit marker when a limit stops the search. All filesystem walks honor the
call context.

Tests: `internal/tools/search_test.go` — `TestGrepTool`,
`TestGlobToolPatternsAndOrdering`, `TestGitignoreRules`,
`TestSearchLimitsCancellationAndInvalidArguments`,
`TestMalformedGitignore`, and `TestExplicitIgnoredFileIsSearchable`.

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
  CompletionTokens`) crosses the compaction threshold — a percent of the
  advertised context window, default 50% (`compactPct` in config, clamped
  10–90; `Agent.CompactThreshold` holds the fraction). The value is zero until
  the first successful response. Slide it in the settings's "Compaction level"
  row (←/→ steps ±10%).
- **Reactive**: if the provider still rejects a request with a context-limit
  error (`context_length_exceeded`, `prompt_too_long`, HTTP 413), `Turn`
  compacts once and retries. A `compacted` guard prevents retry loops.

`compact()` keeps the system prompt and a recent tail, and is **orphan-safe**:
a kept tail that begins with a `tool`-role message walks back to its owning
assistant message so no tool result references an erased call ID. The summary
runs as a non-streaming `Complete` on the configured `tiny` role when a roles
block is present. A legacy config without roles uses the built-in
`deepseek-v4-flash-0731` (`config.DefaultCompactModel`). An explicit
`compactModel` / `compactProvider` remains the per-session override, and an
unavailable fallback leaves compaction on the conversation's own model.

Token bookkeeping: `llm.Usage` (prompt/completion/cached) is read off the
terminal stream chunk (`stream_options: include_usage`) and folded into session
totals via `AddUsage`. The bottom status box shows the latest successful
assistant request's prompt plus completion tokens; it starts at zero. Compaction
and subagent calls count toward the cumulative session totals too.

Commands: `/compact` (compact now), `/compact <model> [provider]` (pick the
summarizer), `/compact off` (restore the configured `tiny` role, or the legacy
built-in default). The settings's
"Compaction model" panel lists every configured model behind a
"default (…)" row that restores the default; "Compaction level" steps the
threshold ←/→.

Tests: `agent_test.go` — `TestTurnAutoCompactsOnContextLimit`,
`TestCompactDoesNotLoopOnRepeatedContextLimit`, `TestCompactKeepsToolCallPair`,
`TestProactiveCompactAtFiftyPercent`, `TestCompactThresholdExplicitOverride`,
`TestUsageAccumulates`; `compact_cmd_test.go` —
`TestCompactModelEmptyResolvesDefault`, `TestCompactModelDefaultFallsBack`,
`TestCompactThresholdFor`, `TestSetCompactPct`; `palette_test.go` —
`TestPaletteCompactPanelAppliesInPlace`,
`TestPaletteCompactPanelDefaultRowRestores`, `TestPaletteCompactionLevelSteps`.

### Recoverable tool-result artifacts

`internal/tools/result.go`, `internal/artifact/`, `internal/session/` — tool
execution has a structured result path. Every result keeps a model-sized
`Preview`, bounded retained evidence, original/stored byte counts, completion
state, exit code, and source metadata. Bash, file reads, native search, MCP,
and artifact reads mark returned bytes as untrusted; the agent wraps those
bytes in one `<untrusted_tool_output>` block before sending them to a
provider. Direct legacy `tools.Execute` callers still receive the old plain
preview.

Retained evidence is capped at 10 MiB per result by default. Overflow keeps a
deterministic head/tail, hashes the retained bytes with SHA-256, and appends a
path-free recovery hint. Persistent runs store payloads under
`~/.ghg/artifacts/sha256/<prefix>/<hash>` with private directory/file
permissions and index references in `sessions.db`; `--no-session` uses a
private temporary store removed on exit. Set `{"artifacts":{"enabled":false}}`
to opt out; bounded previews remain available and explicitly say omitted data
is unrecoverable. `maxBytes` changes the per-result retention ceiling.

The agent exposes `artifact_list` and `artifact_read` as session-scoped,
read-only operations. Listing is metadata-only and bounded; reading accepts
an artifact id plus a bounded byte range, never a path or another session id.
`ghg artifacts gc --max-age … --max-bytes N` removes only unreferenced
payloads, so forks can share immutable content safely. Compaction preserves
the raw message log, keeps atomic tool-call groups, carries a metadata-only
artifact manifest for cited/recent references, and shrinks an oversized
recent result without dropping its recovery id.

Tests: `internal/artifact/store_test.go`, `internal/tools/result_test.go`,
`internal/agent/artifacts_test.go`, `internal/session/artifact_test.go`, and
`cmd/ghg/artifacts_test.go`.

### Background subagents

`internal/agent/background.go` — `task` with `background: true` launches a
subagent that runs **concurrently with the parent** instead of blocking the
turn. This is the channel-native port of opencode's `background-job.ts`
registry.

Each task is a `BackgroundTask` with a `Done chan struct{}`. When the subagent
settles, the registry `settle()`s and **closes `Done` once** — closing a
channel broadcasts to every waiter at once, so the tool caller, the TUI, and
`/tasks` all wake together with no per-waiter state (opencode needs a per-job
`Deferred` for the same thing). On settle the report fans back into the parent
as a **steered message**, so the model sees it on the next loop boundary.

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
`TestResumeRestoresTasks`, `TestTaskPersistsOnStartAndSettle`;
dock click hit-testing: `TestDockClickOpensClickedRow`,
`TestDockClickIgnoredWhilePaletteOpen`.

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
advertised), output at the completion rate — `llm.SessionCost`. Providers
without pricing hide the segment entirely. Tests: `llm/openai_test.go`
(`TestSessionCost`, pricing unmarshal), `config/catalog_test.go`,
`tui/status_test.go` (`TestStatusLineShowsCost`, `TestStatusLineHidesCostWithoutPricing`).

`internal/llm/backend.go` — the agent-facing `Backend` contract is deliberately
smaller than a provider client: `Stream` accepts a request-local `EventSink`
and returns the assembled assistant `Message` plus usage; `Complete` returns a
message plus usage for one-shot work such as compaction. `OpenAIBackend`,
`OpenAIResponsesBackend`, and `AnthropicBackend` adapt their respective wire
clients, while `NewBackend` selects the compiled adapter from the provider protocol
(`openai-completions` remains a compatible legacy spelling). Retry callbacks
supplied by a turn stay in the request-local sink, so foreground and background
subagents can share a backend without mutating a client hook. `CatalogBackend`
is an optional capability: a configured local endpoint can work without
implementing `/models`.

`internal/provider/profile.go` — declarative provider profiles are strict YAML
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
Tests: `provider/profile_test.go`, `llm/backend_test.go`, `llm/responses_test.go`,
`llm/anthropic_test.go`.

### Model roles

`internal/config/roles.go` and the existing TUI/CLI builders provide four model
roles: `default`, `smart`, `fast`, and `tiny`. A role resolves to its configured
model/provider, then the configured `default` role, then legacy
`defaultModel/defaultProvider`; an explicitly configured invalid route is an
error. Acting sessions default to `fast`, planning sessions to `smart`, while
compaction and foreground/background `task` calls use `tiny`. The TUI bottom
status bar exposes clickable `execute`/`plan` modes (`plan` maps to `smart`) and
a role-first model settings flow (`default`, `plan`, `fast`, `tiny` → one-line
`provider/model` routes). Routes from providers without a configured
credential are omitted. `ghg run --role` selects a role for a headless run, and
explicit model/provider flags remain route overrides. Tests:
`config/roles_test.go`, `tui/provider_route_test.go`, `tui/mode_test.go`,
`agent/agent_test.go` (`TestTaskUsesTinyRoleFactory`), and
`tui/compact_cmd_test.go` (`TestCompactModelUsesTinyRole`).

`internal/llm/openai.go` — the streaming client. Typed `HTTPError` (keeps the
`<status>: <body>` shape), `IsContextLimit()` classifies context-overflow
errors for the compaction retry, `Stream` returns the message + usage, and
`Complete` is the non-streaming round-trip used by compaction.

`internal/llm/anthropic.go` — the native Anthropic Messages adapter. It maps
top-level system prompts, multimodal content, tools and grouped tool results,
preserves signed thinking blocks for follow-up turns, assembles fragmented
SSE events, applies stable-prefix prompt-cache breakpoints, maps Anthropic
usage/model metadata, and shares the existing retry/cancellation/error
boundaries.

Transient request failures — 429, any 5xx (e.g. a gateway's 524), and
transport errors — retry with exponential backoff (1s→2s→4s… capped 20s,
+25% jitter, ctx-cancellable). Budget: `maxRetries` in config (default
`llm.DefaultMaxAttempts` = 8, `1` disables). A streaming attempt is only
retried before the first visible delta reaches the UI — after that a retry
would replay rendered text, so the error surfaces instead. Mid-stream
provider `error` chunks and 4xxs (including context-limit, which the
compaction path must see immediately) are never retried. Each retry posts a
`⚠ request failed (…) — retrying in Ns (attempt N/M)` line via the
request-local backend event sink (the legacy `Client.OnRetry` hook remains for
direct client users). Tests: `llm/retry_test.go`, `llm/backend_test.go`.

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
  `@file` mentions, `$skill` invocation, `/goal` loop, `/resume` session
  picker, `--continue`, `/effort` reasoning levels — see the roadmap for the
  full list.
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
  the goal loop. The transcript note states the exact window used. Prompt
  building is pure (`agent.BuildGoalFromContextPrompt` over the window from
  `agent.GoalFromContextMessages`); the TUI command mirrors `/compact`'s
  goroutine + `goalFromContextMsg` pattern, refusing while busy and running
  inline when headless. Tests: `goal_test.go` (`TestGoalFromContext*`).
- **`/plan <goal>` / `/execute [plan]`** are an explicit two-step workflow. `/plan`
  runs the built-in declarative planner on the `smart` role. It may inspect the
  repository with only `read`, `grep`, and `glob`, then must finish with the
  structured `submit_plan` tool; invalid or incomplete proposals are retried once
  and displayed without starting an acting turn. `/execute` runs that proposal, or
  supplied plain text/JSON, through the `fast` role; structured plans seed the
  existing `todowrite` checklist first. The planner is never invoked for ordinary
  chat. The command settings is bounded to the terminal height and scrolls with
  ↑/↓ or the mouse wheel. The bottom status box's separate `(effort)` control
  cycles through off and the available effort levels.
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
definitions; unknown tools and malformed files fail loading. The reserved built-in
`planner` definition is always available and cannot be shadowed.

The TUI `/plan` and headless `ghg run --plan-only` use that same planner runner.
`ghg run --plan` emits the structured proposal and then executes it explicitly with
the `fast` role. `--plan-only` never starts an executor and does not create a
session. There is no autonomous replan loop.

Headless JSON output includes `model_call_start` and `model_call_end` events for
planning, acting, and compaction calls. Each event identifies the role, provider,
wire adapter protocol, model, latency, finish reason, and provider-reported usage.

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
  Injection is via the package hook `tools.LSP` (same pattern as
  `tools.InteractiveBash`); nil hook = unchanged output.
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
  buffered channel drained by one writer goroutine (no locks). Server→client
  requests (`window/workDoneProgress/create`, `workspace/configuration`,
  `client/registerCapability`) get a null-result ack, same as opencode.
  Shutdown is polite `shutdown`/`exit` then SIGKILL of the process group;
  `Manager.Close()` runs next to `mcpMgr.Close()` on exit.
- **TUI** — `/lsp` prints per-server rows (`● connected (root: …)` /
  `○ not started` / `✗ err`); the manager is built in the same startup block
  as MCP and installed on `tools.LSP`.

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
@-mention symbol-range expansion (Linear INF-4991), read warm-up
(opencode forks `touchFile` on read — cut; revisit if first-edit latency
annoys), pull diagnostics, navigation tools (definition/references/hover),
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

Installed: the `golang-*` skill set plus `i-have-adhd` (output-shaping for ADHD
readers; invoke with `/i-have-adhd`, off with "stop adhd mode").

## Scope note

Browser automation and desktop computer-use are intentionally outside this
fork's initial scope. The MCP boundary remains the extension point for
specialized integrations, while the core agent, session, TUI, LSP, skills,
memory, scheduling, and built-in file/shell tools remain part of ghg.
