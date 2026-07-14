# Thread Keep Quickstart

Thread Keep is a local-first code-context VCS. It preserves the "why" behind your
code as immutable, entity-bound context notes stored beside your repository — never
inside your source files. Git stays the source of truth for source code, commits,
and branches; Thread Keep versions the intent, decisions, constraints, examples, and
warnings that Git alone can't capture.

This guide gets you from zero to a committed, searchable context note in about
15 minutes.

## Build and install

Thread Keep uses CGO-backed SQLite (FTS5), so you need a Go toolchain and a C
compiler. From the repository root:

```bash
make build
```

This produces three binaries in `bin/`:

- `thread-keep` — the CLI you use day to day.
- `thread-keep-server` — the self-hosted context-remote server (see
  [team-server.md](team-server.md)).
- `thread-keep-mcp` — the MCP server that exposes context to coding agents (see
  [claude-code.md](claude-code.md) and [codex.md](codex.md)).

Put `bin/thread-keep` on your `PATH`, or call it by path as `./bin/thread-keep`.

Go is indexed out of the box. TypeScript/JavaScript, Python, Java, Kotlin, and Rust
need explicitly installed language packs:

```bash
make build-pack
```

Then place each built pack executable at its fixed path under your user config
directory (for example `$XDG_CONFIG_HOME/thread-keep/packs/...`). Run
`thread-keep indexers list` to see which packs are built in, installed, or missing,
and which languages your current repo uses. Without a detected language's pack, Go
stays searchable and that language reports `missing_pack`.

## Your first 15 minutes

Work inside a real Git repository. Every command below was run against a small Go
package with a single `Run` function.

### 1. Initialize context storage

```bash
thread-keep init
```

```
ok
```

`init` is required before any other local context command. It stores everything
under `thread-keep/` inside the Git common directory — your source tree is untouched.

### 2. Commit your source first, then index

`update` only accepts a **clean, committed** worktree, so the indexed entities and
the recorded source SHA always describe the same revision. Commit your code in Git,
then:

```bash
thread-keep update
```

```
indexed 1 entities at bd362ea576f509debf7f56480716150ebde3b25d
coverage complete: true
coverage: go	indexed	builtin/go
```

If the worktree is dirty, `update` refuses with a `repository_state` error — commit
or stash your source changes first.

### 3. Add a context note

A note attaches to an indexed entity (a function, method, or type) by its key.
Choose one of five kinds:

| Kind | Use it for |
| --- | --- |
| `intent` | Why this code exists / what problem it solves |
| `decision` | A choice made and the alternative rejected |
| `constraint` | A rule the code must keep obeying |
| `example` | A concrete usage or scenario worth remembering |
| `warning` | A trap, gotcha, or thing not to change |

```bash
thread-keep note add example.Run \
  --kind intent \
  --body "Runs the example workflow because callers need a single entrypoint (see PR #12)."
```

```
pending note 8904554f4d83ad3cd5fccada70a26e19 for example.Run
```

**Good notes carry evidence.** A reader six months from now should be able to trust
and trace the note. Compare:

- Good `decision`: "Switched Authorize to a token bucket after the fixed-window
  limiter caused thundering-herd retries in the 2026-05 incident (postmortem
  INC-431); ticket PLAT-882." — states the choice, the reason, and where to verify.
- Bad `decision`: "Changed the rate limiter, it's better now." — no reason, no
  evidence, no way to confirm.

- Good `constraint`: "Amount must stay in minor units (cents); the ledger and the
  Stripe adapter both assume integers — see money.go and adapter_test.go." — names
  the rule and the code that depends on it.
- Bad `constraint`: "Be careful with the amount field." — vague, unactionable.

### 4. Review the working state

```bash
thread-keep status
```

```
ref: refs/contexts/main
source: bd362ea576f509debf7f56480716150ebde3b25d
entities: 1
pending notes: 1
context commit:
coverage complete: true
coverage: go	indexed	builtin/go
```

### 5. See what will be committed

```bash
thread-keep diff
```

```
active	intent	example.Run	Runs the example workflow because callers need a single entrypoint (see PR #12).
```

`diff` shows all pending changes, including notes that need review or have gone
historical. `search` and `context get` show only active bindings.

### 6. Commit the context

Only `commit` creates an immutable context commit and advances your local context
ref. Nothing is committed automatically.

```bash
thread-keep commit -m "Document example workflow" --author you
```

```
context commit 003a35ba4b82df96d57311e1586f53cfff1ed95ebca9f5d24148b04a31458449
```

### 7. Search the evidence

```bash
thread-keep search Run
```

```
example.Run	example.go	fields:entity_key,name,note_body	terms:Run	notes:8904554f4d83ad3cd5fccada70a26e19	binding:active	fresh:true	Runs the example workflow because callers need a single entrypoint (see PR…
```

Results are lexical evidence, not opaque scores: you see the matched fields, matched
terms, active note IDs, binding state, freshness, and a snippet — so you can tell
*why* a result matched.

### 8. Read one entity's context

```bash
thread-keep context get example.Run
```

```
example.Run	function	example.go
[committed] intent	Runs the example workflow because callers need a single entrypoint (see PR #12).
```

`context related <entity-key>` is a bounded one-hop structural view: it reports only
the method's owner type and other entities in the same source file. It never claims
caller/callee, import, data-flow, or impact relationships.

### 9. Review history

```bash
thread-keep log
```

```
003a35ba4b82df96d57311e1586f53cfff1ed95ebca9f5d24148b04a31458449	Document example workflow
```

## Keeping context fresh

Thread Keep never silently treats stale context as current. When your source
changes and you re-run `thread-keep update`, each note's binding is reconciled:

- Unchanged entity → binding stays `active`.
- Renamed but structurally identical entity (one unambiguous match) → the binding
  moves to the new key and stays `active`.
- Changed structure, or ambiguous/unknown lineage → `needs_review`.
- No candidate entity → the binding becomes `historical`.

A `needs_review` (or `historical`) note is **hidden from `search` and
`context get`** until you confirm it — `diff` still shows it. For example, after
changing a function signature, `context get` returns only the entity header, and
`search` finds the entity but with no attached note.

Confirm that a note still applies to the current entity:

```bash
thread-keep note review <note-id> --entity <current-entity-key>
```

After review, the note is active again and reappears in search and context.

When the knowledge itself needs updating (not just re-binding), create a successor
revision. `note revise` never overwrites the prior body — the old revision stays in
the immutable history:

```bash
thread-keep note revise <note-id> --body "Updated contract: Run now takes a reason string for audit logging (PLAT-950)."
```

`note revise` operates on a committed note; use it after the original note has been
committed.

## Sharing with your team

A Thread Keep remote exchanges only content-addressed context objects and versioned
context refs. It never transfers your source files, your SQLite projection, pending
notes, or credentials.

A remote address is either an existing absolute filesystem path or an HTTP(S)
context-remote URL. Plain `http://` is accepted only for loopback hosts, so a token
never travels unencrypted.

```bash
# a shared filesystem path:
thread-keep remote add origin /absolute/path/to/context-remote
# or a hosted context remote:
thread-keep remote add origin https://context.example.com/v1/repositories/my-repo

thread-keep remote push origin
thread-keep remote fetch origin
thread-keep remote pull origin
```

- `push` publishes verified immutable objects, then advances the remote ref.
- `fetch` is safe even with pending local notes; it only advances a local tracking
  ref.
- `pull` refuses pending context changes first, then fast-forwards only when the
  remote tip contains your local tip and matches your current clean Git source SHA.
  Divergent or source-mismatched context returns a conflict rather than
  overwriting — resolve it explicitly, never by an automatic merge.

For HTTP(S) remotes, provide your GitHub token through the
`THREAD_KEEP_REMOTE_TOKEN` environment variable; it is sent as a bearer header and
never stored or logged. See [team-server.md](team-server.md) for running the hosted
context remote.

## Troubleshooting

Every failure exits with a stable code and a `code: message` line on stderr
(machine-readable with `--json`).

| Exit | Error code | Typical cause and fix |
| --- | --- | --- |
| 2 | `validation` | A bad or missing argument/flag (empty query, unknown note ID). Fix the command. |
| 3 | `repository_state` | Worktree is dirty or detached when indexing. Commit or stash your source changes first, then `update`. |
| 4 | `not_initialized` | You ran a context command before `init`. Run `thread-keep init`. |
| 5 | `stale_working_set` / `working_set_dirty` / `nothing_to_commit` / `coverage_incomplete` | Source moved under pending notes, nothing pending to commit, or a detected language lacks coverage. Re-run `update`; commit or discard context first if the working set is dirty; install the missing language pack for coverage errors. |
| 6 | `busy` / `concurrent_update` / `remote_conflict` | A lock is held, a concurrent change raced the commit, or the remote diverged. Retry; for `remote_conflict`, `fetch` and resolve explicitly. |
| 7 | `entity_not_found` | The entity key isn't indexed. Check the key with `search`, and run `update` if the source changed. |
| 8 | `auth` | GitHub rejected the token for an HTTP remote. Check `THREAD_KEEP_REMOTE_TOKEN` has pull (read) or push (write) permission on the mapped repo. |

## Next steps

- [claude-code.md](claude-code.md) — use Thread Keep from Claude Code over MCP.
- [codex.md](codex.md) — use Thread Keep from Codex.
- [team-server.md](team-server.md) — run and authenticate the hosted context remote.
