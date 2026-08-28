# Declarative provider profiles

Status: COMPLETE — declarative profiles now load, validate, resolve, and feed
the existing backend/catalog seams.

## What

Add reusable provider definitions to the existing harness configuration. The
primary `~/.harness/config.json` remains JSONC and continues to own provider
instances, credentials, and per-model routing. Profiles own non-secret
transport metadata and may be embedded in the binary or supplied by the user
or trusted project.

## Goal

- Load strict YAML profiles with deterministic embedded < user < trusted-project
  precedence.
- Validate protocols, catalogs, authentication modes, headers, and endpoint
  URLs before a profile can reach a backend.
- Keep secret indirection in JSONC; profile YAML must never contain a secret or
  a secret resolver expression.
- Preserve existing provider entries by treating a provider without `profile`
  as an anonymous in-memory profile.
- Select the existing backend seam from a resolved profile, with unsupported
  protocols failing explicitly.

## Non-goals

- Migrating existing JSONC or catalog files.
- Implementing Anthropic wire-format support in this slice.
- Allowing project profile files to execute code or bypass folder trust.
- Moving credentials, environment variable names, or `!cmd` references into
  YAML.

## Design

`internal/provider` will own profile schema, embedded defaults, filesystem
loading, validation, and instance resolution. The loader has three levels:

1. profiles embedded in the binary;
2. `~/.harness/providers/*.yaml` (or `HARNESS_HOME/providers`);
3. `.harness/providers/*.yaml` in the current project, only after the caller
   has passed the existing trust gate.

Duplicate IDs within one level are errors. A later level deliberately replaces
an earlier ID. Every profile has `schema: 1`, a known canonical protocol, a
normalized HTTPS endpoint (HTTP is limited to explicit loopback), and an
explicit supported auth/catalog kind. The resolved instance applies the
JSONC provider's optional `baseUrl` and `api` overrides without copying
credentials into the profile model.

The first embedded set covers inference.net, OpenRouter, generic OpenAI-
compatible HTTP, and Anthropic Messages metadata. Only protocols with a
compiled adapter can be used to start an agent; profile parsing remains useful
for future adapters and reports an actionable unsupported-protocol error.

## Prior art

- `plan.md` Phase 1 defines the schema, precedence, URL policy, and anonymous
  compatibility requirement.
- `internal/config/config.go` already resolves `apiKey`/`apiKeyEnv`/secret
  references at point of use; profiles must not duplicate that mechanism.
- `internal/config/trust.go` is the existing project trust boundary.
- `internal/llm/backend.go` is the execution seam that profile resolution must
  feed; provider-specific behavior stays in adapters.

## Tests

- table-driven schema and URL validation tests;
- unknown-field, duplicate-ID, precedence, and trust-gating tests using temp
  directories;
- anonymous legacy-provider compatibility tests;
- backend construction tests for profile headers/auth and clear unknown-
  protocol errors;
- focused package tests, vet, compile checks, and race tests for the new
  loader/backend paths.

## Documentation

- Update `docs/features.md`, `docs/models-providers.md`, `docs/architecture.md`,
  and `docs/roadmap.md` as the implementation lands.
- Link this plan from the completed backend plan; keep that plan's backend
  result intact while this follow-on slice progresses.

## Tasks

- [x] Choose and add the maintained `gopkg.in/yaml.v3` parser after explicit
  dependency approval.
- [x] Add embedded profile schema and the initial provider profile files.
- [x] Implement strict loading, precedence, trust gating, URL normalization,
  and actionable errors.
- [x] Add the JSONC `profile` field and anonymous in-memory compatibility.
- [x] Wire resolved profiles into headless/TUI backend and catalog creation.
- [x] Allow a profile's catalog to name its `models.dev` provider alias so
  missing `context_length` values can use public `limit.context` metadata.
- [x] Update documentation and remove/update stale plan references.
- [x] Run focused tests, full compile/vet, and relevant race tests.

## Result

`internal/provider` now embeds `inference`, `openrouter`, `generic-openai`, and
`anthropic` profiles; loads user and trusted-project overrides; rejects unknown
fields, duplicate IDs, unsafe URLs, invalid auth/catalog metadata, and profile
credential fields; and resolves legacy JSONC entries anonymously. The existing
OpenAI adapter now applies profile default headers and bearer/raw/none auth.
Headless and TUI agent/catalog construction consume the resolved profile. The
Anthropic metadata is now backed by the follow-on native Messages adapter in
`.ai-docs/plans/anthropic-wire-adapter/`.

Catalog profiles may also declare `catalog.models_dev`; ghg uses that alias to
fill missing context windows and reasoning controls from the daily, lazily
refreshed public models.dev data. The effort picker can expose exact values
such as `max` or a toggle-only `off`/`on` surface. Only model IDs currently
needed by ghg are retained locally.

Focused provider/LLM/command tests and the full repository test suite pass.
`go vet ./...`, `CGO_ENABLED=0 go build ./...`, `go mod verify`,
`git diff --check`, and race tests for `internal/provider`, `internal/llm`, and
`cmd/harness` also pass. The pre-existing broader TUI race caveat remains
recorded in the fork plan and was not changed by this slice.
