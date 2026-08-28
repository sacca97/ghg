# Native local search tools

Branch: `main`
Status: COMPLETE — implementation, focused tests, and repository verification pass.

## What this does

Adds built-in `grep` and `glob` tools so the model can search the workspace
without spawning a shell process. Both tools share a bounded, deterministic
filesystem walker and a small pure-Go `.gitignore` matcher.

## Goal

- Search text with regular expressions and return stable `path:line:match`
  results.
- Find files with slash-aware glob patterns, including `**` for recursive
  matches.
- Honor nested `.gitignore` files, including negation, anchoring, and
  directory-only rules.
- Keep search output, result counts, scanned entries, line memory, and file
  access bounded; honor cancellation at every filesystem boundary.
- Never follow symlinks. Use `os.Root` for directory-relative opens so a
  symlink cannot escape the selected search root.

## Non-goals

- Replacing `bash`, `read`, or the existing TUI file-mention index.
- A full ripgrep-compatible flag surface, binary-content searching, or AST
  search.
- A new ignore-file dependency or a generic search framework.

## Design

`internal/tools/search.go` owns the tool schemas, argument decoding, regex
search, glob matching, output limits, and the filesystem walk. `internal/tools/
ignore.go` parses `.gitignore` files and matches paths relative to each ignore
file's directory. The walker uses `fs.WalkDir` for lexical ordering, loads
ignore rules before descending, skips `.git`, symlinks, and non-regular files,
and prunes directories excluded by effective parent rules; a negation of the
directory itself keeps that subtree traversable, while an ignored parent still
blocks re-inclusion of its descendants, matching Git's directory rule.

Search roots default to the current working directory. An explicitly supplied
`path` may select another existing directory or file; every descendant remains
confined to an `os.Root`, selected symlink paths are rejected, and symlink
entries encountered during traversal are skipped. Explicit files are searched
directly even when their parent tree is ignored. Results are rendered relative
to the working directory when possible and absolute otherwise. Search patterns
are evaluated relative to the selected directory; an explicit file is matched
by its basename.

The built-in `maxOutput` budget remains the final output ceiling. Each tool also
accepts `max_results`, defaults to 1,000, and caps it at 10,000; traversal stops
after 100,000 entries or when the output/result budget is reached, with an
explicit marker instead of silently dropping the rest. Search patterns are
capped at 16 KiB and individual matching lines at 4 KiB in the returned text.

## Prior art

- `plan.md` Phase 1 requires native bounded `grep`/`glob`, nested ignore
  semantics, cancellation, deterministic ordering, and symlink policy.
- `docs/tools.md` defines the built-in tool contract: failures are tool output,
  and read-only tools do not take mutation locks.
- `docs/concurrency.md` establishes the existing tool execution ownership;
  this slice uses no additional goroutine, so cancellation stays on the caller
  context and no new lifecycle is introduced.

## Test plan

- Tool schemas and `All()` registration.
- Regex matches, line numbers, include filters, invalid patterns, no matches,
  output/result limits, and cancellation.
- Glob `*`, `?`, character classes, `**`, deterministic ordering, hidden files,
  ignored files, and symlink skipping.
- Nested `.gitignore` positive rules, `!` negation, leading `/` anchoring,
  directory-only rules, ignored-parent re-inclusion, and malformed rules.
- Workspace-root confinement, explicit file searches, outside-root explicit
  paths, missing paths, binary files, and non-regular files.
- `go test -race ./internal/tools` plus the repository verification gates.

## Documentation plan

- Add the tools to `docs/features.md` and `docs/tools.md`, including their
  limits and ignore/symlink behavior.
- Check the native-search item in `docs/roadmap.md`.
- Keep `plan.md` as the high-level source of truth; do not create a second
  todo store or change existing `todowrite` behavior.

## Tasks

- [x] Implement the pure glob and `.gitignore` matchers.
- [x] Implement bounded, cancellable grep/glob filesystem operations.
- [x] Register the tools and add focused tests.
- [x] Update docs and run formatting, tests, vet, build, and race checks.

## Verification

- `go test ./...`
- `go test -race ./internal/tools`
- `go vet ./...`
- `CGO_ENABLED=0 go build ./...`
- `go mod verify`
- `git diff --check`
