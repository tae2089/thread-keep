# Project Guidance

Follow the global prompt rules first. This file only adds project-specific skill routing.

## Skill Routing

- When writing, modifying, or reviewing code, apply `coding-quality-guardrails` as the quality gate.
- Use the read-only `overengineering-review` skill before adding another branch after one new abstraction causes three follow-up regressions, or after tests are green and before committing a change that adds a persisted field, interface method, lifecycle state, compatibility branch, destructive-operation safeguard, or more than twice as much test code as production code; edit only when the user explicitly requests simplification.
- When debugging bugs, regressions, flaky behavior, or failing tests, use `diagnosing-bugs` before changing behavior.
- Before implementing new logic with branching, side effects, resource lifecycles, or ordering constraints, use `flow-design` and keep the design note in the task workspace.
- When designing module boundaries, refactoring, or shaping interfaces, use `codebase-design`.
- When aligning terminology or modeling the domain, use `domain-modeling`.
- When a plan is fuzzy, high-impact, or lacks testable acceptance criteria, use `planning-grill` to sharpen scope, acceptance, and failure modes before decomposing it.
- For multi-step or multi-agent work, use `decompose-and-dispatch` to split the work into bounded units. Use `execute-dispatch-unit` only for a clearly assigned unit with scope, dependencies, and verification.
- When preparing context for human or AI code review, use `ready-code-review`; do not use it to perform the review itself.
- To record a session, distill completed work into a replayable recipe, or replay a `recipe.yaml`, use `session-recipe`.

## Project Notes

### Go Declaration Order

- In every Go file, place package-level `const`, `var`, and `type` declarations after imports and before the first function or method declaration.
- Keep functions and methods contiguous after the declaration block; do not interleave package-level types between them.
- Apply this ordering to production and test files. Local declarations inside functions and Go-like text inside test fixtures are unaffected.
