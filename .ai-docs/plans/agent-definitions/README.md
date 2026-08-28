# Declarative agent definitions and planner workflow

Status: COMPLETE

## Objective

Make the planner a normal declarative agent definition and expose the same
plan-then-execute workflow to headless automation without creating an
autonomous replanning loop.

## Delivered

- Strict Markdown definitions load from trusted project `.agents/*.md` and user
  `~/.ghg/agents/*.md` directories.
- Project definitions take precedence over same-named user definitions.
- Unknown tools, malformed frontmatter, invalid roles, empty prompts, and
  unbounded round budgets fail loading.
- The reserved built-in `planner` definition uses the `smart` role, four model
  rounds, `read`/`grep`/`glob`, and the structured `submit_plan` terminal tool.
- TUI `/plan` and headless plan commands share `Agent.RunDefinition` and the
  bounded planner retry path. `/plan` remains plan-only; `/execute` remains the
  explicit action boundary.
- `ghg run --plan-only` plans and exits; `ghg run --plan` plans and then invokes
  the configured `fast` role with seeded todos.
- Headless JSON emits model-call start/end telemetry for route, protocol,
  latency, finish reason, and usage.

## Non-goals

- `@agent` mention syntax and a general custom-agent picker.
- Autonomous plan/execution/replan loops.
- Mutation tools, task delegation, or MCP tools in the built-in planner.
- GOAL persistence and the later Phase 3 LSP additions.

## Verification

- `go test ./internal/llm ./internal/agent ./internal/tui ./cmd/ghg`
- Definition loader, terminal-tool, retry, telemetry, OpenAI completion-tool,
  and headless plan workflow tests cover the new boundaries.
