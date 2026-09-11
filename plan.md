# ghg: Architecture & Roadmap

Goal: A fast, lightweight, Pi/omp-class coding agent that ships as a **single binary with no Node runtime** in `github.com/sacca97/ghg`.

---

## 1. Architectural Principles

1. **CGO-Free & Single Static Binary:** `modernc.org/sqlite`, pure Go networking and process containment. No linked C dependencies.
2. **Deterministic Agent Loop:** Single-threaded execution loop by default; parallel multi-tool dispatches with per-path file locks.
3. **Contained Process Boundaries:** OS-sandboxed child processes (macOS Seatbelt, Linux Bubblewrap) with deny-by-default external network.
4. **Durable Session vs Context Projection:** Session database (SQLite) is the durable ground truth; context window is a bounded, compacted projection.

---

## 2. Completed Milestones

*Full historical design specs are archived in [`.ai-docs/plans/archive/historical-plan-v1.md`](.ai-docs/plans/archive/historical-plan-v1.md) and documented in [`docs/features.md`](docs/features.md).*

| Phase | Description | Status |
| :--- | :--- | :--- |
| **Phase 0** | Fork from `whip`, module rename, CGO removal, drop browser/Node dependencies. | ✅ Complete |
| **Phase 0.5** | Bubbletea TUI with mouse/theme support, multi-provider streaming (Anthropic/OpenAI), shell escapes. | ✅ Complete |
| **Phase 1** | Session persistence (SQLite), fork/rewind/timeline, content-addressed artifact store. | ✅ Complete |
| **Phase 2** | Model roles (`smart`/`fast`/`tiny`), declarative agents (`.agents/*.md`), conversational read-only Plan mode (`/plan`). | ✅ Complete |
| **Phase 2.5** | Native search (`grep`, `glob`, `find_files`), observation-authorized edits with auto-healing fallback. | ✅ Complete |
| **Phase 3** | Execution policy runtime, OS containment (Seatbelt/bwrap), `approve-for-me`, LSP navigation & safe rename, sandboxed `postEdit` hooks. | ✅ Complete |
| **Phase 3.5** | Embedded static base prompt (`cmd/ghg/system-prompt.md`), verify-before-guessing operating guidance. | ✅ Complete |
| **Phase 3.6** | Adaptive context: proactive pressure guard, live token counter, FTS5 session history recall (`history_search`/`history_read`), stream watchdog. | ✅ Complete |
| **Phase 3.7** | Detachable live sessions: local Unix worker socket (`~/.ghg/run/`), clientless execution, `/detach`, `ghg ps/attach/stop`. | ✅ Complete |
| **Phase 3.8** | Cumulative & adaptive compaction, plan runaway guard, frozen per-turn tools, native `rg` backend, bounded search snapshots & bash rolling preview. | ✅ Complete |

---

## 3. Active Roadmap

### Phase 4 — Safe Web Tools, Memory & DAP Debugging

1. **Safe Web Exploration Tools:**
   - `web_fetch`: SSRF-safe HTTP/HTTPS reader (blocks loopback, link-local, cloud metadata; strictly validates redirects and content sizes).
   - `web_search`: Single backend (Brave Web Search API) returning bounded, citation-ready snippets.
2. **Project Memory:**
   - `.ghg/memory.md` for project-scoped durable preferences alongside user-level `~/.ghg/me.md`.
3. **DAP (Debug Adapter Protocol):**
   - External debugger adapters (`dlv dap` for Go, `debugpy` for Python, `lldb-dap` for C/Rust).
   - Shared framing extraction from `internal/lsp` to `internal/wire`.

---

## 4. Triage & Deferred Scope

*The following items are intentionally excluded or deferred to preserve minimalism and the single-binary architecture:*

- **No Desktop Computer Control:** `computer` tools and OS mouse/screenshot drivers are permanently out of scope.
- **No Headless Browser Engine:** Headless Chrome (`chromedp`/`go-rod`) was stripped to maintain CGO-free, lightweight builds.
- **No CGO AST Parsing (Tree-sitter / FFF):** Revisit only if external CLI wrappers (`ast-grep`) or static binaries provide sufficient value without adding CGO build dependencies.
- **No Multi-User Relay / Cloud Backend:** `ghg` remains a private, local-first developer tool.
