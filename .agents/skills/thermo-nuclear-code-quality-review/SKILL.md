---
name: thermo-nuclear-code-quality-review
description: Run an extremely strict maintainability review for abstraction quality, giant files, and spaghetti-condition growth. Use for a thermo-nuclear code quality review, thermonuclear review, deep code quality audit, or especially harsh maintainability review.
disable-model-invocation: true
---

# Thermo-Nuclear Code Quality Review

Perform a review-only, unusually strict maintainability audit of the current
changes and the surrounding code needed to understand them. Do not modify code
unless the user explicitly asks for fixes.

Be ambitious about structural simplicity. Look for "code-judo" changes that
preserve behavior while deleting concepts, branches, modes, helpers, or layers.
Do not demand a grand redesign when no materially simpler alternative is clear.

## Review Standards

- Treat new ad-hoc conditionals, scattered feature checks, nullable modes, and
  one-off flags as likely design problems. Prefer a simpler state model or a
  canonical ownership boundary over centralizing the same complexity.
- Flag a change that pushes a file from below 1,000 lines to above 1,000 lines
  unless there is a compelling structural reason. Ask whether it should be
  decomposed before more behavior accumulates there.
- Keep logic in the layer that owns the concept and reuse established helpers
  and contracts. Flag feature leakage, near-duplicate utilities, and incidental
  coupling between otherwise separate modules.
- Prefer direct, typed, boring code over magic, casts, loose object shapes,
  silent fallbacks, pass-through wrappers, and abstractions that do not reduce
  cognitive load.
- Distinguish simplification from relocation. Splitting or extracting code is
  useful only when it improves cohesion, ownership, or the number of concepts a
  reader must hold at once.
- Flag non-atomic related updates and unnecessary sequential orchestration when
  a clearly simpler and correct structure is available. Do not turn this into
  speculative concurrency or micro-optimization advice.

For every meaningful change, ask whether it worsens local architecture,
branching, coupling, statefulness, ownership, type boundaries, or scanability—and
whether a plausible reframing would remove that cost rather than disguise it.

## Findings

Prioritize findings in this order:

1. Structural regressions and credible complexity-deleting alternatives.
2. Spaghetti control flow, misplaced ownership, and boundary/type problems.
3. Unjustified file growth or missing decomposition.
4. Abstractions and implementation choices that materially hurt legibility.

Report a small number of high-conviction, actionable findings rather than many
cosmetic nits. For each finding, identify the relevant code, explain the
concrete maintenance cost, and describe the simpler structural direction.

## Tone

Be direct and demanding without being rude. Examples:

- `this pushes the file past 1k lines. can we decompose it first?`
- `this adds another special-case branch to an already busy flow; can the state model remove it?`
- `this abstraction adds indirection without reducing complexity; keep the direct flow.`
- `this moves complexity around but does not delete it; is there a simpler ownership boundary?`

## Approval Bar

Do not approve merely because behavior is correct or tests pass. Treat these as
presumptive blockers when a clearer alternative is visible:

- a structural regression or avoidable growth in branching and state;
- a plausible code-judo simplification that would delete substantial incidental complexity;
- a file crossing the 1,000-line threshold without strong justification;
- feature logic scattered through shared paths or placed outside its canonical layer;
- duplicated canonical logic, unnecessary indirection, or a cast-heavy/loosely typed contract;
- partial state updates that can leave the system incoherent.

If no such problem exists, say so clearly. Do not manufacture findings to
justify the skill's strictness.
