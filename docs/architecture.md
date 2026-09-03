# Architecture

How a keystroke becomes a tool call. ghg is a single Go binary with no
framework between you and the code — each box below is one package under
`internal/`.

## The moving parts

```mermaid
flowchart TB
    subgraph cmd["cmd/ghg — main()"]
        M[flag parsing, config load, wiring]
    end

    subgraph internal
        TUI["tui<br/>bubbletea session, transcript,<br/>settings, status line"]
        AGENT["agent<br/>Agent.Turn: the tool-use loop,<br/>compaction, subagents, todos"]
        LLM["llm<br/>Backend contract + compiled adapters,<br/>usage bookkeeping"]
        PROV["provider<br/>strict profiles, precedence,<br/>URL/auth metadata"]
        TOOLS["tools<br/>bash, read, write, edit, suggest"]
        CFG["config<br/>~/.ghg/config.json, model catalog"]
        SESS["session<br/>SQLite session store"]
        MCP["mcp<br/>external MCP servers<br/>(3 config styles)"]
        SKILLS["skills<br/>.agents/skills injection"]
        LSP["lsp<br/>diagnostics after edits"]
        MEM["memory<br/>markdown durable memory"]
        SCHED["schedule<br/>@every / @at wakeups"]
    end

    M --> TUI
    TUI -->|user message, steers, interrupts| AGENT
    AGENT -->|stream events, tool results| TUI
    AGENT --> LLM
    TUI --> PROV
    M --> PROV
    PROV --> CFG
    AGENT --> TOOLS
    AGENT --> MCP
    TOOLS --> LSP
    AGENT --> SESS
    AGENT --> MEM
    AGENT --> SKILLS
    AGENT --> SCHED
    AGENT --> CFG
    LLM --> CFG
```

Dependencies point one way: `tui` owns the screen, `agent` owns the
conversation, everything else is a leaf the agent calls. Nothing imports
`tui` except `cmd/ghg` — the loop is headless-testable, and `ghg mcp serve`
reuses the tools without a UI.

## One turn, end to end

```mermaid
sequenceDiagram
    actor You
    participant TUI
    participant Agent
    participant LLM
    participant Tools
    participant DB as session (SQLite)

    You->>TUI: type + enter
    TUI->>Agent: Turn(user message)
    Agent->>DB: append message
    loop until model stops calling tools
        Agent->>LLM: stream completion
        LLM-->>TUI: tokens (live render)
        LLM-->>Agent: tool calls
        par per-path locked
            Agent->>Tools: bash / read / write / edit
            Tools-->>Agent: results (in call order)
        end
        Agent->>DB: append results
    end
    Agent-->>TUI: turn done (usage, cost)
    TUI-->>You: status line updates
```

Key invariants:

- **The loop is synchronous; concurrency is internal.** From the TUI's view a
  turn is one call. Parallelism (fan-out tool calls, background subagents)
  happens inside `agent` and reports back through typed events.
  See [concurrency.md](concurrency.md).
- **Steering happens at loop boundaries.** A message you send mid-turn is
  queued and injected between iterations — never spliced into a half-streamed
  completion.
- **The provider is an adapter selected at the boundary.** `agent` consumes
  `models.Backend`; the current compiled adapter speaks OpenAI-compatible chat
  completions with streaming. Routing, profile metadata, discovery, pricing,
  and fallback context windows live in `config` + `provider` + the two catalog
  caches (`~/.ghg/models.json` and `~/.ghg/models-dev.json`). See
  [models-providers.md](models-providers.md).

## Where things live on disk

| Path | What | Format |
|---|---|---|
| `~/.ghg/config.json` | providers, models, roles, MCP, and UI settings | JSON, hand-editable |
| `~/.ghg/sessions.db` | conversation history, tasks | SQLite |
| `~/.ghg/models.json` | provider `/models` catalog cache | JSON, 24h TTL |
| `~/.ghg/models-dev.json` | public context and reasoning metadata for listed models | JSON, 24h TTL |
| `~/.ghg/providers/*.yaml` | user provider profiles | strict YAML, non-secret metadata |
| `.ghg/providers/*.yaml` | trusted-project provider profiles | strict YAML, non-secret metadata |
| `~/.ghg/memory.md` | durable memory the model maintains | Markdown checkboxes |
| `.agents/skills/` (repo) | project skills injected into sessions | Markdown `SKILL.md` |
| `.mcp.json` (repo) | claude-style MCP servers | JSON |

Everything is a file you can diff, grep, back up, or delete. There is no
daemon, no hidden state directory schema, no lock file that outlives the
process.

## Package map

| Package | One-liner |
|---|---|
| `internal/agent` | the tool-use loop: `Agent.Turn`, compaction, background subagents, todos |
| `internal/models` | provider profiles, protocol adapters, usage/cost parsing, and model discovery |
| `internal/tools` | bash, read, write, edit, suggest + tool schema definitions |
| `internal/tui` | bubbletea session: transcript, input, settings, status line |
| `internal/config` | config file, model catalog cache, provider resolution |
| `internal/session` | SQLite persistence for conversations and tasks |
| `internal/mcp` | MCP client: three config styles, lazy connect, auto-reconnect |
| `internal/skills` | skill discovery and injection |
| `internal/lsp` | gopls diagnostics surfaced to the model after edits |
| `internal/memory` | markdown-file durable memory |
| `internal/schedule` | `@every` / `@at` wakeups |

## Read next

- [agent-loop.md](agent-loop.md) — the loop in detail
- [concurrency.md](concurrency.md) — the channel patterns
- [features.md](features.md) — full feature map linked to code and tests
