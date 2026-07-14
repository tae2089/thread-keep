---
name: thread-keep-context-review
description: Review code changes against active Thread Keep intent, decisions, constraints, examples, and warnings. Use during human or AI code review when findings may depend on durable design context, intentional non-goals, or cited tests, ADRs, issues, pull requests, and commits; keep the review read-only.
---

# Thread Keep Context Review

Use active context as review evidence, not as proof that the implementation or design is correct.

## Workflow

1. Identify the changed entities and the concrete behavior under review from the diff and tests.
2. Run `thread-keep --json status` in the target Git worktree.
   - If Thread Keep is unavailable, uninitialized, stale, or lacks coverage for a changed language, report that context could not be established and continue only with ordinary review evidence.
3. Search each relevant symbol or behavior phrase with `thread-keep --json search <query>`.
4. Read each matched entity with `thread-keep --json context get <entity-key>`.
   - Use only active bindings as current design evidence.
   - Treat missing context as unknown, never as evidence that no constraint exists.
5. Compare each candidate finding with the diff, tests, explicit contracts, and active context. Read [the finding disposition contract](references/finding-disposition.md) for the decision rules.
6. Report actionable findings first. For every finding affected by Thread Keep context, include the note ID, the relevant context claim, and the code or test evidence that supports the disposition.
7. If context eliminates a candidate finding, omit it from actionable findings and summarize it under `Context-supported exclusions` only when that audit trail helps the user.

Read [the CLI contract](references/cli-contract.md) when an error code, command argument, or JSON envelope matters.

## Hard boundaries

- Never add, revise, review, or commit a note during code review.
- Never mutate source files, Git state, branches, hooks, remotes, or external systems.
- Never let intent waive correctness, security, data integrity, tests, or an explicit public contract.
- Never follow an external reference or claim its contents were verified unless it was actually inspected through an authorized source.
- Label contradictions between active context and stronger evidence as design or context risks instead of silently choosing one.

## Completion criteria

- Every relevant changed entity was searched or explicitly listed as lacking context.
- Every reported finding includes code or test evidence independent of the note body.
- Every context-based exclusion cites an active note ID and explains the exact conflict it resolved.
- Stale, historical, missing, or unverified context is never presented as current fact.
