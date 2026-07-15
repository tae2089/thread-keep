#!/usr/bin/env sh
# Thread Keep session hook — opt-in headless draft pass.
#
# Wire this as a Claude Code "SessionEnd" hook (see settings.json in this
# directory). When the session that just ended changed source, it starts a
# BACKGROUND headless agent run that follows the bundled context-draft
# workflow: read the recent diff, search existing context, and add
# evidence-backed PENDING notes with agent origin. It never commits context,
# never touches source, and never blocks the session from ending.
#
# Opt in per machine or per shell:
#   export THREAD_KEEP_DRAFT_HEADLESS=1
#
# Recursion guard: the spawned `claude -p` run executes with this project's
# hooks too, so its own SessionEnd would fire this script again. The
# THREAD_KEEP_DRAFT_CHILD marker is exported into the child; when the child's
# hook sees it, it exits immediately.

[ -n "${THREAD_KEEP_DRAFT_CHILD:-}" ] && exit 0
[ "${THREAD_KEEP_DRAFT_HEADLESS:-0}" = "1" ] || exit 0
command -v thread-keep >/dev/null 2>&1 || exit 0
command -v claude >/dev/null 2>&1 || exit 0

# `status` is diagnostic and succeeds for stale indexes. Require a readable,
# clean Git worktree, then use `search` to exercise the working-set freshness
# guard before the background agent is started.
worktree_status="$(git status --porcelain 2>/dev/null)" || exit 0
[ -z "$worktree_status" ] || exit 0
thread-keep --json status >/dev/null 2>&1 || exit 0
thread-keep --json search __thread_keep_freshness_probe__ >/dev/null 2>&1 || exit 0

log="${TMPDIR:-/tmp}/thread-keep-draft.log"
prompt='Follow the Thread Keep context draft workflow for this repository:
1. Run `thread-keep --json status`; stop if the repository is not initialized, not indexed, dirty, or stale against Git HEAD.
2. Inspect the most recent source change with `git log -1 --stat` and `git diff HEAD~1`.
3. For each changed entity, run `thread-keep --json search <symbol>` and `thread-keep --json context get <entity-key>` to read what is already recorded.
4. Add a pending note with `thread-keep --json note add <entity-key> --kind <intent|decision|constraint|example|warning> --body "..." --origin agent` ONLY where the diff, a test, or an issue supplies evidence. Prefer `note revise` when a note already covers the entity. Do not record change-logs.
5. Finish by running `thread-keep --json diff` and summarizing the drafted note IDs.
Hard limits: never run `thread-keep commit`, never push or pull, never modify source files.'

(
	THREAD_KEEP_DRAFT_CHILD=1 claude -p "$prompt" \
		--allowedTools "Bash(thread-keep *),Bash(git log *),Bash(git diff *)" \
		>>"$log" 2>&1 &
)
echo "Thread Keep: headless draft pass started in the background (log: $log)."
exit 0
