# Agent Directives: Minimal Sufficient Execution

## Core Law
Deliver the minimal sufficient solution. Prohibit over-engineering, future-proofing, unnecessary abstractions, and scope creep.

## Workflow
1. **Understand First:** Read the code directly; do not guess or modify blindly.
2. **Minimal Plan:** Define **Goals**, **Non-goals**, **Acceptance Criteria**, and **Out-of-scope files** before coding.
3. **Execution:** Single-threaded by default. Use heavy reasoning only for planning/review, lighter models for execution.

## Safety & Boundaries
- **Irreversible Ops:** Require the user's explicit confirmation codeword. Without an exact match, refuse execution.
- **Allowed Safe Ops:** Rollback/revert, branch switch, moving files to local backup, running tests, read-only analysis.
- **Stop Triggers:** Immediately stop if adding unneeded layers, touching unrelated files, designing for future cases, or expanding test suites.

## Testing Rules
*Tests only verify current changes—never fill historical coverage debt or build future test infrastructure.*
1. Run existing relevant tests first. Add new tests **only** if behavior changed with zero existing coverage or if explicitly requested.
2. **Limit:** Max 1 main path + 1 key failure path. No E2E suites, large snapshots, or new test frameworks.
3. If test code is longer/more complex than the fix, treat as over-engineering and trim.

## Pre-Completion Checklist
- [ ] Intent, acceptance criteria, and non-goals explicitly verified.
- [ ] Minimal files modified; diff is tight with no debug residue.
- [ ] No unrequested abstractions, compatibility shims, or dependencies added.
- [ ] Only necessary, scoped tests executed/added.