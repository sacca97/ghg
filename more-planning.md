# Review Continuation and Edit Reliability Plan

## Goals

- Resume an interrupted review without resetting its round count or review boundary.
- Prevent the common observed-edit failure caused by an omitted `operation`.
- Make batch edit errors identify the entry that failed.
- Remind the reviewer to inspect every area the user explicitly requested before submitting.

## Non-goals

- No database-backed review lifecycle.
- No file-by-file coverage tracker or submission gate.
- No component budgets or automatic budget extensions.
- No per-call CHEAP/DEEP/FINAL reasoning system.
- No new telemetry or dependencies.
- No changes to the 38-round hard review limit.

## 1. Resume an interrupted review

The review budget currently lives only inside one agent turn. A terminal provider error ends that turn, so `/continue` starts another review at round zero.

Keep the minimum continuation state on the existing agent:

- original review request and inventory;
- current and allocated rounds;
- whether the review is at a budget checkpoint;
- whether exploration has closed and only submission remains.

Behavior:

- A new `/review <request>` always starts a fresh review and replaces stale continuation state.
- If a review ends with an error or cancellation, retain its continuation state in memory.
- The worker recognizes the existing exact `/continue` input. When review state exists, it resumes Review mode with that state instead of creating a new inventory and budget.
- If no review state exists, `/continue` keeps its current generic interrupted-turn behavior.
- Clear the saved review state after a successful `submit_review`, `/clear`, or a new session.
- Do not persist this state in SQLite. Restarting the worker may lose it; add persistence only if that becomes a demonstrated problem.
- Normal provider retries and steering already happen inside the same turn and need no separate handling.

Progress must resume from the retained value, for example:

```text
◎ review · exploration 4/20
```

not `0/20`.

## 2. Use a simple coverage reminder

Do not build `ReviewCoverage`, component states, inspected-file databases, or a hard submission gate.

Update the review instructions and budget checkpoint text instead:

- Before submitting, sample every frontend, backend, desktop, authentication, UI, or other area explicitly named by the user.
- An explicitly requested but untouched area is a valid reason to request another four-round lease.
- The final evidence batch is for one narrow confirmation, not an unreviewed subsystem.
- At the 38-round hard limit, submit the best available review and disclose any requested area that remains uninspected.

The existing inventory, review progress, observations, and telemetry remain the evidence source. Do not add another tracker.

## 3. Make observed replacement the default

For `mode: "observed"`, treat a missing or blank operation as `replace` when the edit supplies its observation, path, line range, and content.

Keep explicit operations for:

- `delete`;
- `insert_before`;
- `insert_after`.

Update the tool schema so `operation` is optional and its description states that `replace` is the default. Observation ownership, freshness checks, path authorization, atomic publication, and rollback behavior remain unchanged.

## 4. Identify invalid batch entries

Keep batched edits atomic. Do not return a new per-edit result format.

While validating the batch, prefix failures with their one-based entry number:

```text
edit 2: observation is stale; read the range again
```

No file is changed when any entry fails. The model can reread the named entry and retry the original batch.

## 5. Verification

Add only two focused tests:

1. Start a review, complete several exploration rounds, fail a provider request, then continue. Assert that the next request keeps the original inventory, round count, allocation, and checkpoint/finalization state.
2. Use the existing observed-edit test to verify omitted operations replace successfully, then verify one invalid batch entry is named while the file remains unchanged.

Run the existing tests for the agent, worker, tools, and affected client command handling. No client wire change is required.

## Acceptance criteria

- `/continue` after an interrupted review resumes at the previous `N/M` value.
- Continuing does not rerun inventory or recalculate the budget.
- A failed final review submission resumes in submission-only mode rather than reopening exploration.
- Starting a new review discards an older interrupted review state.
- Missing `operation` defaults to `replace` only for observed edits.
- Invalid edit batches identify the failing entry and remain atomic.
- Review instructions treat untouched requested areas as valid extension reasons.
- Review context after successful submission continues to behave as it does now.
- Existing telemetry remains unchanged and is used to evaluate reasoning cost before any dynamic-reasoning feature is proposed.

## Out of scope

Dynamic reasoning selection, provider-specific effort mapping, coverage enforcement, path-guess prevention, new review telemetry, persisted review state, and changes to successful review compaction behavior are deferred.
