#!/usr/bin/env sh
# Thread Keep session hook — pending-draft reminder.
#
# Wire this as a Claude Code "Stop" hook (see settings.json in this
# directory). It is best-effort by design: every failure path exits 0 so the
# agent's stop is never held up, and nothing here mutates any state.

command -v thread-keep >/dev/null 2>&1 || exit 0
status="$(thread-keep --json status 2>/dev/null)" || exit 0

case "$status" in
*'"pending_notes":0'*)
	;;
*'"pending_notes"'*)
	echo "Thread Keep: pending context drafts exist — review with 'thread-keep diff', then promote with 'thread-keep commit'."
	;;
esac
exit 0
