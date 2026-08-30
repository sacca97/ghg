<div align="center">
<pre>
╔═╗ ╦ ╦ ╔═╗
║ ╦ ╠═╣ ║ ╦
╚═╝ ╩ ╩ ╚═╝
Go GHG Go
</pre>
</div>

An LLM tool-use loop (bash / read / write / edit / grep / glob / find_files / lsp / task), an interactive
bubbletea session, and provider-routable models. One binary, no runtime,
config you can read.

## Why ghg

- **Coding agents should be FAST** — literally as fast as possible. ghg is
  built in Go around that constraint: parallel tool calls, streaming
  everything, nothing between you and the model but a loop.
- **Defaults across other harnesses suck.** They all have awesome patterns,
  but none of them bring them all together. ghg cherry-picks the best ideas
  from pi, opencode, codex, and exo into one opinionated coding agent
  (see [docs/roadmap.md](docs/roadmap.md) — every feature cites its source).
- **Go is great for networking-heavy applications**, and harnesses do a whole
  lot of networking. ghg leans on channels where the TypeScript reference
  designs hand-roll promises — per-path file locks and background subagents
  collapse into primitives the compiler checks
  ([docs/concurrency.md](docs/concurrency.md)).
- **ghg is focused on a future where open-source models are the preferred
  models.** Keeping up with those models is hard — ghg brings you an
  opinion on what model you should be using: live discovery from every
  provider's catalog, new models surfaced in the picker, a fast default that
  tracks the frontier.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/sacca97/ghg/main/install.sh | sh
```

Checksum-verified prebuilt binaries (Linux/macOS, x64/arm64). Or from source
(Go ≥ 1.27):

```sh
go install github.com/sacca97/ghg/cmd/ghg@latest
```

Then `ghg` and you're in. Any OpenAI-compatible endpoint works as a
provider. One command wires up
OpenRouter's whole catalog (`/model` lists every model, no per-model
config):

```sh
ghg auth openrouter   # masked key prompt — or /auth openrouter in-session
```

To update to the latest release later, run `ghg update` — it re-runs the
install script above.

## First things to try

```
/plan <text>        propose a read-only plan before execution (/execute to run)
/goal <text>        work until a structured goal update completes it
/detach             detach the TUI while background streams and tasks keep running
/context-doctor     audit what a fresh session injects, in tokens
/model              pick a model — type to filter (new) entries come from the
                    provider catalog, no config needed
```

Drop a `.mcp.json` in your repo and MCP servers just appear (`/mcp` to see
them). ctrl+c once interrupts; twice quits.

## Docs

The full setup, config reference, MCP, and how everything works:
**[docs/README.md](docs/README.md)**.

Highlights:

- [docs/architecture.md](docs/architecture.md) — the moving parts, keystroke
  to tool call
- [docs/agent-loop.md](docs/agent-loop.md) — one loop, one function
- [docs/concurrency.md](docs/concurrency.md) — channels where others use
  promises
- [docs/features.md](docs/features.md) — full feature map, linked to code
  and tests
- [docs/roadmap.md](docs/roadmap.md) — shipped vs. next, sources cited

## Upstream

This repository is a hard fork of [context-labs/whip](https://github.com/context-labs/whip).

- Base commit: `5b8b9d8297184cf69ca34ccd62a4be91457a8bbc`
- Fork module: `github.com/sacca97/ghg`
- Fork binary: `ghg`
- Fork date: 2026-08-26
