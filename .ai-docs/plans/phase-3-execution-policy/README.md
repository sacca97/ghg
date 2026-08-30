# Phase 3: execution policy and OS sandboxing

Status: execution-policy stabilization gate complete; later Phase 3 slices remain.

## Goal

Give every agent and executable tool one inherited, fail-closed execution boundary. Native
file/search tools must enforce canonical roots, while Bash, local MCP, and LSP children must
use a restricted environment and an OS sandbox. The optional \`auto-review\` mode delegates
only the ambiguous middle to the configured \`tiny\` role through \`approve-for-me\`.

## Scope delivered in this slice

- \`internal/sandbox/\` owns canonical root authorization, protected \`.git\`/\`.ghg\` roots,
  one-shot grants, status reporting, macOS Seatbelt profiles, and Linux bubblewrap argv.
- \`internal/tools.ToolRuntime\` is attached to agents and inherited by subagents. It carries
  approval mode, human/reviewer hooks, minimal child environments, audit history, and LSP.
- Native \`read\`, \`write\`, \`edit\`, \`grep\`, \`glob\`, and \`find_files\` enforce the runtime path
  policy. Search's short Git ranking probe is wrapped too.
- Bash uses the runtime environment and sandbox; local MCP stdio and LSP processes use the
  same backend. Missing backends return a sandbox error rather than running unrestricted.
- \`CommandRule\` stores exact normalized compound shell commands. Substitutions and nested
  shell syntax fail closed at the approval boundary.
- Compound commands now use a full severity accumulator; path-aware \`rm\` classification keeps
  broad deletion hard-denied and grants external removal roots only for the active human-approved call.
- Linux bubblewrap starts from an empty root with explicit system/policy mounts, while backend
  discovery validates trusted absolute paths and startup canonicalizes private temp/cache roots.
- Approval requests, reviewer telemetry, diagnostics, and audit state use structural redaction;
  malformed shell input retains only an opaque fingerprint. Executable backend contracts cover
  wrapped children, descendants, cache/temp roots, cached Go tests, and network policy.
- \`agent.ApproveForMe\` selects the configured \`tiny\` role, supplies no tools, makes one
  bounded completion, validates a strict decision, and emits separate reviewer telemetry.
- \`--sandbox\`, \`--network\`, and \`--approval\` are one-shot CLI/session overrides. Headless
  runs default to \`never\`; only explicit \`--approval auto-review\` or config enables review.

## Design boundaries

The OS wrapper is defense in depth, not the native authorization source. \`danger-full-access\`
is explicit configuration only. Human prompts remain a separate UX layer, and the old
\`--cautious\` gate still controls routine interactive prompts. Reviewer approvals never persist,
recurse, widen roots, or authorize protected metadata, privilege/service changes, credentials,
global installs, or opaque shell syntax.

No LSP navigation/rename, post-edit hook framework, or spend-ceiling work is included here;
those remain later Phase 3 slices.

## Verification

- \`internal/sandbox/policy_test.go\` covers canonical roots, symlink/nonexistent-target escape
  protection, modes, protected roots, and one-shot grants.
- \`internal/tools/permission_test.go\` covers quoted operators, opaque substitutions, and
  human-only/network classifications.
- \`internal/tools/runtime_test.go\` covers reviewer grants, headless fail-closed behavior,
  child inheritance, environment stripping, native path enforcement, removal grants, and
  structural redaction.
- \`internal/tools/runtime_config_test.go\` covers private temp/cache injection, canonical
  cache paths, and cached Go-test routine classification.
- \`internal/sandbox/backend_contract_test.go\` runs wrapped filesystem, network, temp/cache,
  descendant, and cached-Go-test contracts where the host backend permits it.
- \`internal/agent/reviewer_test.go\` covers strict bounded decision parsing.
- Run \`GOCACHE=/tmp/ghg-go-cache go test ./... -count=1\`, \`go test -race ./... -count=1\`,
  \`go vet ./...\`, all-file \`gopls check\`, and \`CGO_ENABLED=0 go build ./...\` before closing
  the slice.

## Next work

1. Add an exact one-time retry after an observed backend denial, preserving the active tool-call
   ID and final single tool result; broader path-intent extraction remains part of that slice.
2. Extend policy status/audits through the remaining TUI report paths, then proceed to LSP
   navigation/rename and hooks.
