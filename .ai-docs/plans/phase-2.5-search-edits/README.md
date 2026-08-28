# Phase 2.5 search and observed edits

Status: implementation complete; Phase 3 checkpoint commit pending (2026-08-28)

This slice replaces unbounded exploration and fragile edit strings with:

- bounded, grouped native search with stable cursors and shared fuzzy path ranking;
- request-scoped search/observation state that can mirror into a session;
- complete-line read observations and explicit exact/observed edit modes;
- deterministic, permission-first, atomic multi-file publication;
- tool-output telemetry and prompt guidance that agree with the tool surface.

Stabilization gate implementation:

- search pages select complete rendered entries under the 8 KiB preview ceiling;
  cursors and displayed/remaining metadata advance only over entries actually
  returned, including ungrouped later pages;
- compaction, title generation, `/goal-from-context`, and declarative one-shot
  calls share route-aware model-call telemetry, including the configured tiny
  provider and adapter for summaries;
- observed edits accept complete lines returned by byte-limited reads, relocate
  unchanged authorized ranges after unrelated shifts, and retain strict stale,
  ambiguous, overlap, permission, and cross-session rejection;
- deterministic acceptance coverage exercises all edit operations, publication
  rollback, sorted locking, line-ending preservation, bounded output, search
  pagination/byte ceilings, and task settlement ordering through persistence
  callbacks.

Verification:

- `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`,
  changed-file `gopls check`, and `CGO_ENABLED=0 go build` pass; the checkpoint
  commit remains pending.

The parent plan remains authoritative; this checkpoint records the completed slice.
