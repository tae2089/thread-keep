# Thread Keep Architecture

## Product boundary

`thread-keep` is a Go-based, local-first code-context VCS. It preserves context
for code entities without source annotations; Git remains the source of truth for
source files, commits, branches, and pull-request merge state.

The product has two layers. The local layer is sufficient for one developer or
coding agent and has no server dependency:

```text
init -> update -> note add/revise/review -> status/diff/search/context get -> commit -> log
     -> rebuild <explicit-context-commit-id> after projection loss
     -> remote add/list/push/fetch/pull for explicit filesystem or HTTP context remotes
     -> context merge start/show/resolve/commit for explicit local semantic merges
     -> candidate import/list/show/promote for explicit local candidate envelopes
```

The optional hosted layer adds an HTTP context remote, GitHub authorization,
multi-node object replication, durable GitHub webhook intake, PR context planning,
isolated runners, informational Checks, opt-in automatic landing, and explicit
manual landing recovery. It is split across `thread-keep-server`,
`thread-keep-coordinator`, and `thread-keep-runner`; see
[PR context planning and landing](guides/pr-context-coordinator.md) for the exact
production contract.

GitHub remains authoritative for repository identity and source merges. Thread
Keep stores context objects and refs, not source files or native Git blobs/refs.
Graph enrichment, source mutation, automatic model calls, and automatic local
context commits are not implemented. Local semantic merge composes only
non-overlapping note records automatically; competing revisions or bindings need
an explicit structured resolution.

## Current implementation contract

- The CLI uses Cobra. `cmd/thread-keep` only composes dependencies and registers commands; `internal/cli` owns command lifecycle, rendering and exit-code policy.
- The core detects Go, TypeScript-family, JavaScript-family, Python-family, Java, Kotlin, and Rust files. Its Go indexer discovers functions, methods and types using `go/ast` and `go/parser`; separately installed TypeScript, JavaScript, Python, Java, Kotlin, and Rust packs use Tree-sitter. Go keys include the repository-relative directory and package; external-pack keys include language, path, kind and qualified name.
- Context is stored outside source files in the Git common directory under `thread-keep/`. SQLite/FTS5 is the local current-state projection; content-addressed files store immutable committed context objects.
- `update` accepts only a clean, committed, non-detached Git worktree. The indexed entity set records the matching Git HEAD SHA.
- Each detected language has coverage (`indexed`, `missing_pack` or `failed`). Search and `context get` include only fresh indexed coverage, and `commit` rejects incomplete coverage.
- `search <query>` returns lexical evidence (`matched_fields`, terms, active note IDs, binding state, freshness, and snippet) with deterministic precedence for exact key, exact name, active note, signature/path, then FTS rank. `context related <entity-key> --limit` is a bounded one-hop structural view containing only method-owner and same-file edges; it does not claim call, import, data-flow, or impact edges.
- `context for-entity`, `context for-change`, and `context query` assemble bounded evidence bundles filtered by kind, state, language, path, entity kind, exact topic, history mode, and limit. They never expand call/import/impact relationships. Each bundle carries source/context IDs, selection reasons, `complete`, and explicit diagnostics such as truncation or incomplete language coverage.
- Pack resolution prefers exact-version `thread-keep-pack-<language>` distributions discovered and validated by the Python launcher, then a legacy fixed local executable for source/manual use. The core wheel stays lightweight; extras such as `thread-keep[typescript,python]` select only required packs, while `thread-keep pack install` shallowly invokes the current Python environment's pip. `init` and `update` never install, replace, or download packs. GitHub Releases expose unsigned raw binaries and checksums for manual and operational use, not a second package-management channel.
- A ContextNote has a stable logical ID, an immutable revision body, and a current entity binding. `note add` creates the first pending active revision; after that note is committed, `note revise` creates a pending successor without overwriting prior history; `note review` is the explicit local transition from `needs_review` to `active`. Supported kinds are `intent`, `decision`, `constraint`, `example` and `warning`. `commit` promotes the complete pending set; selective pending-draft edit/discard is not implemented.
- `update` reconciles active bindings after indexing: unchanged exact entities remain active, one unique same-language/same-kind structural match moves the binding, changed or ambiguous lineage becomes `needs_review`, and no candidate becomes `historical`. Search and `context get` expose only active effective bindings; `diff` exposes pending state transitions.
- The working set is scoped to the Git worktree, branch-derived context ref and source SHA. Linked worktrees do not share pending notes.
- `commit -m <message>` writes an immutable object, then atomically advances the local context ref through SQLite metadata. Ordinary history starts with Context Snapshot schema v3; schema v4 adds landing receipts, and descendants of v4 history remain v4 without repeating the receipt. Provenance is complete/sorted indexed language evidence, every canonical note revision has a checked entity mapping, and the object ID remains content-addressed. A normal commit has one ordered `parent_ids` entry; the first v3 commit after legacy history uses `legacy_parent_id` to preserve object ancestry without inventing legacy provenance. The commit stops if the source SHA, parent ref or snapshot pending-note IDs changed, preserving pending notes on conflict.
- `context merge start/show/resolve/commit` performs an explicit local semantic three-way merge of selected v3 snapshots. Start requires the current source, matching repository/ref/provenance, an empty pending set, and a local input equal to the current local ref; commit additionally requires matching entity sets. A deterministic merge base selects automatic records or structured conflicts. A session persists local/remote/base IDs and explicit resolutions; the final deterministic object preserves ordered `[local, remote]` parents, then one SQLite transaction CAS-advances only the local ref and marks the session committed. Authored resolutions always create a source-bound immutable successor revision; no source mutation, text conflict marker, automatic remote write, or implicit retry occurs.
- `rebuild <context-commit-id>` validates one explicitly selected immutable object DAG and atomically restores an empty SQLite projection. It never infers a canonical ref from object files. A v3 snapshot additionally requires an identical current clean Git source SHA and complete matching indexer provenance before SQLite mutation; v1/v2 keep legacy rebuild behavior.
- `remote add/list/push/fetch/pull` uses a named absolute filesystem path or an HTTP(S) context-remote URL, never a Git remote alias. Plain `http://` is accepted only for loopback hosts so a bearer token never travels unencrypted. Both transports implement the same four-operation contract (`ReadObject`, `PublishObject`, `ReadRef`, `CompareAndSwapRef`) and transfer only hash-verified immutable object DAGs and versioned context refs. Push publishes objects before remote ref CAS; fetch writes only local immutable objects and a tracking ref; pull has a no-pending precondition and applies only a same-source fast-forward. Divergence, remote-ahead state and CAS loss return `remote_conflict`; callers choose `context merge` explicitly when a fetched snapshot needs semantic resolution.
- The HTTP client sends the GitHub token from `THREAD_KEEP_REMOTE_TOKEN` as a bearer header and never persists it in SQLite, configuration, output, or errors. `thread-keep-server` maps each `repository_id` URL segment to a configured GitHub `owner/repo`, verifies the presented token's pull permission for reads and push permission for writes against the configured GitHub API base URL, and distinguishes provider outage (502) from authorization denial (401/403, exit code 8 via error code `auth`). A genuinely absent object returns 404 with error code `object_missing`, which the HTTP client preserves for programmatic handling; pack or index corruption remains a distinct validation/storage failure and never triggers fetch-on-miss. Object uploads are re-verified against their content ID on the server before atomic publication.
- Server storage splits by data shape: immutable context objects are verbatim content-addressed files (never re-serialized or decomposed into tables, so byte-level hashes, unknown future schema fields, and DAG integrity survive), while versioned context refs live in a GORM-backed `context_refs` table with transactional optimistic compare-and-swap (embedded database file by default, PostgreSQL via `--db-dsn`). The ref database is the cluster coordination point; objects need no ordering consensus because identical IDs imply identical bytes.
- Cluster mode runs N nodes behind a load balancer over one shared ref database. Membership sits behind a `Membership` interface with two modes: `db` (default — heartbeat UPSERTs stamped with the database clock, a TTL window filter at query time, active row deletion only on graceful leave) and `swim` (hashicorp/memberlist gossip with static seeds and a transport key derived from the cluster secret). Peer requests authenticate with a shared cluster secret header (constant-time compared, never logged) and always operate on the local node only. Control requests such as anti-entropy object listings have a 15-second deadline; object downloads have a 15-minute deadline, while uploads use 60 seconds plus 2 seconds per MiB with the same 15-minute cap. Object availability uses write-through replication with a `min(replication_factor, live nodes)` copy quorum, sequential peer fetch-on-miss with content-hash re-verification, and periodic anti-entropy list-and-repair. Raft was evaluated and rejected for this data model: immutable content-addressed objects commute, so consensus would add a leader bottleneck without adding an invariant.
- The server drains gracefully on SIGINT/SIGTERM (stop accepting, finish in-flight requests within a bound, leave the membership view, close storage). Storage maintenance follows the git model. New objects are always written loose; a maintenance pass computes reachability once from all ref tips, prunes unreachable objects older than a two-week default grace, and repacks aged reachable loose objects into pack files (zstd-per-entry with a JSON offset index; the pack's largest object acts as a raw dictionary so consecutive near-identical snapshots compress against their shared content — verbatim bytes inside, so hashes, replication, and fetch-on-miss are unaffected; full delta chains are deferred). Pack rewrites drop aged unreachable entries. A repository with an incomplete local DAG is aborted, never partially collected. Maintenance runs automatically after external publishes once the loose count crosses a threshold (`gc.auto: false` disables), optionally on a fixed interval, and manually via `--gc`. Every entry path attempts the same nonblocking advisory lock at `<storage-root>/.maintenance.lock`, so overlapping maintenance against one local storage root returns an observable skipped result instead of running concurrently.
- `candidate import/list/show/promote` consumes a versioned explicit local envelope. Candidate snapshots and draft notes live in dedicated SQLite tables and do not enter pending notes, immutable objects, FTS, normal context output, or remote transfer. Promotion is explicit and only accepts a merged candidate whose merge SHA equals current Git HEAD; it maps exact/changed/missing entity evidence to active/needs-review/historical outcomes atomically.
- `candidate publish <remote> --change <provider-key>` builds and publishes a source-bound context delta without advancing the local context ref. The GitHub coordinator validates provider metadata, plans preview/final outcomes, and can land a schema-v4 receipt only after GitHub reports the source merge. Durable jobs, generations, leases, fencing tokens, and desired Check state make retries idempotent. `landing list/show/recover/session show/resolve/commit` is the explicit local recovery surface for blocked landings.
- The coordinator's supported production mode is `durable_single`: webhook ingress and queued jobs survive process restarts, but planning pauses while the single coordinator is unavailable. The `ha` mode is intentionally rejected. Runner backends are `process`, opt-in `in_process`, Docker, and Kubernetes Job; Docker and Kubernetes use durable attempt records and explicit cleanup contracts.
- `thread-keep-mcp` exposes ten local stdio tools: eight reads (`search`, three context-assembly tools, `context_get`, `related_context`, `status`, and `diff`) and two pending-draft writes (`note_add`, `note_revise`). It exposes no source edit, context commit, push, or remote-write operation, and forces agent origin for drafted notes.
- Human output is concise. `--json` writes a versioned success envelope to stdout; errors use a versioned envelope on stderr and map stable domain errors to process exit codes.

## Module boundaries

| Module | Responsibility |
| --- | --- |
| `cmd/thread-keep` | Process entry point and Cobra root composition |
| `cmd/thread-keep-server` | Context-remote server entry point: flags, configuration loading and HTTP listener |
| `cmd/thread-keep-coordinator` | Durable single-coordinator process: job loops, runner selection, reconciliation, and shutdown |
| `cmd/thread-keep-runner` | Isolated source-indexing worker over bounded stdin/stdout or file protocols |
| `cmd/thread-keep-mcp` | MCP stdio server entry point exposing read tools and pending-note drafting to agents |
| `internal/coordinator` | Planning/control job application services and durable runtime loops |
| `internal/forge` | Provider-neutral repository, change, webhook, and Check contracts |
| `internal/forge/github` | GitHub App metadata, webhook verification/normalization, and Check adapter |
| `internal/mcpserver` | Official Go SDK tool adapter over the application service; protocol lifecycle stays in the SDK and agent origin is enforced on all writes |
| `internal/cli` | Command adapters, service lifecycle, human/JSON output and exit-code mapping |
| `internal/app` | Use cases: initialization, indexing, notes, search, context, commit, semantic merge, log and ordered remote operations |
| `internal/gitrepo` | Git worktree discovery, stable history-root repository identity and mutable-state checks |
| `internal/indexer` | Go entity extraction and stable identity generation |
| `internal/indexing` | Language detection, pack process protocol and index coordination |
| `internal/planner` | Deterministic source evidence extraction and PR context plan inputs |
| `internal/retrieval` | Bounded context selection, filtering, completeness, and diagnostics |
| `internal/remote` | Transport contract, filesystem and HTTP implementations: object publication, verified reads and versioned remote-ref CAS |
| `internal/remote/server` | HTTP remote, webhook/candidate/landing APIs, coordinator persistence, GitHub permission verification, object storage, ref CAS, clustering, and GC |
| `internal/runner` | Durable runner lifecycle plus process, in-process, Docker, and Kubernetes Job backends |
| `internal/store` | SQLite projection, FTS5 search, object storage and local ref CAS |
| `internal/domain` | Context entities, commits, notes and typed error codes |

## Local data integrity

The SQLite database is the local current-state projection; it is not synchronized as a database file. A committed context object is written before its SQLite metadata/ref transaction. If that transaction fails, the object can be unreachable but no pending note or ref is silently lost.

`commit` snapshots the current entity and note state, and `FinalizeCommit` compares the snapshot's pending note IDs with the current pending set before metadata mutation. A mismatch returns `concurrent_update` rather than deleting a newly added note.

`context merge commit` likewise writes its immutable object before finalization. The final SQLite transaction verifies its ready session, empty pending set, expected local ref and source, records both ordered parents, advances the local ref, and marks the session committed together. A failed CAS can leave an unreachable object but leaves the ref and session unchanged for an explicit retry.

## Build constraints

SQLite uses `github.com/mattn/go-sqlite3` with `sqlite_fts5 sqlite_omit_load_extension` build tags. Builds therefore require `CGO_ENABLED=1`; cross-platform releases need native runners or an appropriate C cross-toolchain.

## Deferred work

The following items are not part of the current implementation:

- End-user GitHub OAuth device flow, GitHub App authentication for ordinary remote clients, non-GitHub authorization providers, permission caching, and built-in TLS termination. Production images exist, but TLS termination remains an external deployment responsibility.
- Multi-coordinator HA. `durable_single` persists work across restarts and rejects an overlapping coordinator; lease renewal plus multi-coordinator partition/fencing validation is still required before `ha` can be enabled.
- Object location indexing for larger clusters and S3-compatible object storage backends.
- A remote read-only MCP endpoint backed by a server-side derived projection. Local stdio MCP remains the only MCP surface and the only pending-draft write path.
- The agent-integration evaluation harness defined in [evaluation.md](evaluation.md), automatic session-end draft hooks, and a PR-to-candidate pull skill. The documented Claude Code hook is a user-installed best-effort reminder, not a built-in hook system.
- Additional language indexers beyond Go, TypeScript, JavaScript, Python, Java, Kotlin, and Rust; see [multi-language indexing](multilanguage-indexing.md).
- Graph-based caller/callee, import, data-flow, or impact analysis.
- Automatic AI generation, automatic hook installation, or automatic local context commits.
