---
name: thread-keep-context-draft
description: Create evidence-backed pending Thread Keep context notes with stable rationale and references for changed Go entities. Use when a code change needs an intent, decision, constraint, example, or warning recorded without source annotations, or when existing committed context needs a successor revision before a human explicitly commits the context change.
---

# Thread Keep Context Draft

Create only reviewable pending notes. Do not infer business intent from symbol names alone.

## Workflow

1. Run `thread-keep --json status` in the target Git worktree.
   - If it is not initialized, tell the user to run `thread-keep init` and `thread-keep update` after committing source changes.
2. Search a concrete symbol or evidence phrase with `thread-keep --json search <query>`.
3. Read the target entity with `thread-keep --json context get <entity-key>` and inspect `thread-keep --json diff` for pending duplicates.
4. Draft a note only when the diff, tests, issue text, or user statement supplies evidence.
   - Split the evidence into atomic durable claims and choose exactly one kind for each claim: `intent`, `decision`, `constraint`, `example`, or `warning`.
   - Read [the note body convention](references/note-body-convention.md) to select only the sections allowed for that kind.
   - If one section contains an independently useful claim of another kind, split it into a separate note instead of embedding it.
   - Omit unsupported sections instead of inventing evidence or emitting empty headings.
5. Choose the write action.
   - If active or pending context already expresses the same knowledge, do not create a duplicate.
   - If a committed note needs updated knowledge, create a pending successor with `note revise`.
   - If the matching note is still pending, preserve it and report that it must be committed or otherwise handled by a human before revision.
   - Otherwise add a new agent-origin pending note.

   ```sh
   thread-keep --json note revise <note-id> \
     --body "<evidence-backed successor>" --origin agent
   ```

   ```sh
   thread-keep --json note add <entity-key> \
     --kind <kind> --body "<evidence-backed note>" --origin agent
   ```

6. Run `thread-keep --json diff` and report the action, pending note ID, entity key, and cited references.

Read [the CLI contract](references/cli-contract.md) when an error code, command argument, or JSON envelope matters.

## Hard boundaries

- Never modify source files, Git history, branches, remotes, or hooks.
- Never run `thread-keep commit`, push, pull, or install a hook.
- Never call a model API or claim that a draft is canonical context.
- Do not add a note when the target entity is absent, the worktree is stale, or the evidence is speculative. Report the missing evidence instead.

## Completion criteria

- Every new or revised note is backed by inspected evidence.
- Every note expresses one primary `NoteKind`; independently durable cross-kind claims were split into separate notes.
- No known duplicate note was created.
- Every cited reference was observed in the supplied or repository evidence.
- The final report includes the pending diff or explains why no draft was written.
