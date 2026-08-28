# Test runtime

Status: COMPLETE

## Objective

Keep production retry behavior unchanged while preventing tests that intentionally
exercise a permanently failing HTTP endpoint from waiting through the full client
retry budget.

## Scope

- Configure the shared `internal/tui` and `internal/agent` test backends with one
  allowed request (`MaxRetries: 1`).
- Leave `llm.DefaultMaxAttempts`, production clients, retry classification, and
  backoff timing unchanged.
- Verify the two known slow tests individually and measure `go test ./...`.

## Tasks

- [x] Set the test-only retry budget in both shared backend helpers.
- [x] Run the TUI and agent regression tests independently.
- [x] Run and time the complete test suite.
- [x] Record results and close this plan.

## Rationale

The failing endpoints return HTTP 500, which is intentionally retryable. These
tests need to assert the error path, not the retry policy. Limiting retries at the
test helper keeps that concern local and avoids weakening resilience for real
requests.

## Results

- `TestGoalFromContextErrorLeavesGoalUntouched`: PASS (`0.567s` package run).
- `TestTurnAPIError`: PASS (`0.323s` package run).
- `go test ./...`: PASS in `8.04s` wall time with the warm cache; `internal/tui`
  completed in `7.083s` and `internal/agent` in `0.554s`.

The production retry budget and backoff code were not changed.
