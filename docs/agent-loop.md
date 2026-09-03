# The agent loop

`internal/agent/agent.go` — `Agent.Turn` is the whole idea: append the user
message, stream a completion, run any tool calls, append results, repeat
until the model stops calling tools.

## The cycle

```mermaid
stateDiagram-v2
    [*] --> Append: Turn(user message)
    Append --> CheckBudget: message stored
    CheckBudget --> Compact: tokens > compactPct of window
    CheckBudget --> Stream: fits
    Compact --> Stream: summary replaces old turns
    Stream --> HasTools: completion finished
    HasTools --> RunTools: tool calls present
    HasTools --> [*]: none — turn over
    RunTools --> Steered: results appended in call order
    Steered --> Append: user typed mid-turn (inject at boundary)
    Steered --> CheckBudget: nothing queued
```

Three properties worth knowing:

1. **Steering injects at loop boundaries only.** `Steer` queues a message;
   it lands between iterations, after tool results are appended — never
   mid-generation. Same mechanism delivers background-subagent reports.
2. **A context-limit error is recoverable.** If the provider rejects a
   request (`context_length_exceeded`, `prompt_too_long`, HTTP 413), the
   loop compacts once and retries. A `compacted` guard prevents retry loops.
3. **The loop is headless.** Events (tokens, tool start/end, compaction)
   flow out through a typed `Events` struct; the TUI is one consumer, tests
   are another. `Events.FanIn` merges subagent event streams.

## Parallel tool calls

When the model emits several tool calls in one turn, `runTools` fans them
out to goroutines. Mutations to the same file serialize through a
per-canonical-path channel lock; everything else runs truly in parallel.
Results land back in **call order**, because the chat API matches tool
results to call IDs.

```mermaid
flowchart LR
    subgraph batch["tool calls in one turn"]
        A["write a.go"]
        B["edit a.go"]
        C["write b.go"]
        D["bash go test ./..."]
    end
    A --> LA["lock: /abs/a.go"] --> R["results ch"]
    B --> LA
    C --> LB["lock: /abs/b.go"] --> R
    D --> LG["global bash lock"] --> R
    R --> O["append in call order"]
```

`bash` takes the global lock because a command's side effects aren't
attributable to one path. Reads don't lock. The why and the Go idiom:
[concurrency.md](concurrency.md).

## Compaction

Context is a budget, and the loop spends it deliberately:

- **Proactive** — `maybeCompact` runs before each request once the latest
  successful request's provider-reported context size (`PromptTokens +
  CompletionTokens`) crosses the adaptive threshold: $\min(0.80 \times \text{window}, 400000, \text{window} - \text{reserve})$.
  When explicit `compactPct` is set in config (slidable ←/→ in the ctrl+p settings),
  the chosen percentage is honored while still respecting the output reserve.
  It is zero until the first successful response.
- **Reactive** — a provider context-limit error triggers one compaction + retry.
  A `compacted` guard prevents retry loops.

`compact()` uses cumulative, dedicated summarization:
1. **Cumulative Checkpoint Reuse** — if an earlier compaction exists, its body is passed
   inside `<previous_checkpoint>` tags so the model updates existing state with `<new_history>`
   instead of re-summarizing from scratch.
2. **Dedicated System Prompt** — summarization runs under a focused prompt instructing the model
   to produce a state checkpoint without executing tasks or answering questions.
3. **Truncated Summary Rejection** — checkpoints that hit token output limits or return empty
   are rejected rather than corrupting session state.
4. **Bounded Tail & Atomic Groups** — keeps the system prompt plus a recent tail capped at
   $\min(\text{ContextLimit}/4, 24000)$ tokens. Tool-call groups are selected atomically from
   newest to oldest, ensuring a tail never orphans a tool result from its assistant call.

Tool results are structured before this boundary. The summary ledger records
bounded arguments/output, exit status, duration, and output metadata; the
derived prompt carries a metadata-only manifest for references cited by the
summary or kept tail. A recent result that exceeds the remaining token budget
is shrunk deterministically without dropping its output id, while the raw
SQLite message log remains available for resume, retry, audit, and
`output_read`.

```mermaid
flowchart TB
    subgraph before["conversation before compaction"]
        S[system prompt]
        P["prior checkpoint<br/>(if exists)"]
        T1[old turns 1..N]
        T2["recent tail<br/>(bounded ≤ 24k tokens)"]
    end
    S --> C["compact()"]
    P --> SUM["summarize via Complete<br/>(dedicated prompt, non-streaming)"]
    T1 --> SUM
    T2 --> C
    SUM --> C
    C --> after["system + cumulative summary + tail<br/>(tail never orphans a tool call)"]
```

The summarizer defaults to `deepseek-v4-flash-0731`
(`config.DefaultCompactModel`), falls back to the configured
`compactModel`/`compactProvider`, then to the conversation's own model.
`/compact [model] [provider]` does it by hand.

## Per-turn tool freezing & plan runaway guard

1. **Tool Freezing** — at the start of `Turn()`, `AllTools()` and tool definitions are
   snapshotted once. Subsequent rounds reuse these stable definitions, avoiding per-round
   reallocations or mid-turn tool drift.
2. **Plan Runaway Guard** — in Plan mode, a rollout budget tracks weighted token expenditures
   and a 128 model-call ceiling. Crossing any reserve
   disables tools and forces a final synthesis request for `<proposed_plan>`.
3. **Cheap Review Correction** — validation failures during code reviews trigger a bounded
   2-round correction definition exposing only `submit_review`, preventing full multi-round
   exploratory retries.

## Background subagents

`task` with `background: true` runs a subagent concurrently and reports back
as a steered message on completion — one channel close wakes the tool
caller, the TUI redraw, and `/tasks` simultaneously. Details:
[concurrency.md](concurrency.md#2-background-subagents--one-channel-close-many-waiters).

## Read next

- [concurrency.md](concurrency.md) — the channel primitives
- [tools.md](tools.md) — what the tool calls actually do
- [features.md](features.md#the-agent-loop) — the same loop, linked to code and tests
