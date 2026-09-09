## Goals

- Add backward-compatible batched reads via:
  ```json
  {"ranges":[{"path":"a.go","offset":10,"limit":20},{"path":"b.go","offset":1,"limit":50}]}
  ```
- Run independent ranges concurrently while returning them in request order.
- Issue exactly one observation ID for each successfully returned range.
- Make `grep.patterns` represent independent, labeled searches rather than one indistinguishable regex alternation.
- Clarify when to use `glob` versus `find_files`.

## Non-goals

- No new tool, dependency, generic batching framework, or concurrency abstraction.
- No changes to edit authorization semantics, search ranking, ignore handling, or single-range `read`.
- No historical test cleanup or unrelated documentation work.

## Implementation plan

1. Multi-range `read` — [internal/tools/read.go](/home/sacca/Projects/ghg/internal/tools/read.go)

   - Introduce one small reusable range-argument struct.
   - Extend the schema with `ranges`; retain legacy `path`/`offset`/`limit`.
   - Reject an empty request or mixing scalar and batched forms.
   - Execute range reads concurrently with a small bounded standard-library worker pattern.
   - Preserve request order in the combined output.
   - Keep current line, per-line, byte, sandbox, regular-file, and cancellation checks.
   - Assemble output only at complete range boundaries so truncation never exposes a partial observation. Save one observation per range actually returned; report per-range failures without hiding successful siblings.
   - Preserve scalar observation metadata for legacy calls and add compact JSON metadata for multiple observations.

2. Preserve read bookkeeping — [internal/agent/read_guard.go](/home/sacca/Projects/ghg/internal/agent/read_guard.go), [internal/agent/filelocks.go](/home/sacca/Projects/ghg/internal/agent/filelocks.go)

   - Normalize every entry in `ranges` for duplicate-read suppression.
   - Record all returned observations in the coverage tracker.
   - Record every batched path as touched so existing search ranking continues to work.
   - Reuse the current scalar logic rather than creating a second tracker.

3. Independent `grep.patterns` — [internal/tools/search.go](/home/sacca/Projects/ghg/internal/tools/search.go), [internal/tools/search_rg.go](/home/sacca/Projects/ghg/internal/tools/search_rg.go), [internal/search/state.go](/home/sacca/Projects/ghg/internal/search/state.go)

   - Compile patterns individually instead of joining them into a giant alternation.
   - Keep one filesystem/`rg` traversal; classify each matching line against the individual compiled patterns.
   - Use the existing `search.Item.Pattern` index and add only the minimal snapshot data needed to retain pattern labels across cursors.
   - Render labeled sections grouped by pattern, then file.
   - A line matching multiple queries appears in each applicable pattern group.
   - Keep the existing aggregate snapshot, page, per-file, byte-preview, and cursor limits.

4. Routing guidance — [cmd/ghg/system-prompt.md](/home/sacca/Projects/ghg/cmd/ghg/system-prompt.md)

   - Tell the model to use `grep.patterns` for logically independent searches and avoid giant regex alternations.
   - Say explicitly:
     - use `glob` when the path or glob pattern is known;
     - use `find_files` only when the location is uncertain;
     - do not call both for the same target.
   - Align the tool descriptions with those semantics so the schema no longer describes `patterns` as one OR search.

5. Focused verification

   - Baseline and final: `go test ./internal/tools ./internal/agent ./cmd/ghg`.
   - Add only:
     - one main-path test covering multiple files/ranges, ordering, distinct observations, and concurrent-safe aggregation;
     - one failure-path test covering a failed/oversized range without losing valid sibling results;
     - update the existing grep grouping and system-prompt assertions rather than creating broader suites.

## Acceptance criteria

- Existing scalar `read` calls behave unchanged.
- One call can read disjoint ranges from the same or different files.
- Successful returned ranges have distinct usable observation IDs.
- Batched output remains bounded and deterministic.
- `grep.patterns:["TODO","FIXME"]` returns separately labeled, bounded groups and paginates stably.
- Prompt guidance clearly prevents giant logical alternations and duplicate `glob`/`find_files` discovery.
- No dependencies added and no unrelated files modified.

## Out of scope

The currently modified `internal/agent/agent.go`, session/compaction files, `optimization-plan.md`, and generated message/chat files remain untouched.