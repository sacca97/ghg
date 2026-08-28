# ghg manual

Everything that used to crowd the top-level README: full setup, config
reference, MCP, and the map of how ghg works.

Start with [architecture.md](architecture.md) for the moving parts.

## Install

Prebuilt binaries (Linux/macOS, x64/arm64) from GitHub Releases — checksum-verified:

```sh
curl -fsSL https://raw.githubusercontent.com/sacca97/ghg/main/install.sh | sh
```

The script downloads the release asset for your platform, verifies it against the published `SHA256SUMS`, and drops `ghg` into the first writable directory on your `PATH`. Pin a version with `GHG_VERSION=v0.1.0`, force the install dir with `GHG_BIN_DIR`.

From source instead (requires Go ≥ 1.27):

```sh
go install github.com/sacca97/ghg/cmd/ghg@latest
```

From a cloned repo, `task install` does the same with the version stamped from git.

## Setup

ghg starts without a configured provider. Add one through the profile-driven
auth flow:

```sh
git clone https://github.com/sacca97/ghg && cd ghg
task install                        # builds + installs ghg (version stamped from git)

ghg auth openrouter             # masked key prompt
# or use another loaded profile:
ghg auth <profile> <key>
```

Then `ghg` and you're in. First things to try: `/context-doctor` (audit
what a fresh session injects, in tokens), `/goal <text>` (work until done),
drop a `.mcp.json` in the repo (MCP servers just appear — `/mcp` to see them).

## Run

```sh
task run                 # run locally from source
task run -- -m glm-5.2-fast          # pass flags after --
ghg                    # installed binary, default model
ghg -m example-model -p example     # pick model AND provider
```

`task --list` shows the rest (build, test, fmt, vet, tidy).

In-session: `/model <name> [provider]`, `/tasks` (background subagents), `/clear`, `/help`, `/quit`. ctrl+c once interrupts; ctrl+c twice quits (and kills any agent-spawned child processes).

The `task` tool runs tool calls in **parallel** (per-path file-mutation locks keep edits to the same file serial) and supports `background: true` to launch a subagent that works concurrently and reports back when done.

See [features.md](features.md) for the full feature map and [concurrency.md](concurrency.md) for the channel design.

## Config — `~/.ghg/config.json`

Models are routed to providers: a model lists the providers that serve it, and
you can switch providers without touching the model. A generic first-run entry
looks like this:

```json
{
  "defaultModel": "example-model",
  "providers": {
    "example": {
      "name": "Example provider",
      "profile": "generic-openai",
      "baseUrl": "https://api.example.com/v1",
      "api": "openai-completions",
      "apiKeyEnv": "EXAMPLE_API_KEY"
    }
  },
  "models": {
    "example-model": { "providers": ["example"], "context": 131072 }
  },
  "roles": {
    "default": { "provider": "example", "model": "example-model" },
    "smart":   { "provider": "example", "model": "example-model" },
    "fast":    { "provider": "example", "model": "example-model" },
    "tiny":    { "provider": "example", "model": "example-model" }
  }
}
```

The optional `roles` block accepts only `default`, `smart`, `fast`, and `tiny`.
Acting sessions use `fast`, planning uses `smart`, and compaction plus
delegated tasks use `tiny`; omitted roles fall back to `defaultModel` and
`defaultProvider`.

`context` is the model's **input** window (context limit); the latest successful
request's provider-reported `PromptTokens + CompletionTokens` drives proactive
compaction against it. The provider's `/models` `context_length` overrides it
when advertised; otherwise ghg uses matching models.dev metadata when available.
`maxOut` (optional) caps **output** tokens; 0 uses the
provider's `max_completion_tokens`, else `context`. The old `maxTokens` field
still parses (it always meant the context window) but is superseded by `context`.

**Catalog models need no config entry.** ghg caches each provider's
`GET /models` (24h TTL in `~/.ghg/models.json`), and any advertised model is
usable directly — `ghg -m deepseek-v4-pro` or `/model deepseek-v4-pro` — with
context, vision, effort levels, and pricing taken from the catalog. Config
entries are authoritative overrides when present. Newly announced models appear
in the `/model` picker (dim, marked `(new)`) after `/model refresh` or the next
TTL cycle. If several providers advertise the same id, pass a provider
(`-p` / `/model <name> <provider>`) to disambiguate.

Any OpenAI-compatible endpoint works through the current compiled adapter. A
provider may reference a reusable profile instead of repeating transport
metadata:

```jsonc
{
  "providers": {
    "lab": {
      "profile": "generic-openai",
      "baseUrl": "http://127.0.0.1:8080/v1",
      "apiKey": "$LAB_API_KEY"
    }
  }
}
```

Built-in profiles include OpenRouter, generic OpenAI-compatible HTTP, Anthropic
Messages, and the two OpenCode Go protocol profiles. Add user profiles under
`~/.ghg/providers/*.yaml` and trusted-project profiles under
`.ghg/providers/*.yaml`; precedence is embedded < user < trusted project.
Profile YAML is strict and contains no credential fields. `base_url` must use
HTTPS, except for explicit loopback HTTP endpoints. The instance's
`apiKeyEnv`/`apiKey` remains the credential source; key resolution is env var →
literal/reference.

## MCP

ghg connects to MCP servers and their tools appear in the agent as
`mcp__<server>__<tool>`. Three config styles all work — ghg reads your
existing setup:

- **claude-style**: a `.mcp.json` in the project root (`{"mcpServers": {...}}`)
- **codex-style**: `[mcp_servers.*]` tables in `~/.codex/config.toml`
- **ghg-native**: an `"mcp"` block in `~/.ghg/config.json` (wins on
  name conflicts):

```json
{
  "mcp": {
    "docs": { "command": ["npx", "-y", "@mcp"], "env": { "API_KEY": "$DOCS_KEY" } },
    "web":  { "url": "https://mcp.example.com/mcp", "headers": { "Authorization": "Bearer $TOKEN" } }
  }
}
```

`/context-doctor` audits what a fresh session injects (skills, MCP tool schemas,
server instructions, built-in tool schemas) with per-source token estimates —
useful when arriving from a heavier harness.

Servers connect in the background at startup and lazily on first use — a
slow or broken server never blocks the loop (calls fail fast with an
actionable message, and dropped sessions auto-reconnect with backoff).
`/mcp` shows live status; `/mcp <name> reconnect|enable|disable` manages
servers without restarting. Server instructions teach the model how to use
each server's tools automatically. CLI: `ghg mcp list|add|remove|import`
(`import [--dry-run]` copies imported servers into ghg's own config), and
`ghg mcp test <name>` to doctor one server (status, timing, tool names,
stderr tail; non-zero exit — validate a `.mcp.json` in CI). `ghg mcp
serve` runs ghg's own tools (read/bash/edit/write) as an MCP server for
other harnesses.

Gate the claude/codex imports with the `"mcpImport"` block — useful when
another app writes MCP entries into `~/.codex/config.toml` you don't want
(blocked servers stay visible in `/mcp` and `mcp list` instead of silently
loading):

```json
{
  "mcpImport": {
    "codex": { "enabled": true, "exclude": ["node_repl"] }
  }
}
```

Per source: `enabled` kills the whole source, `only` is a name allowlist,
`exclude` a denylist (wins over `only`). No block = import everything.

## Docs

How it works, from the top down:

- [architecture.md](architecture.md) — the moving parts and how a
  keystroke becomes a tool call: TUI, agent loop, LLM client, tools, MCP,
  storage. Start here.
- [agent-loop.md](agent-loop.md) — `Agent.Turn` in detail: the
  stream-tools-repeat cycle, parallel tool execution, compaction, steering.
- [concurrency.md](concurrency.md) — the two channel patterns
  behind parallel tool calls (per-path locks) and background subagents.
- [tools.md](tools.md) — the tool set the model gets: bash, file
  tools, subagents, and how schemas are defined.
- [models-providers.md](models-providers.md) — provider routing,
  live model discovery, token/cost bookkeeping.
- [features.md](features.md) — the full feature map, each section
  linked to code and tests.
- [roadmap.md](roadmap.md) — what's shipped vs. what's next,
  cross-referenced to the harnesses that inspired each item.
- [learnings/](learnings/) — exploration reports from other
  harnesses (pi, opencode, exo) that informed the design.
