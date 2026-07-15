# Session-end hook examples

Working examples for wiring Thread Keep into the end of a coding session.
Every script follows the project's hard rules for automation:

- **Never block.** Every failure path exits `0`; the agent's stop, the session
  end, and the git commit are never held up by Thread Keep.
- **Never call a model synchronously in `pre-commit`/`pre-push`.** The only
  model call here is a *backgrounded* headless pass at session end, and it is
  opt-in.
- **Never commit context automatically.** Everything an automated pass creates
  is a pending draft; a human promotes it with `thread-keep commit`.
- **Always `--json`** so output is versioned and errors carry stable codes.

## Files

| File | Hook point | What it does |
| --- | --- | --- |
| `claude-code/remind.sh` | Claude Code `Stop` | Prints a reminder when pending context drafts exist. Read-only. |
| `claude-code/draft.sh` | Claude Code `SessionEnd` | Opt-in (`THREAD_KEEP_DRAFT_HEADLESS=1`): after confirming a fresh indexed working set, starts a background headless `claude -p` run that follows the context-draft workflow and adds evidence-backed pending notes with agent origin. |
| `claude-code/settings.json` | — | Hook wiring for the two scripts. |
| `git/post-commit` | git `post-commit` | Refreshes the index (`thread-keep update`) asynchronously after each source commit. |

## Install (Claude Code)

```bash
mkdir -p .claude/hooks
cp examples/hooks/claude-code/remind.sh examples/hooks/claude-code/draft.sh .claude/hooks/
chmod +x .claude/hooks/remind.sh .claude/hooks/draft.sh
# merge examples/hooks/claude-code/settings.json into .claude/settings.json
```

Enable the headless draft pass only where you want it:

```bash
export THREAD_KEEP_DRAFT_HEADLESS=1
```

## Install (git)

```bash
cp examples/hooks/git/post-commit .git/hooks/post-commit
chmod +x .git/hooks/post-commit
```

## The recursion guard, explained

`draft.sh` spawns `claude -p`, and that headless run executes with the same
project hooks — so its own `SessionEnd` would spawn another draft pass,
forever. The script exports `THREAD_KEEP_DRAFT_CHILD=1` into the child; the
first line of the script exits immediately when that marker is present. If you
adapt the script, keep the guard.

## What the draft pass may and may not do

The prompt embedded in `draft.sh` allows only `thread-keep`, `git log`, and
`git diff` commands and instructs the agent to draft notes solely where the
diff, a test, or an issue supplies evidence — no change-logs, no restating
code. Its hard limits mirror the bundled skill
(`.agents/skills/thread-keep-context-draft`): no `commit`, no push/pull, no
source edits. Review results the next morning with `thread-keep diff`.
