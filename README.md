# Thread Keep

Thread Keep is a local-first code-context VCS. It indexes Go functions, methods and types in the core, and can index TypeScript/TSX, JavaScript/JSX, Python, Java, Kotlin, and Rust through explicitly installed Thread Keep language packs. It stores context in the Git common directory and searches entities and ContextNotes with SQLite FTS5.

## Guides

- [Quickstart](docs/guides/quickstart.md) — build, first notes, search, sharing, troubleshooting.
- [Claude Code setup](docs/guides/claude-code.md) — MCP registration, agent instructions, draft skill, hooks.
- [Codex setup](docs/guides/codex.md) — MCP configuration and AGENTS.md instructions.
- [Team server operations](docs/guides/team-server.md) — hosted context remote, GitHub authorization, clustering, maintenance, onboarding.
- [PR context planning](docs/guides/pr-context-coordinator.md) — Server-owned durable webhook intake, dedicated coordinator and runner processes, Checks, landing, and recovery.
- [Release operations](docs/guides/releases.md) — GitHub Actions, GoReleaser, signed pack assets, and npm binary publishing.

## Install from npm

Published releases can be installed without a Go toolchain. npm selects the matching native package for Linux glibc x64/arm64, macOS x64/arm64, or Windows x64:

```bash
npm install --global thread-keep
thread-keep --help
```

The same package also exposes `thread-keep-mcp`. It contains both platform binaries in npm itself and does not run a postinstall downloader. Operational `thread-keep-server`, `thread-keep-coordinator`, and `thread-keep-runner` binaries remain GitHub Release and container artifacts rather than inflating every npm CLI install. Official language packs remain on the signed `thread-keep indexers install --detected` path described below.

## Build

This release intentionally uses CGO-backed SQLite to keep its dependency graph small. Build and test with FTS5 enabled:

```bash
make test
make vet
make build
```

Cross-platform releases need a native runner or a configured C cross-toolchain because SQLite uses CGO.

## Deployment images

Production images use one Dockerfile per deployable process boundary. Build all three local images with:

```bash
make docker-build
```

| Dockerfile | Local image | Runtime | Contents |
| --- | --- | --- | --- |
| `Dockerfile.server` | `thread-keep-server:local` | Distroless Debian 12 | `thread-keep-server` and its CGO runtime only |
| `Dockerfile.coordinator` | `thread-keep-coordinator:local` | Debian 12 slim | `thread-keep-coordinator`, the process-backend runner, `git`, CA certificates, and all official indexer packs |
| `Dockerfile.runner` | `thread-keep-runner:local` | Debian 12 slim | The isolated runner, `git`, CA certificates, and all official indexer packs |

All images run as UID/GID `65532:65532`. The server image has no shell or package manager. Mount configuration and secrets at runtime; do not bake them into an image. Bind-mounted files and storage directories must be readable or writable by that identity as appropriate.

For example, start a single-node server with an external config file and a persistent named volume:

```bash
docker run --rm --publish 8320:8320 \
  --mount type=volume,src=thread-keep-data,dst=/var/lib/thread-keep \
  --mount type=bind,src=/absolute/path/server.json,dst=/etc/thread-keep/server.json,readonly \
  thread-keep-server:local \
  --listen 0.0.0.0:8320 \
  --storage /var/lib/thread-keep \
  --config /etc/thread-keep/server.json
```

See [Team server operations](docs/guides/team-server.md) for storage, database, TLS, clustering, and secret requirements. See [PR context planning](docs/guides/pr-context-coordinator.md) for coordinator and remote-runner deployment contracts.

## Docker E2E

Run the complete local-MVP black-box scenario suite in a disposable Linux container:

```bash
make e2e
```

The container builds the actual CGO binary and exercises real temporary Git repositories. It covers initialization, indexing, notes, search, context, diff, commit/log, JSON errors, dirty/stale working sets and linked-worktree isolation. The scenario runner is [`e2e/run.sh`](e2e/run.sh).

## Local workflow

```bash
thread-keep init
# commit source changes first; update rejects a dirty worktree
thread-keep update
thread-keep note add example.Run --kind intent --body "Runs the example workflow"
thread-keep status
thread-keep search Run
thread-keep diff
thread-keep commit -m "Document example workflow"
thread-keep log
```

`note add` creates a pending logical ContextNote with its first immutable revision. After that note is committed, `note revise <note-id> --body "Updated workflow contract"` creates a successor revision without modifying the prior body. When a source update changes a bound entity, Thread Keep records a pending `needs_review` or `historical` binding rather than silently treating the old context as current. Confirm a reviewed binding explicitly:

```bash
thread-keep note review <note-id> --entity <current-entity-key>
```

`context get` and search show only active bindings; `diff` exposes all pending changes, including review-required or historical bindings. Only `commit` creates an immutable ContextCommit and advances the local context ref. New commits write schema-v3 Context Snapshot manifests: the same content-addressed object records the source Git SHA, complete indexer/pack provenance, and exact entity-to-note-revision mappings. Snapshot IDs remain ordinary Thread Keep object IDs, never Git blobs or native Git refs. The current MVP does not mutate source files, call AI automatically, or create context commits automatically.

`init` is required before using local context commands. `update` accepts only a clean, committed Git worktree so the indexed entities and recorded source SHA always describe the same source revision.

## Evidence search and structural context

`search <query>` returns only fresh indexed entities and includes the matched fields, terms, active note IDs, binding state, and a snippet in JSON output. Human output labels the same evidence fields, so a result is not just an opaque score.

```bash
thread-keep search "Authorize"
thread-keep context related payment.Authorize --limit 20
```

`context related` is a bounded one-hop structural view, not a call graph: it reports only a method’s owner type and entities in the same current source file. It never claims caller, callee, import, data-flow, or impact relationships.

## Context assembly and retrieval

Context assembly answers a bounded question with selected notes, resolved
entities, selection reasons, source/context IDs, and explicit completeness
diagnostics. It does not discover or expand code relationships:

```bash
# Constraints and warnings attached directly to changed entities
thread-keep context for-change --kind constraint --kind warning

# Past and current design decisions relevant to a diff
thread-keep context for-change --kind decision --history all

# Warning notes tagged for cache invalidation
thread-keep context query "cache invalidation" --kind warning --topic cache-invalidation

# Context recorded directly on a known interface
thread-keep context for-entity example.Runner --kind constraint
```

Use `note add` or `note revise` with `--topic <topic>` to add exact retrieval
topics. Notes remain bound to their explicit entity; Context Assembly does not
propagate owner, same-file, interface, implementation, call, import, or impact
relationships. Agents may discover a relevant entity with their existing code
tools and query that entity directly.

`history=current` is the default. `history=all` traverses verified immutable
Context Snapshot ancestry and labels superseded observations as historical.
`complete=false` means callers must inspect `diagnostics`: common reasons are
incomplete language coverage, legacy base provenance, or result truncation.

## Remote sync

An explicitly configured remote exchanges only content-addressed context objects and versioned context refs. It never transfers source files, SQLite projections, pending notes, or credentials. A remote address is either an existing absolute filesystem path or an HTTP(S) context-remote URL; plain `http://` is accepted only for loopback hosts so a token never travels unencrypted.

```bash
thread-keep remote add origin /absolute/path/to/context-remote
# or point at a hosted context remote:
thread-keep remote add origin https://context.example.com/v1/repositories/my-repo
thread-keep remote push origin
thread-keep remote fetch origin
thread-keep remote pull origin
```

`push` publishes verified immutable objects before it CAS-advances the remote ref; a CAS race can leave unreachable remote objects but never changes the local ref. `fetch` is safe with pending local notes and advances only a local tracking ref. `pull` refuses pending context changes before accessing the remote, then fast-forwards only when the remote context tip contains the local tip and matches the current clean Git source SHA. Remote-ahead, divergent, or source-mismatched context returns `remote_conflict` or `stale_working_set` without overwriting local context. Divergence never triggers an automatic merge.

## Hosted context remote with GitHub authentication

`thread-keep-server` is the self-hosted context-remote server. It stores immutable context objects as verbatim content-addressed files on its own disk — never in Git blobs or refs — and keeps versioned context refs in a database with transactional compare-and-swap. The ref database defaults to an embedded file under the storage root; pass `--db-dsn postgres://...` to use PostgreSQL instead. Authorization is delegated to GitHub: each configured `repository_id` maps to a GitHub `owner/repo`, reads require the caller's token to have pull permission and writes require push permission.

```bash
thread-keep-server --listen 127.0.0.1:8320 --storage /var/lib/thread-keep-server \
  --config /etc/thread-keep-server/config.json
# optional external ref database:
#   --db-dsn postgres://user@host:5432/threadkeep
```

```json
{
  "github_api_base_url": "https://api.github.com",
  "repositories": {
    "my-repo": { "github_owner": "acme", "github_repo": "thread-keep" }
  }
}
```

The CLI reads the GitHub token from the `THREAD_KEEP_REMOTE_TOKEN` environment variable and sends it as a bearer header. The token is never stored in configuration or SQLite and never appears in output or error messages. Authorization failures exit with the stable error code `auth` (exit code 8); a GitHub outage is reported as a distinct storage error, not a denial. Run the server behind TLS termination in production; OAuth device flow, GitHub Apps, and other providers are not implemented.

## Multi-node cluster

Multiple `thread-keep-server` nodes can serve the same repositories behind a load balancer. Refs are coordinated by the shared ref database (use PostgreSQL via `--db-dsn` for multi-node deployments), and immutable objects gain availability through three consensus-free mechanisms: write-through replication to live peers with a copy quorum, peer fetch-on-miss with content-hash verification, and periodic anti-entropy repair. Peer membership uses a database lease registry: each node heartbeats into the shared database and stale nodes drop out of the view by TTL — no active eviction, no Raft.

Add a `cluster` section to the configuration and provide the shared peer secret through the `THREAD_KEEP_CLUSTER_SECRET` environment variable (required in cluster mode, never logged):

```json
{
  "github_api_base_url": "https://api.github.com",
  "repositories": { "my-repo": { "github_owner": "acme", "github_repo": "thread-keep" } },
  "cluster": {
    "node_id": "node-a",
    "advertise_url": "https://node-a.internal:8320",
    "replication_factor": 2,
    "heartbeat_seconds": 10,
    "ttl_seconds": 30,
    "anti_entropy_seconds": 300
  }
}
```

Peer-to-peer requests authenticate with the cluster secret and bypass GitHub verification; they read and write only the local node, so replication never fans out recursively. A write is acknowledged once `min(replication_factor, live nodes)` copies exist; a node that was down catches up through fetch-on-miss and anti-entropy.

Membership has two modes selected by `cluster.membership`. The default `db` mode is the lease registry above and assumes all nodes share one ref database. The `swim` mode uses gossip (hashicorp/memberlist) instead: nodes exchange liveness directly, the gossip transport is encrypted with a key derived from the cluster secret, and a new node joins through one or more seed addresses.

```json
"cluster": {
  "node_id": "node-a",
  "advertise_url": "https://node-a.internal:8320",
  "membership": "swim",
  "swim": { "bind_addr": "0.0.0.0:7946", "seeds": ["node-b.internal:7946"] }
}
```

The server shuts down gracefully on SIGINT/SIGTERM: it stops accepting connections, finishes in-flight requests (up to 10 seconds), actively leaves the membership view, and exits 0.

Storage maintenance follows the git model: automatic by default, conservative about deletion, and packing old objects. A maintenance pass walks every ref tip through its parent links, deletes only unreachable objects older than the grace window (default two weeks, matching git's prune expiry), and repacks aged reachable loose objects into a compressed pack file (`packs/pack-*.pack` with a JSON index). Entries are zstd-compressed verbatim object bytes; the largest object in a pack serves as a raw compression dictionary so near-identical snapshots share their common content, without full delta chains. Content addressing and hash verification are unchanged. A repository whose local DAG is incomplete is skipped entirely rather than partially collected.

Maintenance triggers like `git gc --auto`: after an externally published object, when the loose object count exceeds a threshold (default 512), one background pass runs. Configure or disable it:

```json
"gc": {
  "auto": true,
  "auto_threshold": 512,
  "grace_seconds": 1209600,
  "interval_seconds": 3600
}
```

`auto` defaults to true even without a `gc` section; `auto: false` disables all automatic maintenance. `interval_seconds` adds an optional fixed schedule. A manual offline pass stays available and reports JSON on stdout:

```bash
thread-keep-server --gc --gc-grace 336h --storage /var/lib/thread-keep-server \
  --config /etc/thread-keep-server/config.json
```

## Semantic snapshot merge

After fetching a same-source v3 snapshot, resolve divergence explicitly with the selected local and remote snapshot IDs:

```bash
thread-keep context merge start <local-snapshot-id> <remote-snapshot-id> -m "Merge review context" --author reviewer
thread-keep context merge show <session-id>
thread-keep context merge resolve <session-id> <conflict-id> --use local
# or: --use remote
# or: --use authored --entity example.Run --kind decision --body "Resolved contract" --author reviewer
thread-keep context merge commit <session-id>
```

`merge start` requires a clean worktree, no pending notes, the current local ref to equal the chosen local snapshot, and v3 inputs with matching repository, context ref, source SHA, and provenance. `merge commit` additionally requires the selected snapshots to have the same entity set. Non-overlapping note records compose automatically; competing revisions, bindings, or mappings remain in a local conflict session until explicitly resolved. `merge commit` writes one content-addressed snapshot with ordered parents `[local, remote]` and CAS-advances only the local ref. It does not edit source files, write text conflict markers, overwrite a remote ref, or publish automatically; run `remote push` explicitly afterwards when appropriate.

## Provider-neutral candidates

Import a versioned local JSON candidate envelope without configuring a token or contacting a provider:

```bash
thread-keep candidate import /absolute/path/candidate.json
thread-keep candidate list
thread-keep candidate show github:owner/repository#42
thread-keep candidate promote github:owner/repository#42
```

Imported candidate notes are draft-only and never appear in normal search, context, diff, commit history, or remote synchronization. Promotion is explicit: only a merged candidate whose `merge_sha` equals the current clean Git source can create normal pending notes. Exact entity/hash evidence becomes active; changed evidence requires review; a missing entity remains historical candidate evidence. This slice has no GitHub/GitLab/Gitea API client, webhook, token storage, network call, automatic promotion, source mutation, or automatic context commit.

## Projection recovery

SQLite is a derived local projection. If `index.sqlite` is deleted while immutable objects remain, rebuild an empty projection from an explicitly selected context commit and the current clean Git source:

```bash
thread-keep rebuild <context-commit-id>
```

Use a commit ID previously returned by `thread-keep commit` or `thread-keep log`; its matching object is stored under the Git common directory at `thread-keep/objects/<context-commit-id>.json`. Rebuild validates the complete selected ancestry, repository and context ref, reindexes the current Git HEAD, and restores history, committed notes and search atomically. For a schema-v3 Context Snapshot, the current clean Git SHA and complete installed-indexer provenance must match the manifest before SQLite is created; legacy v1/v2 objects retain their existing rebuild behavior. It refuses to overwrite any non-empty projection and never guesses a ref from object timestamps or graph tips because an object may be unreachable after a failed ref transaction.

## TypeScript, JavaScript, Python, Java, Kotlin, and Rust packs

The core automatically detects `.ts`, `.tsx`, `.mts`, `.cts`, `.js`, `.jsx`, `.mjs`, `.cjs`, `.py`, `.pyi`, `.pyw`, `.java`, `.kt`, `.kts`, and `.rs` files. It looks only in Go's user configuration directory (for example `$XDG_CONFIG_HOME/thread-keep/packs/thread-keep-index-typescript`, `$XDG_CONFIG_HOME/thread-keep/packs/thread-keep-index-javascript`, `$XDG_CONFIG_HOME/thread-keep/packs/thread-keep-index-python`, `$XDG_CONFIG_HOME/thread-keep/packs/thread-keep-index-java`, `$XDG_CONFIG_HOME/thread-keep/packs/thread-keep-index-kotlin`, and `$XDG_CONFIG_HOME/thread-keep/packs/thread-keep-index-rust` on Linux); `update` never downloads or installs a pack. Build all bundled packs explicitly with `make build-pack`, then place each executable at its fixed path.

Without a detected language's pack, Go remains searchable and `update` reports that language as `missing_pack`. Such a partial projection cannot become a context commit. Use `thread-keep update --require-complete` when automation must fail immediately on incomplete coverage.

Inspect the known built-in and official pack locations without initializing context storage, launching a pack, downloading anything, or changing files:

```bash
thread-keep indexers list
```

The command reports whether each known indexer is built in, installed as an executable regular file at the fixed user-config path, or missing. It also marks the languages detected in the current repository.

## Official pack installation and releases

Only a release binary built with the official manifest verification key can install packs. The command never runs from `init` or `update`; invoke it explicitly for detected missing languages:

```bash
thread-keep indexers install --detected
```

The installer downloads a signed manifest from the official GitHub Release origin, verifies its Ed25519 signature, selects the current GOOS/GOARCH asset, verifies its exact byte size and SHA-256, then publishes the executable atomically at the fixed user-config pack path. A development build without a release public key rejects installation before contacting the network or creating pack storage.

Release automation embeds a base64 Ed25519 public key only:

```bash
make release-build THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64=<base64-public-key>
```

Create the signed envelope from a raw manifest payload with a securely provisioned base64 Ed25519 private-key file. Do not place that key in this repository or command history:

```bash
go run ./cmd/thread-keep-sign-manifest -- \
  --payload thread-keep-indexers-manifest-v1.json \
  --private-key "$THREAD_KEEP_PRIVATE_KEY_FILE" \
  > thread-keep-indexers-manifest-v1.signed.json
```

Stable tags also publish GoReleaser-built server, coordinator, and runner images for Linux amd64/arm64:

```bash
docker pull ghcr.io/tae2089/thread-keep-server:1.2.3
docker pull ghcr.io/tae2089/thread-keep-coordinator:1.2.3
docker pull ghcr.io/tae2089/thread-keep-runner:1.2.3
```

The images reuse GoReleaser's prebuilt CGO binaries rather than compiling the project again during the Docker build.

## Agent integration over MCP

`thread-keep-mcp` exposes the local context to coding agents through the Model Context Protocol (stdio):

```bash
claude mcp add thread-keep -- thread-keep-mcp --repo /path/to/repo
```

The server uses the official `modelcontextprotocol/go-sdk` for MCP lifecycle,
protocol negotiation, request dispatch, cancellation, and stdio transport.
Thread Keep owns only the tool contracts and their application-service adapter.

Read tools (`search`, `context_get`, `context_for_change`, `context_for_entity`, `context_query`, `related_context`, `status`, `diff`) return the same JSON structures as the CLI. Context query tools expose direct evidence reasons, history, and completeness diagnostics without relation expansion. The only write tools are `note_add` and `note_revise`: they create pending drafts whose origin is always recorded as `agent`, so an agent can capture intent, decisions, constraints, examples, and warnings at the moment they are made. Nothing an agent does through MCP can commit context, touch source, or reach a remote — promotion stays an explicit human `thread-keep commit`.

## AI drafts and hooks

The project-local [context draft skill](.agents/skills/thread-keep-context-draft/SKILL.md) and [prompt](prompts/context-draft.md) guide an agent to add evidence-backed pending notes only. A Git hook or coding-agent completion hook may invoke `thread-keep update` and start a draft job, but it must not block Git commits, call a model synchronously in `pre-commit`/`pre-push`, commit context automatically or mutate source code.

Automation should use `--json`; successful output is versioned and errors are written to stderr with a stable error code.

## License

Thread Keep is available under the [MIT License](LICENSE).
