# Compaction model default + adjustable compaction threshold

Status: COMPLETE — the Phase 2 `tiny` role now supplies compaction when a
roles block is configured; the built-in model remains the legacy fallback.

## What this does

Two user-facing changes, both in the ctrl+p settings:

1. **Compaction model defaults to the configured `tiny` role** when the Phase 2
   roles block is present. A legacy config without roles retains
   `deepseek-v4-flash-0731` as its built-in fallback. `""` continues to mean
   "follow the default"; the panel and `/compact off` restore that selection.
2. **Compaction level becomes a percentage preference**, default **50%** of the
   model's context window (was hardcoded 90%). New settings row "Compaction
   level" (category "Session") with ←/→ stepping ±10% (range 10–90), persisted
   as `compactPct` in config. `maybeCompact` compacts once the estimate crosses
   that fraction of `ContextLimit`, making compaction deterministic instead of
   context-bloating-at-90%.

## Goal

- Summaries run on the configured `tiny` model by default, regardless of the
  conversation's model; legacy configs use DeepSeek V4 Flash.
- Auto-compaction fires at 50% by default; users slide it in ctrl+p.

## Non-goals

- New slash command for the threshold (settings + config key only).
- Changing `compactKeepBack`, summary prompt, or the reactive
  context-length-exceeded retry.
- Token-counting the agent → the threshold stays estimate-based
  (`EstimateTokens`), which is the existing contract.

## Design

**Surfaces:** config (`internal/config/config.go`), agent loop
(`internal/agent/agent.go`), TUI (`internal/tui/tui.go`,
`internal/tui/settings.go`).

- `config.Config`: add `CompactPct int json:"compactPct,omitempty"` (percent,
  0 = default 50; validated/clamped 10–90).
- `config.Default()`: `CompactModel: "deepseek-v4-flash-0731"`.
- `config`: `const DefaultCompactPct = 50`, `const DefaultCompactModel =
  "deepseek-v4-flash-0731"`.
- `agent.Agent`: replace package const `compactThreshold = 0.9` with a field
  `CompactThreshold float64` (fraction of ContextLimit; 0 = default 0.5).
  `maybeCompact` uses `a.threshold()`.
- TUI wiring: `compactThresholdFor(cfg)` → clamped fraction; set on the agent
  at startup (`initialModel` block), `switchModel`, `previewModel` (agent is
  rebuilt there), and the resume path (tui.go:474). Store the *fraction* on
  the agent so no percent plumbing into the loop.
- settings:
  - New `setCompactPct(pct)` helper: clamp 10–90, update agent + cfg + Save,
    transcript note.
  - New row "Compaction level": dynDesc shows `compact at NN% of context`,
    dynHint `←/→`, stepBack/stepFwd ±10%.
  - `paletteState` badge `[NN%]`.
  - panelCompact: "current" entry → the configured tiny model (or
    "default (deepseek-v4-flash-0731)" for a legacy config) — the label for
    "" (follow the default automatically), not a frozen name.
- `/compact off` restores the configured `tiny` role (or the legacy built-in
  default) and labels the active fallback.

## Prior art

Roadmap line 54 (compaction) and line 108 (settings) — extend both; checkboxes
already ticked, so update `docs/features.md` sections instead.

## Test plan

- `agent_test.go`: rework `TestProactiveCompactAtNinetyPercent` → fifty
  percent default; add explicit-threshold + clamp cases
  (pure `compactThresholdFor` unit test).
- `compact_cmd_test.go`: stepper test (setCompactPct clamps at 10/90, persists
  `cfg.CompactPct`, updates `agent.CompactThreshold`).
- `palette_test.go`: existing panelCompact test still passes; "current" →
  relabeled default row still applies "" on select.
- Everything under `task check` + `go test -race ./...` (agent field touched).

## Docs plan

- `docs/features.md`: update the compaction + settings sections (threshold %,
  default compaction model).
- README: `/compact` help line already documents the model picker; update the
  "90%" mention in `/help` text and features doc.

## Tasks

1. config: DefaultCompactPct/DefaultCompactModel consts, CompactPct field,
   Default() compaction model. ✅
2. agent: CompactThreshold field + threshold() helper; maybeCompact uses it. ✅
3. tui: compactThresholdFor, wire into all four agent-build sites,
   setCompactPct helper, settings row + state + panel relabel, /compact off text. ✅
4. Tests (above). ✅
5. `task check`, `go test -race ./...`, docs/features.md update. ✅
6. Adversarial review pass: fixed the stale ppanel comment, made compactPct()
   read the authoritative cfg int, dropped the per-step transcript note.
   Kept `CompactModel` explicit in Default() (self-documenting; the "" path
   still resolves dynamically). ✅
