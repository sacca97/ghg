---
name: golang-how-to
description: "Golang skills orchestrator — always active on any Go coding, review, debug, or setup task. Routes to the right golang-* skill and names the boundary when two overlap."
user-invocable: true
license: MIT
compatibility: Designed for Claude Code, Codex or similar harness. Requires git.
metadata:
  author: samber
  version: "1.4.0-ghg"
  note: "Trimmed for this repo: routing rows for libraries absent from go.mod were removed along with their skills. Upstream: https://github.com/samber/cc-skills-golang"
  openclaw:
    emoji: "🧭"
    homepage: https://github.com/samber/cc-skills-golang
    requires:
      bins:
        - go
        - gopls
    install:
      - kind: go
        package: golang.org/x/tools/gopls@latest
        bins: [gopls]
allowed-tools: Read Edit Write Glob Grep Bash(go:*) Bash(git:*) Agent AskUserQuestion LSP Bash(gopls:*) mcp__gopls__*
---

**Persona:** You are a Go skills orchestrator. A task rarely belongs to one skill — load the primary and every applicable secondary together, at the start, without waiting.

**Repo context:** this module is stdlib + charmbracelet + `modelcontextprotocol/go-sdk` + `modernc.org/sqlite`, built `CGO_ENABLED=0`. It uses no DI container, no cobra/viper, no testify, no ORM, no gRPC/GraphQL. Skills for those were removed rather than kept as dead routes. Surviving skills that point at one are marked "(upstream skill, not installed here)" inline; their `references/` files were left untouched, so treat any `golang-*` name there that has no local directory as upstream history. Adding one of those dependencies is a `plan.md` decision, not a skill lookup.

## Skill loading

| Intent | Primary | Also load |
| --- | --- | --- |
| Design an API, choose a pattern | `golang-design-patterns` | `golang-structs-interfaces`, `golang-naming` |
| Name a type, function, or package | `golang-naming` | `golang-code-style` |
| Handle errors idiomatically | `golang-error-handling` | `golang-safety` (nil-heavy code) |
| Write goroutines, channels, sync | `golang-concurrency` | `golang-context` (if cancellation) |
| Pass deadlines / cancel operations | `golang-context` | `golang-concurrency` (if goroutines) |
| Design structs, embed, use interfaces | `golang-structs-interfaces` | `golang-design-patterns` |
| Build or change the bubbletea TUI | `bubbletea-tui` | `golang-concurrency`, `golang-testing` |
| SQLite queries, transactions, schema | `golang-database` | `golang-error-handling`, `golang-security` |
| Build or extend the CLI surface | `golang-cli` | `golang-testing` |
| Write tests | `golang-testing` | `golang-concurrency` (if `-race` matters) |
| Apply optimization patterns | `golang-performance` | `golang-benchmark` (measure first) |
| Measure with pprof / benchstat | `golang-benchmark` | `golang-performance` (fix), `golang-troubleshooting` (root cause) |
| Debug a panic or unexpected behavior | `golang-troubleshooting` | `golang-safety`, `golang-benchmark` (if perf-related) |
| Instrument for production | `golang-observability` | `golang-performance` (if SLO breach) |
| Audit security vulnerabilities | `golang-security` | `golang-safety`, `golang-lint` |
| Review formatting and style | `golang-code-style` | `golang-naming`, `golang-lint` |
| Refactor or restructure existing code | `golang-refactoring` | `golang-naming`, `golang-code-style`, `golang-project-layout` |
| Configure golangci-lint | `golang-lint` | `golang-code-style` |
| Write godoc / README / CHANGELOG | `golang-documentation` | `golang-naming` |
| Set up or reorganize package layout | `golang-project-layout` | `golang-design-patterns`, `golang-lint` |
| Set up CI/CD pipeline | `golang-continuous-integration` | `golang-lint`, `golang-security` |
| Consider a library | `golang-popular-libraries` | `golang-dependency-management` — but see the repo context above |
| Look up a package's docs, versions, importers, or CVEs | `golang-pkg-go-dev` | `golang-dependency-management` |
| Navigate, diagnose, or refactor local code | `golang-gopls` | — |
| Adopt new Go language features | `golang-modernize` | `golang-lint` |

Non-Go work in this repo (feature design, roadmap triage, minimalism) belongs to `new-feature-development` and `ponytail`, not to this table.

## gopls vs godig vs govulncheck

Three tools, three scopes — they do not overlap:

- **`gopls`** (→ `golang-gopls`) answers questions about **your build**: your code plus dependencies exactly as pinned in `go.sum`, `replace` directives included. Definitions, references, `documentSymbol`, diagnostics after an edit, safe rename, and a single-shot `go_vulncheck`. Reach for it first for anything local.
- **`godig`** (→ `golang-pkg-go-dev`) answers questions about the **published ecosystem** — any module, whether or not it is in `go.mod` yet. Versions, exported symbols, examples, licenses, `imported-by`, known CVEs for a version in isolation. It never reads your checkout.
- **`govulncheck`** (→ `golang-security`) is the **whole-tree audit**: walks the module's call graph to confirm which known vulnerabilities are actually reachable. The tool of record for CI gates; `gopls`'s `go_vulncheck` is the lighter mid-edit version.

Use Context7 only for a library whose docs are genuinely not on pkg.go.dev.

## Competing clusters — boundary lines

Full boundary tables and routing examples: [disambiguation.md](references/disambiguation.md). Full catalog with "use when" hooks: [by-category.md](references/by-category.md). Both still describe skills that were removed from this repo; treat those rows as history.

- **Performance**: `golang-performance` (optimization patterns) · `golang-benchmark` (measurement) · `golang-troubleshooting` (root cause) · `golang-observability` (always-on production)
- **Errors**: `golang-error-handling` (idioms) vs `golang-safety` (preventing panics)
- **Style**: `golang-code-style` · `golang-naming` · `golang-lint` · `golang-documentation`
- **Package lookup**: `golang-pkg-go-dev` (published) · `golang-gopls` (local build) · `golang-dependency-management` (go.mod) · `golang-security` (whole-tree CVE scan)
- **Type vs architecture**: `golang-structs-interfaces` (type design) vs `golang-design-patterns` (architectural patterns)
- **Goroutine vs cancel**: load `golang-concurrency` + `golang-context` together when cancelling goroutines via context
- **Correctness vs threat**: `golang-safety` (internal bugs) vs `golang-security` (external threats)
- **Features vs rules**: `golang-modernize` (language adoption) vs `golang-lint` (static analysis config)
- **Process vs target shape**: `golang-refactoring` owns the safe, staged *process* of changing existing code; `golang-naming`/`golang-code-style`/`golang-project-layout`/`golang-design-patterns`/`golang-modernize` own what the result should look like. Load `golang-refactoring` alongside whichever owns the target shape.

---

Not exhaustive — refer to individual skill files and the official Go documentation. Upstream issues: <https://github.com/samber/cc-skills-golang/issues>.
