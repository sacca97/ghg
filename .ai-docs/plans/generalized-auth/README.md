# Generalized profile authentication

Branch: working tree

Status: COMPLETE

## What this does

Generalize `/auth` and `ghg auth` from an OpenRouter-only command to the
loaded declarative provider profile set. A profile supplies its display name,
authentication mode, environment-variable name, documentation URL, protocol,
and optional catalog capability; JSONC continues to own credential values or
secret references.

## Goal

- Resolve `/auth <id>` and `ghg auth <id>` through `provider.Profiles`.
- Validate credentials through the selected compiled backend before saving.
- Use catalog discovery when the profile advertises it, otherwise use an
  authenticated protocol probe without creating a catalog entry.
- Preserve literal/env mode's never-split-brain storage rule and the existing
  masked TUI/key-indirection safety boundary.
- Keep profile additions YAML-only: adding `opencode` must not require Go
  changes.
- Let the interactive TUI start without credentials, explain the degraded state,
  and promote the first authenticated agent in place through `/auth`.
- Resolve one provider profile through ordered per-model routes, so a single
  subscription can select different compiled wire adapters and auth headers
  without duplicating credentials or `/auth` entries.

## Non-goals

- OAuth, keychain encryption, or new secret-reference syntax.
- Roles, agent definitions, sampling parameters, provider failover, or other
  Phase 2 work.
- Changing the provider wire adapters beyond the small optional probe
  capability needed by catalog-less authentication.

## Design

`internal/provider/profile.go` adds optional `auth.env_var` and
`docs.keys_url` metadata and exposes them through `provider.Resolved`. A
profile's ordered `routes` use `path.Match` globs and can override only
protocol, auth mode/header, and default headers. Route credentials inherit the
profile's environment-variable name; base URL, docs, catalog, and environment
source remain provider-level.
`internal/config` owns `UpsertProviderKey`, which imports no credentials into
profile data and writes exactly one of `apiKey` or `apiKeyEnv`.

`internal/auth` owns the shared capability-driven validation flow. It resolves
the profile, constructs `llm.Backend`, calls authenticated
`CatalogBackend.Models` for private catalogs, or fetches a public catalog to
obtain a real model ID before calling the optional `ProbeBackend`. The probe
uses a one-token bound and rejects an explicit `error.type: AuthError`; a
model/request error, including a 401 `ModelError`, is not treated as a bad key.
A backend with neither capability returns a confirmation-required result;
callers save only after an explicit user confirmation. Validation errors
redact the supplied key before they cross the CLI/TUI boundary.

The CLI and TUI share profile resolution and validation but retain their
existing input surfaces. TUI keys stay out of input history, queue messages,
transcript blocks, and rendered output. CLI `--env` reads the profile's own
environment variable and rejects profiles that do not declare one. Catalog
conversion/persistence is shared so the validation response is the catalog
seed rather than a second request.

The provider package no longer imports config merely to convert a JSONC
provider; callers pass the small credential-free `provider.Instance` directly.
This keeps the dependency direction valid while `Config.UpsertProviderKey`
accepts `provider.Resolved`.

The interactive TUI now treats an absent/empty credential as a degraded start with a
nil agent and the concise `No provider has been configured — run /auth` note.
Secret-resolution failures and explicit
unknown model/provider/profile requests remain hard errors. Agent-dependent commands,
scheduler ticks, and background task starts are nil-safe; a successful `/auth` seeds a
catalog when available and builds the first live agent without restarting. Headless
`ghg run` and JSON output retain fail-fast behavior.

## Files

- `internal/provider/profile.go` and `internal/provider/profiles/*.yaml`
- `internal/config/provider_key.go`, `internal/config/catalog.go`
- `internal/llm/backend.go`
- `internal/auth/auth.go`
- `internal/tui/auth_cmd.go`, `internal/tui/complete.go`, `internal/tui/registry.go`,
  `internal/tui/tui.go`
- `cmd/ghg/auth.go`, `cmd/ghg/main.go`
- focused config/provider/auth/CLI/TUI tests and feature/roadmap docs

## Test plan

- Profile metadata parsing, validation, precedence, and custom YAML-only IDs.
- Literal/env upsert idempotence, mode switching, missing env metadata, and
  any-provider usability without leaking values.
- Catalog validation and seeding, catalog-less probe validation, explicit
  unvalidated confirmation, unknown-ID listings, auth-header routing, and
  redacted errors against deterministic local HTTP fixtures.
- TUI masked prompt/listing/completion/live rekey and key non-leakage tests.
- Cold-start TUI behavior: actionable degraded startup, nil-agent command safety,
  and first-agent promotion after `/auth`.
- CLI key source/`--env` behavior, catalog seeding, and bad-key no-write tests.
- Full tests, vet, CGO-free build, diff validation, and race coverage for the
  affected Go packages.

## Tasks

- [x] Add profile auth/docs metadata and provider/config boundary cleanup.
- [x] Add shared auth validation/probe and generic credential/catalog helpers.
- [x] Generalize the TUI `/auth` path and completion.
- [x] Generalize `ghg auth` and remove the OpenRouter-only implementation.
- [x] Add cold-start TUI behavior and first-agent promotion.
- [x] Add ordered per-model profile routes, the model-level `api` escape hatch,
  and the unified OpenCode Go profile.
- [x] Update docs/roadmap and run the phase-specific verification gates.

## Verification

- Focused auth/TUI/config/provider/LLM/CLI tests pass.
- Route tests cover first-match-wins, auth/header inheritance, the removed
  legacy OpenCode profile, unsupported protocols, and model-level overrides.
- The new cold-start/auth tests pass under `go test -race`.
- `go vet ./...`, `CGO_ENABLED=0 go build ./...`, and the scoped diff check pass.
- The repository-wide `go test ./...` gate passes.
- The broad affected-package race run remains red only at the pre-existing
  `internal/tui/TestScheduleFiresWakeup` scheduler/chroma race; the new tests
  are race-clean in isolation.

## Prior art

- `plan.md` Phase 2, “Generalized `/auth` — any profile, not just OpenRouter”.
- `internal/provider` profile precedence/trust and `internal/config/secret.go`
  secret resolution at point of use.
- `.ai-docs/plans/openrouter-auth/README.md` for the existing masked prompt,
  guarded config save, catalog seed, and key-leak hardening to preserve.
