# Anthropic Messages wire adapter

Status: COMPLETE

## Scope

Implement the `anthropic-messages` adapter behind `llm.Backend`. This slice is
limited to Anthropic wire translation, streaming/completion behavior, model
discovery, and the provider-neutral fields required to round-trip Anthropic
assistant blocks. Roles, generalized authentication, and other Phase 2 work
remain separate.

## Acceptance criteria

- translate system prompts, text/images, tools, parallel tool calls/results,
  and preserved thinking/redacted-thinking blocks;
- assemble fragmented Anthropic SSE events and expose text/thinking/retry
  callbacks through the existing `EventSink`;
- map stop reasons, maximum output, input/output/cache usage, typed HTTP
  errors, context limits, retries, cancellation, and model discovery;
- test the adapter with deterministic HTTP fixtures, including history and
  session JSON round-trips;
- update the Phase 2 roadmap/feature documentation only for this adapter.

## Tasks

- [x] Add the Anthropic client, backend, protocol factory case, and opaque
  provider-block history field.
- [x] Add focused request, response, SSE, retry, error, catalog, and history
  tests.
- [x] Update feature/roadmap documentation and mark this slice complete.
- [x] Run formatting, focused tests, vet/build, and the relevant race test.

## Notes

Anthropic Messages uses a top-level `system` field, `content` blocks,
`tool_use`/`tool_result` blocks, and an SSE event sequence distinct from OpenAI
Chat Completions. The adapter keeps those details out of the agent loop.

## Result

`llm.NewBackend` now selects `AnthropicBackend` for `anthropic-messages`.
Requests translate system/text/image/tool history into Messages content,
including grouped parallel tool results and stable-prefix `cache_control`
breakpoints. Responses assemble complete and fragmented SSE streams,
preserve thinking/redacted-thinking blocks, expose text/thinking/retry events,
and map stop reasons and input/output/cache usage. Typed HTTP errors, context
limits, cancellation, retry boundaries, maximum output, and paginated model
discovery are covered by deterministic fixtures.
