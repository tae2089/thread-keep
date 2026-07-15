# Thread Keep with OpenAI Codex

This guide connects Thread Keep to the Codex CLI, IDE extension, or desktop app
through a local stdio MCP server. These Codex surfaces share MCP configuration for
the same host.

## Prerequisites

- A Git repository where Thread Keep is already initialized and indexed:

  ```bash
  thread-keep init
  # commit your source changes first; update rejects a dirty worktree
  thread-keep update
  ```

- The `thread-keep-mcp` binary on your `PATH` (or an absolute path you can use in
  configuration). The published core package installs both required commands:

  ```bash
  python3 -m pip install thread-keep
  which thread-keep thread-keep-mcp
  ```

  Use `where thread-keep` and `where thread-keep-mcp` on Windows. A source checkout
  can use `make build` instead.

## MCP registration

The simplest setup uses the Codex CLI:

```bash
codex mcp add thread-keep -- thread-keep-mcp
codex mcp list
```

To configure the server by hand, add a block to `~/.codex/config.toml`. A trusted
repository may instead use `.codex/config.toml` for project-scoped configuration:

```toml
[mcp_servers.thread-keep]
command = "thread-keep-mcp"
```

Every tool accepts a `repo` argument. Use an absolute worktree path so a call
does not depend on where Codex launches the MCP server. If one repository should
be the default, add `args = ["--repo", "/absolute/path/to/repo"]`; an explicit
tool-call `repo` overrides that default. If `thread-keep-mcp` is not on `PATH`,
set `command` to its absolute path.

Restart the Codex surface after changing configuration. In the CLI/TUI, use
`/mcp` to confirm the server and its tools are active. For current Codex options,
use `codex mcp --help` and the official
[Codex MCP documentation](https://learn.chatgpt.com/docs/extend/mcp.md).

The server speaks the Model Context Protocol over stdio. On startup it logs `thread-keep-mcp listening on stdio` to stderr — that line is informational, not an error.

## Tool surface and safety model

Thread Keep exposes exactly ten tools. Every tool accepts optional `repo`; it is
required at call time when the server has no `--repo` default.

**Read tools** (return the same JSON structures as the CLI):

| Tool | Purpose |
| --- | --- |
| `search` | Search indexed code entities and committed context notes with lexical evidence. Argument: `query`. |
| `context_get` | Read the active context notes bound to one entity key. Argument: `entity_key`. |
| `context_for_change` | Assemble bounded context for changes since an immutable context snapshot. |
| `context_for_entity` | Assemble bounded current or historical context for one entity. Argument: `entity_key`. |
| `context_query` | Assemble bounded context from lexical entity and note evidence. Argument: `query`. |
| `related_context` | Bounded one-hop structural view: the entity's owner type and same-file entities only. Never call, import, or impact edges. Arguments: `entity_key`, optional `limit` (default 20, max 100). |
| `status` | Working-set status: pending notes, coverage, and source state. No arguments. |
| `diff` | All pending context changes awaiting an explicit human commit. No arguments. |

**Write tools**:

| Tool | Purpose |
| --- | --- |
| `note_add` | Draft one evidence-backed pending note bound to an entity. Arguments: `entity_key`, `kind` (`intent`, `decision`, `constraint`, `example`, `warning`), `body`, optional `author`. |
| `note_revise` | Draft a pending successor revision for a committed note instead of duplicating it. Arguments: `note_id`, `body`, optional `author` and replacement `topics`. |

Safety model:

- Write tools create **pending drafts only**. A drafted note stays pending until a human commits it.
- Every note created through MCP records its origin as `agent` — the tool sets this automatically; there is no origin argument to set.
- There is **no commit, push, or source-mutation surface** over MCP. Nothing Codex does can commit context, edit source files, or reach a remote.
- Promotion is always an explicit human step: `thread-keep commit`, after reviewing `thread-keep diff`.

## Project instructions for Codex (AGENTS.md snippet)

Add this to your project's `AGENTS.md` so Codex uses Thread Keep consistently:

```markdown
## Thread Keep context

- Before editing an unfamiliar function, type, or method, consult `search` and
  `context_get` to read the durable context already recorded for that entity.
- Pass this repository's absolute worktree path as `repo` to every Thread Keep
  tool call unless the MCP server was registered with this repository as `--repo`.
- At decision time, draft evidence-backed notes via `note_add`. Pick the kind
  that fits: `intent`, `decision`, `constraint`, `example`, or `warning`. Every
  note body must cite its evidence — the diff, a test, an issue, or an explicit
  user statement. Do not record change-logs or restate what the code obviously does.
- `search` before adding to avoid duplicates. If a note already covers the entity,
  use `note_revise` to add a successor revision instead of creating a second note.
  The existing note must already be committed.
- Drafted notes are never canonical. Treat them as pending until a human runs
  `thread-keep commit`. Do not rely on your own drafts as established context.
```

## Review at the end of a session

The simplest workflow is to ask Codex directly at the end of a work session:

> Review this session's changes and draft any durable context notes through thread-keep.

The same rules apply: notes must be evidence-backed and must not be change-logs. Codex drafts pending notes only — you still promote them by hand.

Codex also supports trusted project `Stop` hooks, but Thread Keep does not install
or enable one automatically. If you add your own `.codex/hooks.json`, keep it as a
best-effort reminder or validation step and review it through `/hooks`; the human
`diff` and `commit` gate remains unchanged. See the official
[Codex hooks documentation](https://learn.chatgpt.com/docs/hooks.md).

## Human review routine

Drafts accumulate until you review and commit them:

```bash
thread-keep diff              # inspect every pending change
thread-keep commit -m "Capture <topic> context"  # promote pending notes
thread-keep log               # confirm the new context commit
```

`commit` promotes the complete pending set. The current CLI does not provide a
selective pending-draft edit or discard command. `note revise` creates a successor
for a previously committed note; it does not edit an uncommitted draft. If any
draft is unacceptable, do not commit the set.

When a source update changes a bound entity, Thread Keep records a pending `needs_review` (or `historical`) binding instead of silently carrying the old context forward. `diff` surfaces these. Confirm a reviewed binding explicitly before committing:

```bash
thread-keep note review <note-id> --entity <current-entity-key>
```

## Troubleshooting

- **`validation: repo is required when --repo is not set`.** Pass an absolute Git worktree path as the tool's `repo`, or register the server with a `--repo` default.
- **`repository_state` error.** The selected tool-call `repo` (or `--repo` default) is not a Git worktree. An explicit tool-call `repo` never falls back to the process default.
- **`not_initialized` error.** The repository has no Thread Keep context storage yet. Run `thread-keep init` (then `thread-keep update`) in that repo.
- **`entity_not_found` error.** The `entity_key` you passed to `context_get`, `related_context`, or `note_add` does not match an indexed entity. Run `search` first to find the exact entity key, then retry with that key. If the entity is new, run `thread-keep update` (on a clean, committed worktree) so it gets indexed.
- **The server is configured but tools do not appear.** Run `codex mcp list`,
  restart the Codex client, then use `/mcp` to inspect the active server. For a
  project-scoped `.codex/config.toml`, make sure the project is trusted.
