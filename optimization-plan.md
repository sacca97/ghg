# Repository Navigation Round-Trip Optimization Plan

## Status

This is the authoritative implementation plan. It is a first optimization
phase, not a permanent rejection of programmatic orchestration.

The prior telemetry/baseline implementation step remains omitted. Performance
is evaluated with existing logs and a small manual replay set; no new telemetry
subsystem is required.

## Architectural decision

Prefer bounded native composites for recurring deterministic workflows. Add
generic programmatic orchestration only if measured residual traces remain
dominated by adaptive multi-tool sequences.

Use the smallest suitable operation:

1. `grep` for literal or regex text;
2. `structural_search` for syntax-aware code patterns;
3. `lsp` for semantic identity, references, and symbol context;
4. `read` for exact bounded ranges;
5. parallel direct calls for independent queries; and
6. one existing `bash` call for a known verification sequence.

The model chooses what evidence it needs. Tools collapse predictable lookup,
selection, normalization, and pagination that require no model judgment.

## Goals

- Reduce sequential `grep -> model -> read -> model -> lsp` loops.
- Prove the value of deterministic composite operations with minimal changes.
- Add bounded in-process structural search without replacing GHG's tools.
- Let explicitly requested structural source participate in observed edits.
- Reuse sandboxing, git-aware traversal, snapshots, cursors, result bounds,
  untrusted marking, and session persistence.
- Verify that representative workflows become materially faster without losing
  answer correctness.

## Non-goals

- No telemetry service, benchmark framework, or exploration-round budget.
- No embedded JavaScript, Python, Starlark, or generic `execute_code` tool.
- No external ast-grep executable or hidden subprocess wrapper.
- No CGO/Rust backend in this phase.
- No AST rewriting or model-visible structural mutation.
- No replacement of `grep`, `read`, existing LSP navigation, or `lsp_rename`.
- No system-prompt injection, reminder, compaction, or tool-aging change.
- No multi-language grammar bundle in the first implementation.
- No generic evidence-bundle or repository-context framework.

## 1. Add composite read-only LSP operations

Implement these first because they use existing machinery and directly remove
known model round trips.

Extend the existing `lsp` tool. Compose calls through the existing
`LanguageService.Navigate` boundary in `internal/tools`; do not create another
tool, package, or language-service abstraction.

### `symbol_references`

Input:

```json
{
  "operation": "symbol_references",
  "path": "internal/tools/language.go",
  "symbol": "runLSPResult",
  "include_declaration": true
}
```

Behavior:

1. Authorize the file for reading.
2. Request `document_symbol`.
3. Select an exact symbol name.
4. Use its `SelectionRange` start as the semantic position.
5. Request `references`.
6. Return one bounded normalized result.

If no exact symbol exists, return a concise not-found result. If multiple exact
symbols exist, return bounded candidates containing kind and range and require
position-based disambiguation; never guess.

### `symbol_context`

Input:

```json
{
  "operation": "symbol_context",
  "path": "internal/tools/language.go",
  "symbol": "runLSPResult"
}
```

Behavior:

1. Authorize the file and request `document_symbol`.
2. Resolve an exact symbol as above.
3. Read its full `Range` through the existing bounded observed-read path.
4. Return source text and an observation ID accepted by `edit`.

`symbol_context` always creates an observation because its purpose is to fetch
the implementation, commonly before an edit. Honor existing line/byte limits.
For an oversized symbol, return the first bounded observed range plus normal
continuation metadata rather than raising limits.

Keep position-based `definition`, `references`, `hover`, and `document_symbol`
unchanged. Keep `lsp_rename` separate because it owns preview/apply state and
write authorization.

### Focused tests

- A unique exact symbol resolves references and observed context.
- An absent or ambiguous symbol returns an actionable bounded result without
  choosing a candidate.

## 2. Replay representative LSP workflows

Use existing debug/exported logs and 3–5 fixed prompts; do not build a replay
framework. Include at least:

- where a known symbol is defined and referenced;
- show a known implementation before changing it; and
- determine whether another test covers a symbol or behavior.

Record manually in this document or an implementation report:

| Metric | Before | After |
| --- | ---: | ---: |
| Model invocations | | |
| Sequential model/tool rounds | | |
| Tool calls | | |
| Tool-output bytes entering context | | |
| Wall-clock completion | | |
| Correct result | | |

Proceed to structural search even if LSP only covers part of the cases, but
record what remains unresolved. The comparison validates the native-composite
thesis and provides a reference for the next stage.

## 3. Add Go-only in-process `structural_search`

### Naming

Call the public tool `structural_search`, not `ast_grep`. The selected matcher
is AST-grep-inspired but is not guaranteed to accept the complete ast-grep rule
language. Its description must say exactly what it supports:

> Search Go source structurally using bounded code patterns and metavariables.

### Backend ladder

Start with a pinned, audited version of
`github.com/odvcencio/gotreesitter/grep` and its pure-Go Tree-sitter runtime.
Qualify it with GHG fixtures for:

- Go function and method declarations;
- `$NAME`, `$_`, and multi-node metavariables such as `$$$ARGS`;
- contextual Go call patterns where expression parsing is ambiguous;
- exact UTF-8 byte ranges and complete match text;
- invalid patterns and partial parses; and
- cancellation or enforceable per-file parse limits.

Record license, dependency size, binary-size increase, cold/warm parse time,
allocations, and grammar-loading behavior. This is one bounded dependency
assessment, not a general benchmark suite.

If the high-level `grep` package lacks a required operation, use the same
library's raw query API or outliner for that concrete operation. If the pure-Go
stack still cannot satisfy the fixtures or safety bounds, stop this phase and
document the missing capability. Do not automatically introduce CGO.

Only after a demonstrated real-workload need may a later plan consider a CGO
bridge to upstream ast-grep. Do not keep two production backends or retain code
from a rejected spike.

### Internal package boundary

Wrap the chosen functionality in one small concrete package:

```go
package structuralsearch

type Query struct {
	Language string
	Patterns []string
}

type Match struct {
	StartByte int
	EndByte   int
	Pattern   int
}

func Search(ctx context.Context, query Query, source []byte) ([]Match, error)
```

For this phase, `Language` accepts only `go`. Reject other values clearly.
The package matches one already-authorized source buffer and returns byte
spans. It does not walk directories, read files, rank, paginate, create
observations, or edit source.

### Tool contract

```json
{
  "patterns": ["func ($RECV $TYPE) $NAME($$$ARGS) $$$BODY"],
  "language": "go",
  "path": "internal/tui",
  "max_results": 20,
  "cursor": "optional opaque cursor",
  "observe": false
}
```

- `patterns` is required, non-empty, and bounded to a small count.
- `language` is required and must be `go` in V1.
- `path` defaults to the workspace and passes normal read authorization.
- `max_results` follows existing search-page defaults and caps.
- `cursor` reads an immutable stored snapshot without traversal or reparsing.
- Cursor requests cannot change query semantics.
- `observe` defaults to `false`.
- No raw Tree-sitter queries, node-kind selectors, rewrites, or fixes are
  exposed publicly in V1.

Known exact symbols should still use `lsp`. `structural_search` is for
repository-wide syntactic shapes and naming patterns that text search cannot
represent reliably.

### Traversal and execution

Authorize and canonicalize the requested root, then reuse GHG's existing
git-aware search traversal. Read only authorized bounded regular files with Go
source extensions and pass their bytes to `structuralsearch.Search`.

Use a small bounded worker pool. Stop scheduling promptly on cancellation or
when the snapshot match/byte ceiling is reached. Enforce maximum file size,
query size, pattern count, and match count around the library boundary.
Invalid patterns, unsupported language, partial parses, and library failures
must return normal tool errors rather than crash GHG.

Compile patterns once per request when supported. Merge and deduplicate exact
ranges returned by multiple patterns. The library receives no paths and owns no
filesystem, sandbox, network, or mutation responsibility.

### Snapshots and output

Convert byte spans to GHG's one-based line/column convention. Extend
`internal/search.Item` only with fields needed to persist and render a match
range. Save results as kind `structural_search` in the existing session-scoped
`search.Registry`.

Reuse stable snapshots, result/byte bounds, grouped output, touched/git-aware
ranking where applicable, opaque cursor pagination, and concise incomplete
reasons. Mark model-visible source untrusted. Backend objects never enter model
context.

### Optional observations

When `observe=false`, return bounded source evidence and ranges without
creating observation records.

When `observe=true`, handle only matches rendered on the current page:

1. Group displayed matches by file.
2. Open and validate each file once.
3. Expand partial spans to complete covering lines.
4. Verify the current bytes still match the snapshot.
5. Merge overlapping/adjacent visible ranges where useful.
6. Create ordinary session observations for only the displayed source.
7. Render each observation ID beside its authorized range.

Never observe hidden pages or omitted results. If a file changed after snapshot
creation, mark its results stale and issue no observation. Keep `edit`
unchanged; structural rewriting remains unavailable.

### Focused tests

- A Go pattern produces bounded grouped results, and cursor pagination performs
  no traversal or parsing.
- An observed visible result authorizes `edit`; stale or unauthorized source
  produces no observation.

## 4. Replay structural workflows and decide the next step

Repeat the same measurement table using representative prompts such as:

- locate selected TUI command declarations;
- determine whether another test structurally covers a behavior; and
- find implementations matching a code shape.

Compare direct text/LSP exploration with the new tool. Correctness must be
unchanged. The target is a material reduction in sequential model/tool rounds;
30% is a useful initial engineering target, not a hard architectural threshold.

Classify remaining slow traces:

- recurring deterministic sequence: consider one bounded native operation;
- exact repeat work: consider snapshot/observation reuse;
- predictable first file read: consider tagged-file prefetch;
- model routing problem: consider concise stable guidance; or
- genuinely adaptive conditional pipelines: reconsider Code Mode.

Do not implement those follow-ups in this phase.

## Existing behavior to preserve

- Independent direct calls emitted together execute through the current
  parallel batch path.
- Known format/build/test sequences may use one existing `bash` call when safe,
  with dependent checks sequenced normally.
- Subagents are for genuinely independent complex work, not one deterministic
  navigation chain.
- Existing observations, safe edits, safe rename, post-edit hooks, compaction,
  and tool-output retention remain unchanged.

## Acceptance criteria

- `symbol_references` replaces symbol discovery plus references for a unique
  symbol without ambiguity guessing.
- `symbol_context` returns bounded source and an edit observation.
- `structural_search` is in-process, Go-only, read-only, and requires no
  external executable or runtime download.
- The public tool exposes bounded code patterns, not unsupported ast-grep or
  raw Tree-sitter rule syntax.
- Traversal, authorization, bounds, ranking, snapshots, persistence, cursors,
  and untrusted marking reuse existing GHG paths.
- `observe=false` creates no observations; `observe=true` validates each visible
  file once and observes only displayed unchanged bytes.
- The pure-Go dependency passes required fixtures and only one production
  backend remains.
- Representative workflows retain correctness and show a material reduction
  in sequential model/tool rounds; results are recorded.
- No CGO, generic code runtime, AST mutation, or prompt injection is introduced.

## Expected files

- `internal/tools/language.go`
- existing focused LSP tool tests
- `internal/structuralsearch/structuralsearch.go`
- `internal/structuralsearch/structuralsearch_test.go`
- `internal/tools/structural_search.go`
- `internal/tools/structural_search_test.go`
- `internal/tools/tools.go`
- `internal/search/state.go` only for required range fields
- `go.mod` and `go.sum`
- this plan or a short implementation report for replay results

Prefer extending existing test files where clearer. Do not create a generic
search engine abstraction or a new test framework.

## Out of scope

- model/provider adapters
- TUI rendering and interaction
- session schema migrations
- MCP protocol changes
- post-edit hook and safe-rename internals
- compaction, history, tool-result aging, and rollout budgets

## Explore later

### Tagged-file prefetch

Prefetch small explicit `@file` mentions through observed `read` only if the
predictable first-read round remains material.

### Prompt guidance and exploration reminders

Batching, sufficiency, negative-search, and reminder instructions can cause
both premature stopping and excess continuation. Consider concise stable
guidance only if replay traces show a routing problem. Keep live reminders off.

### Duplicate-query suppression

Exact repeated searches and unchanged reads could reuse snapshots or
observations. Revisit if duplicates remain material; never infer semantic
equivalence between different queries.

### Additional languages and selectors

Add grammars based on observed usage rather than shipping the library's full
registry. Consider a bounded `kind` plus field-regex selector only if code
patterns cannot express recurring declaration searches without guesswork.

### CGO ast-grep integration

If real workloads demonstrate semantics the pure-Go engine cannot support,
prepare a separate decision document for a narrow CGO bridge to pinned upstream
ast-grep crates. Account for release builds, cross-compilation, FFI ownership,
panic containment, and `CGO_ENABLED=0`. Do not add it merely because CGO is
acceptable.

### Generic Code Mode

Reconsider `execute_code` only if measured residual traces are dominated by
adaptive sequences with conditionals, filtering, aggregation, or per-result
follow-up that bounded native tools cannot anticipate. It must call existing
GHG tools through the same policy boundary and expose only compact final
evidence.

### AST rewriting and evidence bundles

Keep mutations on the observed-edit/safe-rename path. Do not add structural
rewrite or a generic context bundle unless a concrete recurring workflow earns
it.
