# Phase 3: LSP navigation, safe rename, and post-edit hooks

Status: implemented. This expands the two Phase 3 sections in `plan.md` and records
the delivered LSP navigation/rename and post-edit hook scope.

## Goal

Complete the code-intelligence edit loop with the least new machinery:

1. expose bounded LSP definition, references, document-symbol, and hover navigation;
2. make LSP rename a previewed, session-bound, stale-checked atomic mutation;
3. run configured argv-style post-edit hooks after successful publication and before
   final readback and diagnostics; and
4. make the same language service and hook runtime available in the TUI, `ghg run`,
   planners where read-only navigation is allowed, and delegated agents.

The user-visible mutation order is:

`authorize + lock -> publish -> postEdit hooks -> reread final bytes -> diff/readback -> diagnostics`

## Non-goals

- No `internal/wire` extraction until DAP creates a second framing consumer.
- No completion, formatting, code actions, call hierarchy, pull diagnostics, semantic
  tokens, or server auto-installation.
- No server-initiated `workspace/applyEdit`; ghg owns preview and application.
- No project-local hook files, shell interpolation, `preTool`, or `postTurn` hooks.
- No durable rename-preview table. A restart invalidates outstanding preview IDs and the
  model must preview again.
- No new LSP/protocol, diff, file-locking, process-runner, or test dependency.
- No generic plugin or event framework for hooks.

## Acceptance criteria

- `lsp` exposes only `definition`, `references`, `document_symbol`, and `hover`, returns
  bounded normalized results, and works identically in TUI and headless runs.
- The built-in planner can use `lsp`; it cannot use `lsp_rename`.
- `lsp_rename preview` accepts one source position and new name, validates the complete
  `WorkspaceEdit`, and returns a bounded diff plus a short opaque preview ID.
- `lsp_rename apply` uses exactly the stored bytes behind that ID. It never recomputes
  the rename, rejects a different session, stale bytes/version, unsupported edit shape,
  outside-workspace path, overlap, or failed authorization before publishing anything.
- Rename publication reuses the existing sorted staging/rename/rollback implementation.
  A partial publication either rolls back or reports the existing explicit rollback
  failure form.
- UTF-16 positions are negotiated explicitly and converted at exact rune boundaries;
  malformed positions and surrogate-splitting offsets fail closed.
- A configured hook runs once per matching successful mutation batch, under the shared
  sandbox/runtime, with sorted canonical paths appended as argv.
- Hook timeout/non-zero exit never rolls back or changes the successful tool exit status;
  bounded output and the final on-disk target bytes are reported honestly.
- With no hooks configured and with navigation unused, current write/edit/diagnostic
  behavior is unchanged.
- LSP servers and hook subprocesses receive the existing secret-filtered child
  environment and OS sandbox. No package-global LSP callback remains.

## Out-of-scope files

Do not modify provider adapters, artifact/session schemas, MCP protocol code, memory,
skills, web tools, DAP, the sandbox policy model, or provider configuration. Session code
should not change: rename previews are intentionally same-process capabilities.

## Existing seams to reuse

- `internal/lsp/client.go` already has cancellable JSON-RPC request/notification routing
  and `Content-Length` framing.
- `internal/lsp/manager.go` already owns lazy spawn deduplication, document versions,
  diagnostics, lifecycle shutdown, and the restricted `ToolRuntime` used by LSP child
  processes.
- `internal/tools.ToolRuntime` is inherited by subagents and already wraps non-Bash
  subprocesses with the configured sandbox and secret-free environment.
- `internal/agent/filelocks.go` already provides sorted per-path locks and one global
  mutation lock.
- `internal/tools/edit.go` already implements staged same-directory writes, deterministic
  publication order, rollback, compact diffs, and bounded readback.
- `internal/tools/result.go` already supplies common result bounds and untrusted-output
  marking.
- The existing fake LSP server tests can answer arbitrary methods; do not introduce a
  real-gopls or end-to-end test harness.

## Design decisions

### Runtime ownership, not globals

Replace the anonymous `ToolRuntime.LSP` shape with one named interface in
`internal/tools`. It should cover diagnostics, warm-up, navigation, and rename preview
lookup. Remove the package-global `tools.LSP` fallback after all constructors install the
manager on the runtime.

The manager remains one process-level object per TUI/headless invocation. `Runtime.Child`
copies the interface, so subagents share server processes and document state without a
second registry. TUI model switches reuse the same runtime. Headless code creates the
manager immediately after `NewConfiguredRuntime`, assigns it before planner/actor calls,
and defers `Close`.

The interface types live in `internal/tools` because `internal/lsp` already depends on
that package for `ToolRuntime`; importing `internal/lsp` back into tools would create a
cycle. Keep the interface narrow and operation-oriented—do not mirror the LSP protocol.

### Tool surfaces

Add two tools to `tools.All()`:

- `lsp`: read-only `operation` enum with `definition`, `references`,
  `document_symbol`, and `hover`; `path` is always required, while `line` and `column`
  are required except for `document_symbol`. Optional `include_declaration` applies only
  to references.
- `lsp_rename`: mutating `operation` enum with `preview` and `apply`. Preview requires
  `path`, `line`, `column`, and `new_name`; apply requires only `rename_id`.

Tool coordinates are one-based lines and one-based Unicode-rune columns. Convert them to
the negotiated LSP encoding before requests and convert returned ranges back for display.
This keeps model-facing coordinates aligned with bounded `read` output. Invalid line or
rune boundaries are actionable errors.

Add both names to the declarative-agent static allowlist, but add only `lsp` to the
built-in planner definition. Tool descriptions, not a second prompt block, explain the
operations. Update the main/subagent prompt inventories only where they currently list
built-ins explicitly.

### Bounded normalized navigation

Synchronize the requested file before every navigation request. Normalize protocol
responses to stable path/range/text rows:

- definition: file path and start/end range;
- references: the same range rows, sorted and deduplicated;
- document symbols: name, kind, range, and selection range, flattening nested
  `DocumentSymbol` children; also accept the older flat `SymbolInformation` response;
- hover: bounded plain/Markdown content plus its optional range.

Use deterministic hard caps rather than pagination in this slice: 20 definitions, 100
references, 200 flattened symbols, 8 KiB hover content, and the existing common tool
preview ceiling. Include an omitted-count marker when the server returns more. Reject
non-`file` result URIs; authorize every returned path for read through the runtime so
module/toolchain definitions may work only when their roots are already readable.

Mark navigation output as untrusted `lsp` data. Do not let hover Markdown or symbol text
be interpreted as harness instructions.

### Document synchronization and encoding

Refactor the `WaitDiagnostics` inline didOpen/didChange work into one manager helper that:

1. resolves/spawns the covering client;
2. reads the exact current bytes;
3. sends `didOpen` once or `didChange` only when the content hash changed;
4. increments the document version only for changed bytes; and
5. returns client, canonical path/URI, bytes, and version to diagnostics/navigation.

Store a stdlib SHA-256 content hash beside each open document version rather than a
second full file copy. Serialize the short read/version/notify section with one manager
document-sync mutex initially.

`ponytail:` a single synchronization mutex is deliberately coarser than a per-document
lock map. Upgrade only if measurements show unrelated LSP files contending; process I/O
and requests remain outside it.

During initialize, advertise only UTF-16 in `general.positionEncodings`, decode
`capabilities.positionEncoding`, and accept missing/`utf-16` (missing defaults to UTF-16
in LSP 3.17). Reject a server that explicitly selects another encoding rather than
silently corrupting edits. Retain full-text sync; parse only the capability fields needed
for these guarantees.

Add a maximum accepted LSP frame size before allocation. A local or compromised server
must not force an unbounded `Content-Length` allocation. Use one documented constant
large enough for bounded rename responses (16 MiB initially) and close the client on
overflow.

### Read warm-up

After a successful observed `read`, call the runtime language service's non-blocking
`Warm(path)`. The manager first checks extension coverage, then starts a background
bounded `syncDocument` using its own lifecycle context. It shares the existing spawn
deduplication and becomes a no-op for an already synchronized unchanged file.

Use a two-second warm-up budget. A read never waits for or fails because of warm-up, and
`Manager.Close` cancels the manager lifecycle so no warm-up survives shutdown.

`ponytail:` warm-up may time out on an unusually slow first server start; the later
foreground navigation/edit path retries because caller cancellation is not recorded in
the manager's broken-server cache. Increase the budget only from observed startup data.

### Server requests

Replace the current “all server requests receive null” branch with a small method switch:

- `workspace/applyEdit`: return `{applied:false, failureReason:"use lsp_rename preview/apply"}`;
- `workspace/configuration`: return an empty array;
- `window/workDoneProgress/create` and `client/registerCapability`: return `null`;
- unknown requests: return JSON-RPC method-not-found.

This is typed dispatch only, not a general server-request framework. In particular, the
server can never cause filesystem mutation on the client read goroutine.

## Safe rename

### Preview parsing and validation

`Manager.PreviewRename` synchronizes the source document, converts the model position to
UTF-16, issues `textDocument/rename`, and accepts the two standard text-edit containers:

- `changes: map[DocumentUri][]TextEdit`; or
- `documentChanges` containing only `TextDocumentEdit` entries.

Reject an ambiguous response containing both containers. Reject `CreateFile`,
`RenameFile`, `DeleteFile`, annotated resource operations, non-file URIs, unsupported
position encoding, paths outside the canonical workspace, symlink escapes, missing or
non-regular files, invalid ranges, split UTF-16 surrogate positions, and overlapping
edits. Unknown harmless fields on `TextEdit` may be ignored; unknown union variants fail
closed.

For every affected file:

1. canonicalize and require containment in `Policy.Workspace()` even if another external
   write root is configured;
2. read exact original bytes and mode;
3. convert all LSP ranges to byte offsets against those bytes;
4. apply edits from highest byte offset to lowest;
5. store original and updated bytes plus the manager's current document version; and
6. if `documentChanges` supplies a version, require it to match the synchronized version.

Bound one preview to 256 files, 10,000 text edits, and 32 MiB of combined original plus
updated bytes. These are trust-boundary limits, not target task sizes; return an
actionable “rename is too large” error rather than partially previewing an unsafe edit.

Render a deterministic path-sorted summary and reuse the existing compact `editDiff`
format for bounded per-file previews. The opaque ID is `rn_` plus 128 bits from
`crypto/rand`; do not encode paths or session data in it.

### Preview registry

Keep validated plans in `Manager`, keyed by `(sessionID, renameID)`, guarded by its
existing mutex. Store at most 32 outstanding plans per session and evict the oldest
unused entry when adding the next. Clear all plans on `Close`.

The ID is a same-process, same-session capability:

- another session cannot inspect or apply it;
- successful application consumes it;
- a stale/invalid plan is dropped and must be previewed again;
- authorization denial or user rejection leaves it available for a later apply; and
- resume after process restart intentionally requires a new preview.

Use the session ID already carried in the tool-call observation context. An empty ID is
acceptable only for the one manager instance of a `--no-session` headless run.

### Application, locking, and publication

The agent currently selects path locks before tool execution, but `lsp_rename apply`
reveals its paths only after resolving the opaque ID. Treat only the `apply` operation as
a global mutation in `internal/agent/filelocks.go`; preview and read-only `lsp` calls take
no mutation lock.

`ponytail:` the existing global lock is intentionally conservative. It avoids a new
resolver/lock service and makes rename application serialize with Bash and every native
mutation. Move to preview-path locks only if rename concurrency becomes a measured issue.

Under that lock, apply performs a complete preflight before staging:

1. resolve the plan for the active session without consuming it;
2. authorize every canonical path for native write and run the normal cautious gate;
3. reread every file and require exact equality with the stored original bytes;
4. require every tracked LSP version still matches (unchanged syncs do not increment it);
5. stage all updated files beside their destinations; and
6. publish in canonical lexical order with rollback on the first rename failure.

Extract only the staging/publication/rollback portion of `runObservedEdit` into a small
unexported `internal/tools` helper accepting path/original/updated/mode records. Reuse it
from observed edit and LSP rename. Do not extract a generic patch engine or new package;
the LSP manager returns an already validated neutral rename plan to the tool layer.

After successful publication, run hooks once for the complete path set, reread final
bytes, render the diff against the previewed originals, append bounded readback and
diagnostics for existing affected files, then consume the ID. Hook changes are part of
the reported final diff. If a hook deletes a target, report that fact and skip its
diagnostics without converting the already-successful rename into failure.

## Post-edit hooks

### Configuration

Add a root `postEdit` array to `config.Config`:

```jsonc
{
  "postEdit": [
    {
      "command": ["gofmt", "-w"],
      "extensions": [".go"],
      "timeoutSeconds": 10
    }
  ]
}
```

Each entry has:

- required non-empty argv `command`;
- optional normalized dot-prefixed `extensions` allowlist; empty means all changed
  files; and
- optional `timeoutSeconds`, default 10, valid range 1–60.

Validate this shape during config load with the existing config validation path. Reject
empty argv elements, NUL bytes, malformed extensions, and out-of-range timeouts. Do not
resolve executables or inspect project files at config-load time.

Convert validated config entries into immutable `tools.PostEditHook` values on the
runtime after `NewConfiguredRuntime`. Deep-copy argv/extensions in `Runtime.Child`.
Interactive runtime construction already happens after folder trust; headless mode is
already documented as trusted automation, so no second trust flag is needed.

### Execution

Add one runtime method that receives the complete canonical changed-path batch. For each
configured hook in config order:

1. filter the sorted path list by extension and skip the hook if none match;
2. append the matching paths to a cloned configured argv;
3. execute directly—never through a shell—with cwd set to `Policy.Workspace()`;
4. use `ChildEnv(nil)` and `WrapCommand`, so secrets, mounts, network, and sandbox mode
   match LSP/Bash policy;
5. cap it with the smaller of the hook timeout and caller context;
6. own a process group and kill the group on timeout/cancellation; and
7. capture bounded stdout and stderr separately, retaining the tail where formatter
   failures normally appear.

Use a tiny concurrency-safe bounded writer local to `hooks.go`; do not extend Bash's
shell runner or export MCP's private ring buffer merely to run argv. Cap each stream at
8 KiB and include omitted-byte counts.

Return structured hook reports to the mutation caller. Successful silent hooks add no
noise. Non-zero, timeout, cancellation, wrap/spawn failure, or non-empty output produces
one bounded note naming the configured executable and status. Redact the note with the
runtime before including it in tool output.

Hooks are trusted configured policy, not model-selected commands, so they do not invoke
the approval reviewer. The existing sandbox still denies network and paths outside its
policy. A hook failure never retries with broader permissions.

### Mutation integration

Call the hook runner only after successful publication:

- `write`: one path;
- exact `edit`: one path;
- observed multi-file `edit`: one invocation per configured hook with the entire sorted
  path batch; and
- successful LSP rename apply: the complete renamed path batch.

Do not run hooks after validation/staging/publication failure. Keep the agent's existing
mutation lock held through hooks, final reread, and diagnostics—the lock already wraps
the entire tool call. For observed edit and rename, construct diffs/readback from the
post-hook bytes, not the staged bytes. When no hook matches, preserve the current output
shape byte-for-byte where practical.

## Ordered implementation slices

### 1. Baseline and runtime parity

- Run the existing `internal/lsp`, `internal/tools`, and `internal/agent` tests first.
- Introduce the named language-service interface.
- Construct/install/close the manager in `cmd/ghg/run.go` as the TUI already does.
- Remove `tools.LSP`; update direct tests to attach a stub through `ToolRuntime`.
- Confirm runtime children retain the language service and later hook configuration.

Gate: existing diagnostics tests pass without a package global, and a focused headless
agent test sees diagnostics/navigation through its runtime.

### 2. Protocol and synchronization foundation

- Bound `Content-Length` before allocation.
- Parse/validate initialize position encoding.
- Add typed server-request responses and explicit `workspace/applyEdit` rejection.
- Extract content-hash-aware document synchronization from `WaitDiagnostics`.
- Add manager lifecycle cancellation and non-blocking read warm-up.

Gate: existing didOpen/didChange diagnostics behavior remains green; repeated unchanged
sync does not bump version; unsupported encoding and oversized frame fail closed.

### 3. Read-only navigation

- Add protocol response structs and manager methods for the four operations.
- Normalize, authorize, sort/deduplicate, cap, and render results.
- Add the `lsp` tool and planner allowlist entry.
- Mark navigation output untrusted.

Gate: one table-driven fake-server main-path test covers all four operations and one
failure table covers unsupported/non-file/outside-policy results. Do not add real-gopls
tests.

### 4. Rename preview

- Add UTF-16/rune/byte conversion helpers.
- Decode both allowed `WorkspaceEdit` text containers and reject resource operations.
- Validate containment, versions, ranges, overlap, and bounds.
- Store a bounded session-scoped plan and return its compact preview/ID.

Gate: one table-driven preview test covers `changes`, versioned `documentChanges`, and
non-ASCII positions; one adversarial table covers outside-workspace, stale version,
overlap, resource operation, and malformed UTF-16.

### 5. Rename apply and shared publication

- Extract the existing atomic publication helper without changing observed-edit
  semantics.
- Add `lsp_rename` and make apply use the agent's global mutation lock.
- Preflight authorization, exact bytes, and versions; publish once; consume only after
  success.
- Render final post-publication diff/readback/diagnostics.

Gate: one main-path test proves the previewed bytes—not a recomputed server response—are
applied across multiple files. One failure test changes a file after preview and proves
zero files are published. Existing Phase 2.5 rollback coverage remains authoritative;
do not duplicate its full matrix.

### 6. Hooks and mutation ordering

- Add/validate `postEdit` config and attach it to the runtime.
- Implement sandboxed argv execution with bounded output and process-group timeout.
- Wire write, exact edit, observed edit, and rename to run hooks before final readback
  and diagnostics.

Gate: one table-driven main-path test covers extension filtering, sorted multi-file argv,
and formatter-modified final readback. One failure-path test covers non-zero/timeout and
proves the mutation remains while the result reports the hook failure. Reuse the runtime
backend contracts; do not create a hook E2E suite.

### 7. Documentation and final verification

- Update `docs/features.md` with the actual tool schemas, runtime ownership, rename
  invalidation rules, and hook config example.
- Mark only implemented roadmap bullets complete in `plan.md`; keep deferred LSP/DAP and
  hook variants deferred.
- Update explicit tool inventories in system/subagent prompts.

Run, in order:

```bash
go test -count=1 ./internal/lsp ./internal/tools ./internal/agent ./internal/config
go test -race -count=1 ./internal/lsp ./internal/tools ./internal/agent
go test -count=1 ./cmd/ghg ./internal/tui
go test -count=1 ./...
go vet ./...
gofmt -s -l .
CGO_ENABLED=0 go build ./...
```

Run `gopls check` only on changed Go files. Do not add GitHub Actions changes in this
slice unless an existing required job fails because of these changes.

## Expected file changes

Keep the diff within this list unless implementation reveals a direct caller that must
change:

- `internal/lsp/client.go`, `manager.go`, focused existing tests, and at most one small
  rename-specific Go file if manager growth becomes unreadable;
- `internal/tools/runtime.go`, `tools.go`, `read.go`, `edit.go`, plus focused
  `lsp.go`/`hooks.go` and tests;
- `internal/agent/filelocks.go`, `definitions.go`, `task.go`, and focused tests;
- `internal/config/config.go` and its existing config tests;
- `cmd/ghg/run.go`, `cmd/ghg/main.go`, `internal/tui/tui.go`;
- `docs/features.md` and `plan.md` after the code gates pass.

Do not create a compatibility shim for the removed global. Fix every in-repository caller
to use the runtime once, at the shared seam.
