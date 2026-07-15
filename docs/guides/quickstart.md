# Thread Keep Quickstart

Thread Keep is a local-first code-context VCS. It preserves the "why" behind your
code as immutable, entity-bound context notes stored beside your repository — never
inside your source files. Git stays the source of truth for source code, commits,
and branches; Thread Keep versions the intent, decisions, constraints, examples, and
warnings that Git alone can't capture.

This guide gets you from zero to a committed, searchable context note in about
15 minutes. Use the published package path unless you are developing Thread Keep
itself or need an operational server binary.

## Install Thread Keep

Install the lightweight core package into a Python 3.9 or newer environment. It
contains the local CLI and MCP server:

```bash
python3 -m pip install thread-keep
thread-keep --help
```

Published wheels support Linux glibc 2.39 or newer on x64/arm64, macOS 15 or
newer on Apple Silicon, and Windows x64. Add only the language packs your
repository needs:

```bash
python3 -m pip install "thread-keep[typescript,python]"
# Or, after the core is installed:
thread-keep pack install typescript python
# Install every official pack only when needed:
python3 -m pip install "thread-keep[all]"
```

Each extra selects a separate native pack distribution at the exact core version.
The `pack install` command is a shallow wrapper around the current Python
environment's pip; it does not detect or install languages implicitly. Check the
result from inside the repository you want to index:

```bash
thread-keep indexers list
```

Go is built in. Other detected languages report `installed` or `missing` so you
know which optional pack is needed.

### Build from source

Thread Keep uses CGO-backed SQLite (FTS5), so a source build needs a Go toolchain
and a C compiler. From the Thread Keep repository root:

```bash
make build
```

This produces five runtime binaries in `bin/`:

- `thread-keep` — the CLI you use day to day.
- `thread-keep-mcp` — the local stdio MCP server used by coding agents (see
  [claude-code.md](claude-code.md) and [codex.md](codex.md)).
- `thread-keep-server` — the self-hosted context-remote server (see
  [team-server.md](team-server.md)).
- `thread-keep-coordinator` — the durable PR context planning process (see
  [pr-context-coordinator.md](pr-context-coordinator.md)).
- `thread-keep-runner` — the isolated one-job planning worker used by the
  coordinator.

Put `bin/thread-keep` on your `PATH`, or call it by path as `./bin/thread-keep`.

Go is indexed out of the box. A source build needs explicit TypeScript/JavaScript,
Python, Java, Kotlin, and Rust packs:

```bash
make build-pack
```

Then copy each required executable from `bin/` to the `thread-keep/packs/`
subdirectory of your operating system's user config directory. Run
`thread-keep indexers list` to verify the resolved paths. Without a detected
language's pack, Go stays searchable and that language reports `missing_pack`.

Upgrade, pin, or roll back the core and selected pack set through pip:

```bash
python3 -m pip install --upgrade "thread-keep[typescript,python]"
python3 -m pip install "thread-keep[typescript,python]==1.2.3"
```

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

`init` is required before stateful local context commands such as `update`,
`status`, and `note add`. Inspection/install commands such as `indexers list` and
`pack install` do not require initialization. Local context is stored under the
self-ignored `.thread-keep/` directory at the worktree root. Thread Keep source
annotations are not added and generated storage stays out of Git status.

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
  --topic workflow-entrypoint \
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

`--topic` is optional and repeatable. Use it for stable exact filters such as a
subsystem or invariant; do not turn topics into a second copy of the note body.

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

## Verify where the context lives

The commands above are the normal verification path:

- `status` distinguishes pending work from the current committed snapshot;
- `diff` shows what has not been committed;
- `context get` and `search` show active committed context; and
- `log` shows immutable context history.

After `thread-keep init`, the worktree root contains:

```text
.thread-keep/
├── .gitignore                      # ignores this generated directory
├── index.sqlite                    # rebuildable local projection and pending state
└── objects/
    └── <context-snapshot-id>.json  # immutable committed object
```

Each linked worktree has an independent local store. On first use after an
upgrade, a legacy `.git/thread-keep/` store is copied into `.thread-keep/` and
left untouched as a backup. Do not alternate old and new clients after this
migration. Do not edit SQLite directly. If the local projection is lost while
committed objects remain, recover it with an explicit ID from `thread-keep log`:

```bash
thread-keep rebuild <context-snapshot-id>
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

### Resolve same-source divergence

After `remote fetch` reports a competing snapshot for the same source revision,
use the local and fetched snapshot IDs explicitly:

```bash
thread-keep context merge start <local-snapshot-id> <remote-snapshot-id> \
  -m "Merge review context"
thread-keep context merge show <session-id>
thread-keep context merge resolve <session-id> <conflict-id> --use local
# repeat with --use remote or an authored resolution when appropriate
thread-keep context merge commit <session-id>
thread-keep remote push origin
```

The merge advances only the local context ref. It never edits source files or
publishes automatically; inspect the result before the explicit push.

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
| 5 | `stale_working_set` / `working_set_dirty` / `nothing_to_commit` / `coverage_incomplete` | Source moved under pending notes, nothing is pending, or a detected language lacks coverage. Return the source to the indexed commit or commit an acceptable complete pending set before changing source; install the missing pack for coverage errors. Selective pending-draft discard is not currently available. |
| 6 | `busy` / `concurrent_update` / `remote_conflict` | A lock is held, a concurrent change raced the commit, or the remote diverged. Retry; for `remote_conflict`, `fetch` and resolve explicitly. |
| 7 | `entity_not_found` | The entity key isn't indexed. Check the key with `search`, and run `update` if the source changed. |
| 8 | `auth` | GitHub rejected the token for an HTTP remote. Check `THREAD_KEEP_REMOTE_TOKEN` has pull (read) or push (write) permission on the mapped repo. |

## Next steps

- [claude-code.md](claude-code.md) — use Thread Keep from Claude Code over MCP.
- [codex.md](codex.md) — use Thread Keep from Codex.
- [team-server.md](team-server.md) — run and authenticate the hosted context remote.
- [pr-context-coordinator.md](pr-context-coordinator.md) — plan and land context
  for GitHub pull requests.
- [../architecture.md](../architecture.md) — read the current implementation
  contract and explicit product limits.
