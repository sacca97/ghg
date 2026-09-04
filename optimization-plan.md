After inspecting the current implementation, I’d revise the proposal to avoid adding multi-range and multi-query APIs yet. Independent tool calls already execute concurrently in one model round, and `grep.patterns` already combines alternatives into one traversal.

## Revised implementation plan

1. Make capability advertising truthful

   - Preflight the selected shell and configured language-server executables.
   - Remove unavailable tools from the first model request.
   - If `gopls` alone is unavailable while another LSP works, explicitly state that Go LSP operations are unavailable.
   - Generate current-tool guidance from the filtered tool set instead of hardcoded lists.

2. Fix redundant reads at EOF

   - Treat `next_offset=0` as EOF in [read_guard.go](/home/sacca/Projects/ghg/internal/agent/read_guard.go:379).
   - Say the observation already reaches EOF.
   - Never synthesize `end+1`.

3. Correct the read-only allowlist

   Add `structural_search` to [planSafeTools](/home/sacca/Projects/ghg/internal/agent/plan.go:136), making it available in Plan and Ask modes.

4. Keep and refine batching guidance

   The current uncommitted [system-prompt.md](/home/sacca/Projects/ghg/cmd/ghg/system-prompt.md:4) already contains the correct dependency rule. Refine it to:

   > Minimize unnecessary tool invocations and model/tool round-trips. Emit independent calls together in one response. Sequence calls only when an earlier result determines the next query.

   This preserves model judgment while using the existing parallel executor in [agent.go](/home/sacca/Projects/ghg/internal/agent/agent.go:1297).

5. Expose the existing `grep.patterns` capability correctly

   [search.go](/home/sacca/Projects/ghg/internal/tools/search.go:45) already supports multiple patterns in one traversal, but its JSON schema still requires `pattern` even when `patterns` is supplied.

   - Fix the schema so either `pattern` or `patterns` is accepted.
   - Keep its current OR semantics and file-grouped pagination.
   - Do not add `queries` yet.

6. Add soft exploration checkpoints

   - Count one exploration round per model response containing repository-navigation tools, regardless of batch size.
   - Keep the counter local to `Agent.turn`.
   - Inject transient reminders after rounds 10, 20, and 30.
   - Require justification for further exploration without blocking tools.
   - Do not count mutation batches or post-edit verification.

7. Extend existing telemetry

   Reuse tool/model events to record:

   - checkpoint level;
   - whether the next response continued with tools;
   - duplicate fingerprint;
   - tool batch size;
   - same-tool count within the response;
   - tool duration;
   - output bytes.

   These support the requested measurements without storing raw queries.

8. Replay representative workflows before adding more composites

   Measure:

   - model invocations;
   - sequential model/tool rounds;
   - tool calls;
   - calls per model response;
   - repeated `grep`/`read` calls in the same response;
   - wall time;
   - output bytes;
   - correctness.

## Deferred additions

### `grep.queries`

Add only if traces frequently show multiple independent `grep` calls in one response where `patterns` cannot suffice because queries need separate scopes, options, attribution, or pagination.

It would otherwise introduce grouped snapshots, per-query errors, and cursor semantics without reducing model rounds.

### `read.ranges`

Defer. Multiple `read` calls in one response already:

- execute concurrently;
- consume one model round;
- preserve separate observation IDs;
- isolate failures and output limits.

A wrapper would mainly reduce invocation counts while complicating output budgeting, partial failures, redundant-read suppression, and observation metadata.

### `inspect_symbols`

Keep deferred. The model can batch several `symbol_context` or `symbol_references` calls in one response today. Add a multi-symbol composite only if traces still show sequential symbol lookup chains.

### Code Mode

Remain measurement-gated. Consider it only if residual traces are dominated by adaptive workflows where each intermediate result genuinely determines the next operation.

The resulting design principle is:

> Batch independent work in one model response using the existing parallel executor. Collapse deterministic chains into native tools only when doing so measurably reduces sequential rounds or repeated traversal. Keep the model involved where intermediate evidence requires judgment.

No files were changed.