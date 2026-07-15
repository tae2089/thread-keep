# Using Thread Keep with Claude Code

This guide shows how to wire Thread Keep into Claude Code so the agent reads existing
code context before it edits, and captures durable decisions as pending context notes
while it works. Everything the agent writes stays a draft; a human always promotes it.

## 1. Prerequisites

Install the core package, then initialize and index the repository before connecting
an agent:

```bash
python3 -m pip install thread-keep
which thread-keep thread-keep-mcp
```

Use `where thread-keep` and `where thread-keep-mcp` on Windows. A Thread Keep source
checkout can use `make build` instead.

```bash
# from the repo you want to work in
thread-keep init
# commit your source changes first — update rejects a dirty worktree
thread-keep update
thread-keep status
```

If `update` reports a language as `missing_pack`, install the language pack before you
rely on cross-language search (see the
[Quickstart](quickstart.md#install-thread-keep)). Go is always indexed.

## 2. Register the MCP server

`thread-keep-mcp` speaks the Model Context Protocol over stdio. Register it with Claude
Code:

```bash
claude mcp add thread-keep -- thread-keep-mcp
claude mcp get thread-keep
```

Every tool accepts an optional `repo` worktree path. If one repository should be
the default, append `--repo /path/to/repo`; an explicit tool-call `repo` always
overrides that default.

Use `claude mcp list` to inspect all configured servers. The current command and
scope options are documented in the official
[Claude Code MCP guide](https://docs.anthropic.com/en/docs/claude-code/mcp).

Alternatively, commit a project-scoped `.mcp.json` at the repo root so every collaborator
gets the same server:

```json
{
  "mcpServers": {
    "thread-keep": {
      "command": "thread-keep-mcp",
      "args": ["--repo", "."]
    }
  }
}
```

### The 10 tools

The server exposes exactly ten tools. Eight are read-only and return the same JSON
structures as the CLI; two are writes. Every tool accepts optional `repo`, which
is required at call time when the server has no `--repo` default.

**Read**

| Tool | Purpose |
| --- | --- |
| `search` | Search indexed code entities and committed notes with lexical evidence (requires `query`). |
| `context_get` | Read the active context notes bound to one `entity_key`. |
| `context_for_change` | Assemble bounded context for changes since an immutable context snapshot. |
| `context_for_entity` | Assemble bounded current or historical context for one `entity_key`. |
| `context_query` | Assemble bounded context from lexical entity and note evidence. |
| `related_context` | Bounded one-hop structural view: owner type and same-file entities only. No call, import, or impact edges. |
| `status` | Working-set status: pending notes, coverage, source state. |
| `diff` | All pending context changes awaiting an explicit human commit. |

**Write**

| Tool | Purpose |
| --- | --- |
| `note_add` | Draft one evidence-backed pending note bound to an entity (`entity_key`, `kind`, `body`; optional `author` and `topics`). |
| `note_revise` | Draft a pending successor revision for a committed note (`note_id`, `body`; optional `author` and replacement `topics`). |

`kind` is one of `intent`, `decision`, `constraint`, `example`, `warning`.

### Safety model

The write path is deliberately narrow:

- `note_add` and `note_revise` only create **pending** drafts. They never commit.
- The origin is always recorded as `agent`. The input schema has **no** `origin` field,
  so an agent cannot forge a human origin.
- Nothing an agent does over MCP can commit context, touch source files, or reach a
  remote. Promotion is always an explicit human `thread-keep commit`.

## 3. Make the agent actually use it

Registering the server is not enough — the agent needs instructions. Paste a block like
this into your project's `CLAUDE.md` so the agent reaches for Thread Keep at the right
moments:

```markdown
## Code context (Thread Keep)

Before modifying an unfamiliar function, method, or type:
- Call `search` for the symbol, then `context_get` on the entity key to read any
  existing intent, decisions, constraints, and warnings.

When you make a durable decision or discover a constraint while working:
- Draft a note with `note_add` at the moment it happens — pick the right `kind`
  (intent / decision / constraint / example / warning).
- Only draft evidence-backed notes: cite the diff, a test, an issue, or an explicit
  user statement in the body. Do not draft change-logs or restate what the code
  plainly shows.

Before adding a note, `search` for an existing one on the same entity. If one exists,
prefer `note_revise` over creating a near-duplicate.

Never assume your drafts are canonical — they stay pending until a human commits them.
```

## 4. The bundled draft skill

The repository ships an agent skill at `.agents/skills/thread-keep-context-draft`. To use
it inside a project, copy that directory into the project's agent skills directory:

```bash
cp -R .agents/skills/thread-keep-context-draft \
  /path/to/project/.agents/skills/thread-keep-context-draft
```

The skill drives a CLI-based drafting flow: it runs `thread-keep --json status`, searches
and reads the target entity, then adds evidence-backed pending notes and shows the diff.
It never modifies source, installs hooks, calls a model, or commits.

**When to use which:**

- **Skill** — end-of-session or batch drafting over a set of changed entities, driven
  through the CLI.
- **MCP** — in-flow capture: the agent records a decision the moment it makes one, mid-edit.

## 5. Hooks (session-end drafting)

Ready-to-copy scripts for everything in this section live in
[`examples/hooks/`](../../examples/hooks/README.md), including an opt-in headless
draft pass with its recursion guard.

Hooks can keep the index fresh and nudge drafting, but the project has hard rules:

- A hook must **never block** a Git commit or push.
- **Never** call a model synchronously inside `pre-commit` / `pre-push`.
- **Never** auto-commit context.
- Always use `--json` so output is versioned and errors carry a stable code.

### Claude Code `Stop` hook (best-effort reminder)

Claude Code hook events live in `.claude/settings.json` under `"hooks"`; the relevant
ones here are `PostToolUse`, `Stop`, and `SessionEnd`. Below is a minimal example — adapt
paths and matchers to your setup.

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": ".claude/hooks/thread-keep-draft.sh" }
        ]
      }
    ]
  }
}
```

A small, non-blocking script that only reminds (or kicks off a headless draft pass) when
the source is clean and entities are indexed:

```bash
#!/usr/bin/env bash
# .claude/hooks/thread-keep-draft.sh — best-effort, never blocks
status="$(thread-keep --json status 2>/dev/null)" || exit 0
# Inspect $status (e.g. with jq): only proceed when the worktree is clean and
# entities are indexed. Then print a reminder, or trigger a headless draft pass.
echo "Thread Keep: source clean and indexed — review pending drafts with 'thread-keep diff'."
exit 0
```

Keep it best-effort: exit `0` on anything unexpected so the agent's stop is never held up.

### Git `post-commit` hook (keep the index fresh)

Refresh the index asynchronously after each commit so `update` never delays the commit
itself. Background it and send output to a log:

```bash
#!/usr/bin/env sh
# .git/hooks/post-commit — async, non-blocking index refresh
( thread-keep --json update >> .git/thread-keep-update.log 2>&1 & )
exit 0
```

## 6. Daily review routine

Agent drafts are proposals. A human reviews and promotes them:

```bash
thread-keep diff                                   # see agent-drafted pending notes
thread-keep commit -m "Record payment authz constraint" --author "Jane Dev"
```

`commit` promotes the complete pending set. The current CLI does not provide a
selective pending-draft edit or discard command, and `note revise` only accepts a
previously committed note. If any draft is unacceptable, do not commit the set;
this is a current local-workflow limitation rather than an implicit approval path.

When a source update changes a bound entity, its binding goes `needs_review`. Confirm the
reviewed binding against the current entity key:

```bash
thread-keep note review <note-id> --entity <current-entity-key>
```

## 7. Troubleshooting

**Tools return `validation: repo is required when --repo is not set`.** Pass an
absolute Git worktree path as the tool's `repo`, or register the server with a
`--repo` default.

**Tools return `repository_state`.** The selected tool-call `repo` (or `--repo`
default) is not a Git worktree. An explicit tool-call path does not fall back to
the process default.

**Tools return `not_initialized`.** Context storage does not exist yet. Run
`thread-keep init`, then `thread-keep update` (after committing source), in the repo.

**Notes rejected with `entity_not_found`.** The `entity_key` must match an indexed entity
exactly. Find valid keys with `search` (MCP) or `thread-keep search <query>` (CLI), then
use the returned key. If the entity truly is not indexed, re-run `thread-keep update`.
