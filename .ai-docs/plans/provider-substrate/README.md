# Provider-neutral backend boundary

Status: COMPLETE for this slice — the execution seam and OpenAI adapter are in
place. Declarative profiles are tracked as the follow-on
[provider-profiles](../provider-profiles/README.md) slice; the Anthropic wire
adapter is tracked separately in
[anthropic-wire-adapter](../anthropic-wire-adapter/README.md).

## What

Move the agent loop off the concrete OpenAI HTTP client so the harness can add
Anthropic Messages and profile-selected adapters without teaching the agent
provider-specific behavior.

## Goal

- Give turns, compaction, and foreground/background subagents one small backend
  contract: streaming events and non-streaming assistant messages.
- Keep retry, cancellation, context-limit detection, usage accounting, and
  streamed callbacks observable exactly as they are today.
- Keep model discovery optional and separate from turn execution.
- Make retry callbacks per request instead of mutating a shared client from the
  agent turn goroutine.

## Non-goals

- Anthropic wire support or declarative profile loading; those are subsequent
  Phase 1/Phase 2 slices.
- A config migration, a new model catalog, or changes to tool semantics.
- Rewriting the existing OpenAI message/tool storage format before a second
  adapter needs it.

## Design

`internal/llm/backend.go` owns the provider-neutral execution interfaces:

- `Backend.Stream(ctx, request, EventSink)` returns the assembled assistant
  message and usage.
- `Backend.Complete(ctx, request)` returns an assistant `Message` and usage.
- `CatalogBackend` is an optional capability for providers that expose model
  discovery; the agent does not require it.

`llm.OpenAIBackend` is an adapter around the existing `llm.Client`. The client
keeps its current callback-based methods for compatibility, while its retry
implementation is also callable with a request-local event sink. The adapter
uses that path, so concurrent turns do not race through `Client.OnRetry`.

The TUI and headless runner construct the adapter through the small protocol
factory. Existing `openai-completions` config entries map to the compiled
`openai-chat-completions` adapter. Unsupported protocols fail explicitly.

The agent fields become `Backend` and `CompactBackend`; all subagents inherit
the parent backend, and compaction uses the selected backend's `Message` text.

## Prior art

- The current `internal/llm/openai.go` already owns retries, usage, context
  errors, and stream assembly; this slice preserves those invariants.
- `plan.md` Phase 1 defines the two-method backend boundary and optional
  catalog capability.
- `docs/concurrency.md` requires request-scoped callbacks and warns against
  shared mutable state across worker goroutines.

## Verification

- Unit-test the adapter contract, request-local retry callback, completion
  message conversion, and unsupported protocol errors.
- Run formatting, vet, focused package tests, then `go test ./...` with the
  repository's writable temporary Go caches.
- Run the race detector on the affected packages; retain the pre-existing TUI
  race caveat in this plan rather than hiding it.

## Documentation

- Update `docs/features.md` with the backend/catalog split and callback safety.
- Update `docs/roadmap.md` when this slice is complete; the Anthropic follow-on
  adapter and profile work remain tracked in their own plans.
- Keep this plan current as field names and factory behavior settle.

## Tasks

- [x] Add the neutral backend, event sink, catalog capability, and protocol
  factory.
- [x] Refactor OpenAI retry/complete internals behind the adapter without
  changing legacy client behavior.
- [x] Move agent turns, compaction, and subagents to the backend fields.
- [x] Update TUI/headless construction and focused tests.
- [x] Update feature/roadmap docs and run verification.

## Result

`llm.Backend` now has the planned two methods. `llm.EventSink` carries
request-local stream/thinking/retry callbacks, `llm.CatalogBackend` keeps model
discovery optional, and `llm.NewBackend` maps both the legacy
`openai-completions` config value and the canonical
`openai-chat-completions` protocol to `llm.OpenAIBackend`; the native
`anthropic-messages` protocol is implemented by the follow-on Anthropic
adapter.

The legacy `llm.Client.Stream` and `Complete` methods remain available to
existing direct callers. Their wire-only `stream` fields moved into the
OpenAI adapter request shape, so the shared `llm.Request` passed through the
agent no longer controls transport mode. The agent now stores
`Backend`/`CompactBackend`; foreground and background turns share that
contract, and compaction consumes the returned assistant `Message`.

Verification completed:

- `gofmt` on all changed Go files, `git diff --check`, `go vet ./...`, and
  `CGO_ENABLED=0 go build ./...` passed.
- The affected `llm`, `agent`, `tui`, `cmd/harness`, and `mcp` test suites
  passed with localhost access; the backend and agent packages also passed
  `go test -race`.
- The broader Phase 0 race caveat remains recorded in
  `.ai-docs/plans/harness-fork-detach/README.md`; this slice did not alter
  those pre-existing TUI/tool failures.
