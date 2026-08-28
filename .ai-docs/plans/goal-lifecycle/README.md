# Structured goal lifecycle

Status: delivered in Phase 2.5 (2026-08-28)

## Objective

Replace the inherited prose/token-driven `/goal` loop with a durable lifecycle
that can be resumed deliberately and can explain progress, blockers, limits,
and completion.

## Delivered

- `internal/goal` defines the persisted record, six lifecycle states, bounded
  notes, model-update validation, and host-controlled pause/limit transitions.
- `internal/session` stores the current goal by session and goal ID, appends
  checkpoints, migrates the legacy `sessions.goal` string, and copies/deletes
  the ledger with sessions.
- `internal/agent` injects goal context per request and exposes a request-local
  `update_goal` tool. The tool can checkpoint progress, report a genuine
  blocker, or complete only with a verification note; it cannot forge host
  pause or limit states.
- `internal/tui` persists live checkpoints, accounts goal turns and provider
  usage, pauses active goals on process resume, requires `/goal resume`, and
  uses the round limit only as a budget circuit breaker.

## State rules

`active` is the only state that drives another turn. `blocked`,
`usage-limited`, and `budget-limited` require explicit `/goal resume`.
`paused` is used for interruption, clear, and process restart. `complete` is
terminal; starting a new objective creates a new goal ID.

The model never completes a goal through final prose. It must call
`update_goal` with `status: complete` and a concise verification note.

## Verification

- `go test ./internal/goal ./internal/session ./internal/agent ./internal/tui`
- `go test ./...` reaches the known pre-existing
  `internal/auth/TestAuthenticateCatalogUsesOneValidatedResponse` fixture
  mismatch (`OPENCODE_KEY` versus the obsolete `OPENCODE_GO_KEY`).
