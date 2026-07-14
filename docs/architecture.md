# Thread Keep Architecture

## Product boundary

`thread-keep` is a Go-based, local-first code-context VCS. It preserves context for code entities without source annotations; Git remains the source of truth for source files, commits and branches.

The current implementation deliberately covers a small local workflow:

```text
init -> update -> note add/revise/review -> status/diff/search/context get -> commit -> log
     -> rebuild <explicit-context-commit-id> after projection loss
     -> remote add/list/push/fetch/pull for explicit filesystem or HTTP context remotes
     -> context merge start/show/resolve/commit for explicit local semantic merges
```

Webhooks, graph enrichment, source mutation and automatic model calls are not implemented. Candidate support is intentionally limited to local provider-neutral envelope import and explicit promotion; remote support is limited to explicit object/ref synchronization with fast-forward-only pull, over either a filesystem path or the HTTP context-remote protocol served by `thread-keep-server`. Divergent same-source v3 snapshots can be merged only through the separate explicit local merge lifecycle.

The hosted topology keeps this separation: Git hosting remains authoritative for source commits, branches, pull requests, and repository identity. The separate Thread Keep context-remote service (`thread-keep-server`, first slice implemented) stores immutable ContextObjects, Context Snapshots, and context refs, and delegates authentication to GitHub by verifying the caller's bearer token against the mapped GitHub repository's pull/push permissions. Neither the client nor the server makes source files, native Git blobs/refs, or the local SQLite projection into context storage. The local implementation already uses Git-style three-way comparison over snapshot ancestry: only non-overlapping note records compose automatically; competing revisions or bindings require explicit resolution in a structured local conflict session before a two-parent merge snapshot can advance a local ref.

## Current implementation contract

- The CLI uses Cobra. `cmd/thread-keep` only composes dependencies and registers commands; `internal/cli` owns command lifecycle, rendering and exit-code policy.
- The core detects Go, TypeScript-family, JavaScript-family, Python-family, Java, Kotlin, and Rust files. Its Go indexer discovers functions, methods and types using `go/ast` and `go/parser`; separately installed TypeScript, JavaScript, Python, Java, Kotlin, and Rust packs use Tree-sitter. Go keys include the repository-relative directory and package; external-pack keys include language, path, kind and qualified name.
- Context is stored outside source files in the Git common directory under `thread-keep/`. SQLite/FTS5 is the local current-state projection; content-addressed files store immutable committed context objects.
- `update` accepts only a clean, committed, non-detached Git worktree. The indexed entity set records the matching Git HEAD SHA.
- Each detected language has coverage (`indexed`, `missing_pack` or `failed`). Search and `context get` include only fresh indexed coverage, and `commit` rejects incomplete coverage.
- `search <query>` returns lexical evidence (`matched_fields`, terms, active note IDs, binding state, freshness, and snippet) with deterministic precedence for exact key, exact name, active note, signature/path, then FTS rank. `context related <entity-key> --limit` is a bounded one-hop structural view containing only method-owner and same-file edges; it does not claim call, import, data-flow, or impact edges.
- `indexers install --detected` is the only pack-installing command. A release binary validates a signed official manifest, selected platform artifact, byte size and SHA-256 before atomically publishing a fixed-path executable; `init` and `update` never call it.
- A ContextNote has a stable logical ID, an immutable revision body, and a current entity binding. `note add` creates the first pending active revision; `note revise` creates a successor revision without overwriting prior history; `note review` is the explicit local transition from `needs_review` to `active`. Supported kinds are `intent`, `decision`, `constraint`, `example` and `warning`.
- `update` reconciles active bindings after indexing: unchanged exact entities remain active, one unique same-language/same-kind structural match moves the binding, changed or ambiguous lineage becomes `needs_review`, and no candidate becomes `historical`. Search and `context get` expose only active effective bindings; `diff` exposes pending state transitions.
- The working set is scoped to the Git worktree, branch-derived context ref and source SHA. Linked worktrees do not share pending notes.
- `commit -m <message>` writes an immutable object, then atomically advances the local context ref through SQLite metadata. New objects use Context Snapshot schema v3: provenance is complete/sorted indexed language evidence, every canonical note revision has a checked entity mapping, and the object ID remains content-addressed. A normal v3 commit has one ordered `parent_ids` entry; the first v3 commit after legacy history uses `legacy_parent_id` to preserve object ancestry without inventing legacy provenance. The commit stops if the source SHA, parent ref or snapshot pending-note IDs changed, preserving pending notes on conflict.
- `context merge start/show/resolve/commit` performs an explicit local semantic three-way merge of selected v3 snapshots. Start requires the current source, matching repository/ref/provenance, an empty pending set, and a local input equal to the current local ref; commit additionally requires matching entity sets. A deterministic merge base selects automatic records or structured conflicts. A session persists local/remote/base IDs and explicit resolutions; the final deterministic object preserves ordered `[local, remote]` parents, then one SQLite transaction CAS-advances only the local ref and marks the session committed. Authored resolutions always create a source-bound immutable successor revision; no source mutation, text conflict marker, automatic remote write, or implicit retry occurs.
- `rebuild <context-commit-id>` validates one explicitly selected immutable object DAG and atomically restores an empty SQLite projection. It never infers a canonical ref from object files. A v3 snapshot additionally requires an identical current clean Git source SHA and complete matching indexer provenance before SQLite mutation; v1/v2 keep legacy rebuild behavior.
- `remote add/list/push/fetch/pull` uses a named absolute filesystem path or an HTTP(S) context-remote URL, never a Git remote alias. Plain `http://` is accepted only for loopback hosts so a bearer token never travels unencrypted. Both transports implement the same four-operation contract (`ReadObject`, `PublishObject`, `ReadRef`, `CompareAndSwapRef`) and transfer only hash-verified immutable object DAGs and versioned context refs. Push publishes objects before remote ref CAS; fetch writes only local immutable objects and a tracking ref; pull has a no-pending precondition and applies only a same-source fast-forward. Divergence, remote-ahead state and CAS loss return `remote_conflict`; callers choose `context merge` explicitly when a fetched snapshot needs semantic resolution.
- The HTTP client sends the GitHub token from `THREAD_KEEP_REMOTE_TOKEN` as a bearer header and never persists it in SQLite, configuration, output, or errors. `thread-keep-server` maps each `repository_id` URL segment to a configured GitHub `owner/repo`, verifies the presented token's pull permission for reads and push permission for writes against the configured GitHub API base URL, and distinguishes provider outage (502) from authorization denial (401/403, exit code 8 via error code `auth`). A genuinely absent object returns 404 with error code `object_missing`, which the HTTP client preserves for programmatic handling; pack or index corruption remains a distinct validation/storage failure and never triggers fetch-on-miss. Object uploads are re-verified against their content ID on the server before atomic publication.
- Server storage splits by data shape: immutable context objects are verbatim content-addressed files (never re-serialized or decomposed into tables, so byte-level hashes, unknown future schema fields, and DAG integrity survive), while versioned context refs live in a GORM-backed `context_refs` table with transactional optimistic compare-and-swap (embedded database file by default, PostgreSQL via `--db-dsn`). The ref database is the cluster coordination point; objects need no ordering consensus because identical IDs imply identical bytes.
- Cluster mode runs N nodes behind a load balancer over one shared ref database. Membership sits behind a `Membership` interface with two modes: `db` (default — heartbeat UPSERTs stamped with the database clock, a TTL window filter at query time, active row deletion only on graceful leave) and `swim` (hashicorp/memberlist gossip with static seeds and a transport key derived from the cluster secret). Peer requests authenticate with a shared cluster secret header (constant-time compared, never logged) and always operate on the local node only. Control requests such as anti-entropy object listings have a 15-second deadline; object downloads have a 15-minute deadline, while uploads use 60 seconds plus 2 seconds per MiB with the same 15-minute cap. Object availability uses write-through replication with a `min(replication_factor, live nodes)` copy quorum, sequential peer fetch-on-miss with content-hash re-verification, and periodic anti-entropy list-and-repair. Raft was evaluated and rejected for this data model: immutable content-addressed objects commute, so consensus would add a leader bottleneck without adding an invariant.
- The server drains gracefully on SIGINT/SIGTERM (stop accepting, finish in-flight requests within a bound, leave the membership view, close storage). Storage maintenance follows the git model. New objects are always written loose; a maintenance pass computes reachability once from all ref tips, prunes unreachable objects older than a two-week default grace, and repacks aged reachable loose objects into pack files (zstd-per-entry with a JSON offset index; the pack's largest object acts as a raw dictionary so consecutive near-identical snapshots compress against their shared content — verbatim bytes inside, so hashes, replication, and fetch-on-miss are unaffected; full delta chains are deferred). Pack rewrites drop aged unreachable entries. A repository with an incomplete local DAG is aborted, never partially collected. Maintenance runs automatically after external publishes once the loose count crosses a threshold (`gc.auto: false` disables), optionally on a fixed interval, and manually via `--gc`. Every entry path attempts the same nonblocking advisory lock at `<storage-root>/.maintenance.lock`, so overlapping maintenance against one local storage root returns an observable skipped result instead of running concurrently.
- `candidate import/list/show/promote` consumes only a versioned explicit local envelope. Candidate snapshots and draft notes live in dedicated SQLite tables and do not enter pending notes, immutable objects, FTS, normal context output, or remote transfer. Promotion is explicit and only accepts a merged candidate whose merge SHA equals current Git HEAD; it maps exact/changed/missing entity evidence to active/needs-review/historical outcomes atomically.
- Human output is concise. `--json` writes a versioned success envelope to stdout; errors use a versioned envelope on stderr and map stable domain errors to process exit codes.

## Module boundaries

| Module | Responsibility |
| --- | --- |
| `cmd/thread-keep` | Process entry point and Cobra root composition |
| `cmd/thread-keep-server` | Context-remote server entry point: flags, configuration loading and HTTP listener |
| `cmd/thread-keep-mcp` | MCP stdio server entry point exposing read tools and pending-note drafting to agents |
| `internal/mcpserver` | Official Go SDK tool adapter over the application service; protocol lifecycle stays in the SDK and agent origin is enforced on all writes |
| `internal/cli` | Command adapters, service lifecycle, human/JSON output and exit-code mapping |
| `internal/app` | Use cases: initialization, indexing, notes, search, context, commit, semantic merge, log and ordered remote operations |
| `internal/gitrepo` | Git worktree discovery, stable history-root repository identity and mutable-state checks |
| `internal/indexer` | Go entity extraction and stable identity generation |
| `internal/indexing` | Language detection, pack process protocol and index coordination |
| `internal/remote` | Transport contract, filesystem and HTTP implementations: object publication, verified reads and versioned remote-ref CAS |
| `internal/remote/server` | HTTP context-remote server: routing, GitHub permission verification, file-backed object storage and DB-backed ref CAS |
| `internal/store` | SQLite projection, FTS5 search, object storage and local ref CAS |
| `internal/domain` | Context entities, commits, notes and typed error codes |

## Local data integrity

The SQLite database is the local current-state projection; it is not synchronized as a database file. A committed context object is written before its SQLite metadata/ref transaction. If that transaction fails, the object can be unreachable but no pending note or ref is silently lost.

`commit` snapshots the current entity and note state, and `FinalizeCommit` compares the snapshot's pending note IDs with the current pending set before metadata mutation. A mismatch returns `concurrent_update` rather than deleting a newly added note.

`context merge commit` likewise writes its immutable object before finalization. The final SQLite transaction verifies its ready session, empty pending set, expected local ref and source, records both ordered parents, advances the local ref, and marks the session committed together. A failed CAS can leave an unreachable object but leaves the ref and session unchanged for an explicit retry.

## Build constraints

SQLite uses `github.com/mattn/go-sqlite3` with `sqlite_fts5 sqlite_omit_load_extension` build tags. Builds therefore require `CGO_ENABLED=1`; cross-platform releases need native runners or an appropriate C cross-toolchain.

## Deferred work

- Context-remote hardening: GitHub OAuth device flow and GitHub App auth, non-GitHub providers, unreachable-object garbage collection, permission caching, TLS termination and deployment artifacts for `thread-keep-server`, and hosted coordination around the existing local semantic merge lifecycle
- Context-remote clustering follow-ups: object location indexing for larger clusters and S3-compatible object storage backends
- Remote read-only MCP endpoint backed by a server-side derived projection (transfer objects stay verbatim); local stdio MCP remains the write path
- The agent-integration evaluation harness defined in [evaluation.md](evaluation.md); session-end draft hooks and the PR-to-candidate pull skill
- Live provider adapters for source/PR identity, webhooks, token storage and automatic candidate promotion
- Additional language indexers beyond Go, TypeScript, JavaScript, Python, Java, Kotlin, and Rust; see [multi-language indexing design](multilanguage-indexing.md)
- Graph-based impact analysis
- Automatic AI generation or hook installation
