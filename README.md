# Thread Keep

Thread Keep is a local-first version control system for the knowledge behind your
code. It attaches durable notes such as intent, decisions, constraints, examples,
and warnings to functions, methods, and types—without adding annotations to source
files.

Git continues to version the code. Thread Keep versions the context that explains
why the code is shaped that way.

## Is Thread Keep for you?

Use Thread Keep when you want developers and coding agents to:

- find the reason behind an implementation without searching old chats;
- keep decisions and warnings attached to the code entity they describe;
- notice when a code change may have made saved context stale;
- review AI-drafted context before it becomes part of shared history; or
- share code context without transferring source files.

Thread Keep is not a call graph, a general code-search platform, or an autonomous
agent. It does not edit source files, call an AI model, or commit context
automatically.

## Install

Published releases support Python 3.9 or newer on Linux glibc 2.39 or newer
x64/arm64, macOS 15 or newer on Apple Silicon, and Windows x64:

```bash
python3 -m pip install thread-keep
thread-keep --help
```

Go is indexed by the core package. Install only the language packs your repository
needs:

```bash
python3 -m pip install "thread-keep[typescript,python]"
# or install all official packs
python3 -m pip install "thread-keep[all]"
```

The supported optional packs are TypeScript/TSX, JavaScript/JSX, Python, Java,
Kotlin, and Rust. You can see what the current repository needs with:

```bash
thread-keep indexers list
```

macOS Intel, Windows arm64, and Linux musl are not release targets. See the
[Quickstart](docs/guides/quickstart.md#install-thread-keep) for source builds and
manual pack setup.

## Save your first context note

Run these commands inside a Git repository. `update` requires a clean, committed
worktree so the index always describes an exact Git revision.

```bash
# 1. Create local Thread Keep storage and index the current commit.
thread-keep init
thread-keep update

# 2. Find the exact key of the function, method, or type you want to document.
thread-keep search "Run"

# 3. Add a pending note using a key returned by search.
thread-keep note add example.Run \
  --kind intent \
  --body "Runs the example workflow through one audited entry point (see PR #12)."

# 4. Review the pending change, then commit it explicitly.
thread-keep status
thread-keep diff
thread-keep commit -m "Document the example workflow"

# 5. Confirm that the note is searchable and present in history.
thread-keep context get example.Run
thread-keep log
```

Nothing is promoted automatically. `note add` and agent write tools create pending
drafts; only `thread-keep commit` creates an immutable context snapshot. The current
CLI commits the complete pending set and does not yet provide selective draft edit
or discard.

For expected output, note-writing examples, stale-context review, and common errors,
follow the [15-minute Quickstart](docs/guides/quickstart.md).

## Verify what was saved

Use the CLI first:

- `thread-keep status` shows the indexed source revision, coverage, current context
  snapshot, and pending-note count.
- `thread-keep diff` shows every pending note or binding change that has not been
  committed.
- `thread-keep search <query>` and `thread-keep context get <entity-key>` show active
  committed context.
- `thread-keep log` shows immutable context history.

Thread Keep stores local data in a generated, self-ignored directory at the
worktree root:

```text
.thread-keep/
├── .gitignore                         # ignores this generated directory
├── index.sqlite                       # rebuildable local projection and working state
└── objects/
    └── <context-snapshot-id>.json     # immutable committed context object
```

On first use after upgrading from the legacy layout, Thread Keep copies
`.git/thread-keep/` into `.thread-keep/` through a validated SQLite backup and
leaves the legacy directory untouched. Do not alternate between an older client
that writes the legacy directory and a newer client that writes `.thread-keep/`.

Do not edit `index.sqlite` directly. If the projection is lost but the immutable
objects remain, use `thread-keep rebuild <context-snapshot-id>` with an ID from
`thread-keep log`.

## What is implemented?

| Area | Current capability | Read more |
| --- | --- | --- |
| Local context | Initialize, index, add/revise/review notes, inspect pending changes, commit, search, read, and rebuild | [Quickstart](docs/guides/quickstart.md) |
| Languages | Built-in Go plus six optional Tree-sitter-based language packs | [Multi-language indexing](docs/multilanguage-indexing.md) |
| Retrieval | Lexical evidence, entity context, bounded same-file/owner relationships, and filtered context assembly | [Architecture](docs/architecture.md) |
| Coding agents | Local stdio MCP with eight read tools and two pending-draft tools; no commit or remote-write tool | [Codex](docs/guides/codex.md), [Claude Code](docs/guides/claude-code.md) |
| Sharing | Explicit filesystem or HTTP(S) remotes, fast-forward pull, and explicit semantic merge | [Quickstart: sharing](docs/guides/quickstart.md#sharing-with-your-team) |
| Hosted remote | GitHub-authorized object/ref server, PostgreSQL option, clustering, and storage maintenance | [Team server](docs/guides/team-server.md) |
| PR context planning | Opt-in durable GitHub webhook intake, isolated planning runners, Checks, automatic landing, and manual recovery | [PR context planning](docs/guides/pr-context-coordinator.md) |
| Distribution | Platform wheels, raw release binaries, checksums, and three production container images | [Release operations](docs/guides/releases.md) |

Important limits are explicit: structural context is not a call graph; local MCP is
the only MCP endpoint; coordinator mode is durable single-coordinator rather than
HA; and live Kubernetes runner validation remains opt-in for the target cluster.

## Connect a coding agent

The core wheel includes `thread-keep-mcp`. Initialize and update the repository
before registering it with your agent.

- [OpenAI Codex setup](docs/guides/codex.md)
- [Claude Code setup](docs/guides/claude-code.md)

Both integrations enforce the same boundary: agents may read committed context and
draft pending notes, while a human reviews `thread-keep diff` and runs
`thread-keep commit`.

## Share context with a team

A Thread Keep remote transfers immutable context objects and versioned context
references. It never transfers source files, the local SQLite projection, pending
notes, or credentials.

```bash
thread-keep remote add origin /absolute/path/to/context-remote
# or use a hosted context remote
thread-keep remote add origin https://context.example.com/v1/repositories/my-repo

thread-keep remote push origin
thread-keep remote fetch origin
thread-keep remote pull origin
```

`pull` is fast-forward only. Divergence is reported instead of being overwritten;
resolve same-source snapshot divergence explicitly with `thread-keep context merge`.
For hosted setup, authentication, backups, clustering, and maintenance, use the
[Team server guide](docs/guides/team-server.md).

## Build from source

The SQLite search projection uses CGO and FTS5, so a source build needs Go and a C
compiler:

```bash
make test
make vet
make build
```

`make build` creates five runtime binaries in `bin/`: `thread-keep`,
`thread-keep-mcp`, `thread-keep-server`, `thread-keep-coordinator`, and
`thread-keep-runner`. Build the six optional language-pack binaries separately:

```bash
make build-pack
```

Run the complete local black-box workflow in a disposable Linux container with
`make e2e`. Build the three production process-boundary images with
`make docker-build`.

## Documentation

| Start here | Purpose |
| --- | --- |
| [Quickstart](docs/guides/quickstart.md) | Installation, first note, verification, sharing, and troubleshooting |
| [Codex setup](docs/guides/codex.md) | MCP registration and project instructions |
| [Claude Code setup](docs/guides/claude-code.md) | MCP registration, draft skill, and optional hooks |
| [Team server](docs/guides/team-server.md) | Self-hosted remote operations and team onboarding |
| [PR context planning](docs/guides/pr-context-coordinator.md) | GitHub App, coordinator, runner, landing, and recovery |
| [Release operations](docs/guides/releases.md) | Supported targets and release verification |
| [Architecture](docs/architecture.md) | Current implementation contract, module boundaries, and deferred work |
| [Evaluation plan](docs/evaluation.md) | Planned agent-effectiveness measurements; not current product telemetry |

## License

MIT
