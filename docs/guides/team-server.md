# Running the Thread Keep context remote for a team

This guide is for operators who need a shared HTTP(S) context remote. If a shared
filesystem path is enough, use the simpler remote flow in the
[Quickstart](quickstart.md#sharing-with-your-team); you do not need to run a server.

The shortest production reading path is:

1. Read **What the server stores and never stores**.
2. Complete **Single-node setup** behind external TLS termination.
3. Use **Team onboarding and handover** for clients.
4. Add clustering only when one node is no longer sufficient.
5. Include both the object root and ref database in backup and recovery.

The `thread-keep-server` binary is an operational GitHub Release/container
artifact; it is not included in the local `thread-keep` PyPI wheel.

`thread-keep-server` is the self-hosted context-remote server. It lets a team
publish and inherit code context — the intent, decisions, constraints, examples,
and warnings captured against code entities — independently of the Git host that
owns the source. This guide covers a single-node install, the GitHub-delegated
authorization model, team onboarding, multi-node clustering, storage
maintenance, backup, and how divergence is resolved.

The server never touches source. It exchanges only content-addressed context
objects and versioned context refs; Git hosting stays authoritative for source
commits, branches, and repository identity.

## What the server stores and never stores

| Stored | Not stored |
| --- | --- |
| Immutable context objects as verbatim content-addressed files (later zstd packs after maintenance) | Source files |
| Versioned context refs in a database (embedded file by default, PostgreSQL via `--db-dsn`) | Native Git blobs or refs |
| | GitHub tokens or any credential |
| | The client's local SQLite projection |
| | Pending (uncommitted) notes |

Objects are stored byte-for-byte and addressed by content hash, so identical IDs
imply identical bytes. That is why objects need no ordering consensus and why the
ref database is the only coordination point. Refs live in a `context_refs` table
with transactional optimistic compare-and-swap.

## Single-node setup

### Configuration file

The server loads a JSON config through `--config`. The full schema for a
single node is small:

```json
{
  "github_api_base_url": "https://api.github.com",
  "repositories": {
    "my-repo": { "github_owner": "acme", "github_repo": "thread-keep" }
  }
}
```

| Key | Meaning |
| --- | --- |
| `github_api_base_url` | GitHub REST base URL used for permission checks. Defaults to `https://api.github.com` when omitted. Point it at your GitHub Enterprise API base to authorize against Enterprise. |
| `repositories` | Map of `repository_id` to a GitHub `owner/repo`. The `repository_id` is the URL segment clients address (`/v1/repositories/<id>`) and must be a safe path segment (no `/`, `\`, `.`, or `..`). |
| `repositories.<id>.github_owner` | GitHub owner (user or org) that owns the mapped repository. Required. |
| `repositories.<id>.github_repo` | GitHub repository name. Required. |

### Running the server

`--storage` and `--config` are both required.

```bash
thread-keep-server \
  --listen 127.0.0.1:8320 \
  --storage /var/lib/thread-keep \
  --config /etc/thread-keep-server/config.json
# optional external ref database:
#   --db-dsn postgres://user@host:5432/threadkeep
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen` | `127.0.0.1:8320` | TCP listen address. The server always serves plaintext HTTP. |
| `--storage` | (required) | Absolute path for the object store. Created if absent; also holds the embedded ref database by default. |
| `--config` | (required) | Path to the repository-mapping config above. |
| `--db-dsn` | `<storage>/refs.db` | Ref database DSN. Use `postgres://...` for an external database; otherwise an embedded database file path. |
| `--gc` | off | Run one garbage-collection pass and exit without serving (see Storage maintenance). |
| `--gc-grace` | `336h` (two weeks) | With `--gc`, objects newer than this age are never collected. |

A minimal systemd unit:

```ini
[Unit]
Description=Thread Keep context remote
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/thread-keep-server \
  --listen 127.0.0.1:8320 \
  --storage /var/lib/thread-keep \
  --config /etc/thread-keep-server/config.json
Restart=on-failure
# In cluster mode only, provide the peer secret out of band:
#   Environment=THREAD_KEEP_CLUSTER_SECRET=<cluster-secret>
User=thread-keep
Group=thread-keep

[Install]
WantedBy=multi-user.target
```

### TLS and the loopback rule

The server terminates plaintext HTTP only. In production, run it behind a
reverse proxy (nginx, Caddy, a cloud load balancer) that terminates TLS and
forwards to the listen address. Clients send a GitHub token as a bearer header,
so the connection must be encrypted end to end.

The client enforces this: an `https://` remote URL is always accepted, but a
plain `http://` remote URL is accepted **only for loopback hosts** so a token
never travels unencrypted. Publish the server to teammates over `https://`; use
`http://127.0.0.1:...` only for a local proxy sidecar or local testing.

## Authorization model

Authorization is delegated entirely to GitHub. There is no user database on the
server.

- The client reads the token from the `THREAD_KEEP_REMOTE_TOKEN` environment
  variable and sends it as a bearer header. The token is never stored in
  config, in the database, or in any output or error message.
- For each request, the server looks up the addressed `repository_id`, resolves
  the mapped GitHub `owner/repo`, and asks the GitHub API about the caller's
  permissions on that repository.
- **Reads** (fetch, pull) require the token to have **pull** permission.
- **Writes** (push) require **push** permission.

Failure modes are distinct and stable:

| Situation | Result |
| --- | --- |
| Missing/invalid token, or token lacks the required permission | Error code `auth`, CLI exit code `8` |
| GitHub itself is unreachable or erroring | A 502-style storage error, reported as distinct from denial |
| Addressed `repository_id` not configured | Not found |

Distinguishing a GitHub outage from a real denial matters operationally: an
`auth` failure means the caller genuinely lacks access; a storage error means
retry once GitHub recovers.

### GitHub Enterprise

Set `github_api_base_url` to your Enterprise API base (for example
`https://github.example.com/api/v3`). Permission checks then run against
Enterprise. OAuth device flow, GitHub Apps, and non-GitHub providers are not
implemented; token-based delegation is the only auth path.

## Team onboarding and handover

This is the point of running the server: a new teammate inherits the previous
developers' captured context instead of rediscovering it. The flow is
client-side and takes a few commands.

```bash
# 1. Get the source the normal way.
git clone git@github.com:acme/thread-keep.git
cd thread-keep

# 2. Initialize local context storage and index the current source.
thread-keep init
thread-keep update

# 3. Point at the team's context remote (the repository_id from config.json).
thread-keep remote add origin https://context.example.com/v1/repositories/my-repo

# 4. Provide a GitHub token with pull access to the mapped repo.
export THREAD_KEEP_REMOTE_TOKEN=<github-token>

# 5. Pull the shared context.
thread-keep remote pull origin

# 6. Explore the inherited context.
thread-keep search "Authorize"
thread-keep context get payment.Authorize
```

`pull` refuses if there are pending local notes, then fast-forwards only when the
remote context tip contains the local tip and matches the current clean Git
source SHA. A fresh clone satisfies this, so a new teammate fast-forwards cleanly.

Coding agents inherit the same context automatically through the local MCP
server — see [Claude Code integration](./claude-code.md) and
[Codex integration](./codex.md). Agents read the pulled context and can draft
new pending notes, but they can never commit context, touch source, or reach the
remote; promotion stays an explicit human `thread-keep commit` followed by
`thread-keep remote push`.

## Multi-node cluster

### When you need it

Clustering is for **availability**, not throughput. Run multiple nodes behind a
load balancer when you need the context remote to survive a node failure or a
rolling restart. A single node is fine for a team that can tolerate brief
downtime.

### Requirements

- The shared ref database **must be PostgreSQL** in multi-node mode. All nodes
  point `--db-dsn` at the same PostgreSQL instance; the ref database is the
  cluster's coordination point.
- Every node must have the `THREAD_KEEP_CLUSTER_SECRET` environment variable set
  to the same value. It is **required** in cluster mode, is compared in constant
  time, and is never logged. Peer-to-peer requests authenticate with this secret
  and bypass GitHub verification, operating only on the local node so replication
  never fans out recursively.

### Cluster configuration

Add a `cluster` block. Defaults come from the server; set `node_id` and
`advertise_url` per node.

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

| Key | Default | Meaning |
| --- | --- | --- |
| `node_id` | (set per node) | Stable identifier for this node. |
| `advertise_url` | (set per node) | URL other nodes use to reach this node for peer requests. |
| `membership` | `db` | Membership mode: `db` (lease registry in the shared database) or `swim` (gossip). |
| `swim` | — | Present only when `membership` is `swim` (see below). |
| `replication_factor` | `2` | Target number of object copies. A write is acknowledged once `min(replication_factor, live nodes)` copies exist. |
| `heartbeat_seconds` | `10` | How often a node heartbeats into the shared database (`db` mode). |
| `ttl_seconds` | `30` | Age after which a non-heartbeating node drops out of the membership view. |
| `anti_entropy_seconds` | `300` | Interval of the background list-and-repair pass. |

Object availability comes from three consensus-free mechanisms: write-through
replication to live peers with a copy quorum, peer fetch-on-miss with
content-hash verification, and periodic anti-entropy repair. There is no Raft and
no leader — immutable content-addressed objects commute, so consensus would only
add a bottleneck.

### Membership modes

`db` (default) is a lease registry: each node heartbeats an UPSERT into the
shared ref database, stale rows fall out of the view by `ttl_seconds`, and a row
is actively removed only on a graceful leave. It assumes all nodes share one ref
database.

`swim` uses gossip (hashicorp/memberlist): nodes exchange liveness directly, the
gossip transport is encrypted with a key derived from the cluster secret, and a
new node joins through one or more seed addresses.

```json
"cluster": {
  "node_id": "node-a",
  "advertise_url": "https://node-a.internal:8320",
  "membership": "swim",
  "swim": { "bind_addr": "0.0.0.0:7946", "seeds": ["node-b.internal:7946"] }
}
```

| `swim` key | Meaning |
| --- | --- |
| `bind_addr` | Local address the gossip transport binds to. |
| `seeds` | One or more existing-node addresses used to join the cluster. |

### Behavior when a node dies

- Writes still succeed as long as the copy quorum `min(replication_factor, live nodes)`
  can be met.
- A read that lands on a node missing the object triggers fetch-on-miss from a
  peer, re-verified against the content hash before it is served.
- A node that was down catches up through fetch-on-miss and the periodic
  anti-entropy repair, so the cluster self-heals without operator action.
- On `SIGINT`/`SIGTERM` the server drains gracefully: it stops accepting
  connections, finishes in-flight requests (up to 10 seconds), **actively leaves
  the membership view**, and exits 0. A crash (no graceful leave) leaves the row
  to expire by `ttl_seconds` in `db` mode.

## Storage maintenance

Maintenance follows the Git model: automatic by default, conservative about
deletion, packing old objects.

A pass computes reachability once from every ref tip through parent links, then:

- deletes only **unreachable** objects older than the grace window (default two
  weeks, matching git's prune expiry),
- repacks aged reachable loose objects into a compressed pack file
  (`packs/pack-*.pack` with a JSON index). Entries are zstd-compressed verbatim
  object bytes; the largest object in a pack acts as a raw compression dictionary
  so near-identical snapshots share common content, without full delta chains.

Content addressing and hash verification are unchanged by packing. A repository
whose local DAG is incomplete is skipped entirely rather than partially collected.

### Automatic maintenance (default)

Like `git gc --auto`, one background pass runs after an externally published
object once the loose-object count crosses a threshold. Configure it with a `gc`
block:

```json
"gc": {
  "auto": true,
  "auto_threshold": 512,
  "grace_seconds": 1209600,
  "interval_seconds": 3600
}
```

| Key | Default | Meaning |
| --- | --- | --- |
| `auto` | `true` | Master switch. `true` even without a `gc` block; set `false` to disable all automatic maintenance. |
| `auto_threshold` | `512` | Loose-object count that triggers a background pass. Must be at least 1. |
| `grace_seconds` | `1209600` (14 days) | Unreachable objects younger than this are never deleted. Must be at least 60. |
| `interval_seconds` | (unset) | Optional fixed schedule for an additional periodic pass. Must be positive when set. Ignored when `auto` is `false`. |

### Manual offline pass

A manual pass stays available regardless of the `gc` setting and prints a JSON
report on stdout. Run it against an idle server (or a stopped one against the
same storage):

```bash
thread-keep-server --gc --gc-grace 336h \
  --storage /var/lib/thread-keep \
  --config /etc/thread-keep-server/config.json
```

The report lists, per repository, `kept`, `deleted`, `packed`, and whether the
pass `aborted` (an incomplete DAG).

## Backup and recovery

Because the two data shapes are stored separately, back them up separately:

- **Objects**: copy the `--storage` directory (loose objects and `packs/`). The
  files are content-addressed and immutable, so a file-level copy is consistent.
- **Refs**: back up the ref database — the embedded database file under the
  storage root, or your PostgreSQL instance when using `--db-dsn`.

Recovery is forgiving. Every client holds its own local context and can always
re-push it, so a lost server can be rebuilt from the team's working copies. If a
**client** loses its local SQLite projection (objects still present), it rebuilds
with `thread-keep rebuild <context-commit-id>` — see the
[storage verification and recovery section of the Quickstart](quickstart.md#verify-where-the-context-lives).

## Divergence

The server never merges automatically. When two clients push context that has
diverged, the losing push returns `remote_conflict` and the remote ref is left
unchanged — the server only ever CAS-advances a ref along a fast-forward.

Resolution is entirely client-side and explicit, through the
`thread-keep context merge` lifecycle: after fetching the competing same-source
snapshot, a developer runs `merge start`, inspects conflicts with `merge show`,
resolves each with `merge resolve` (choosing `local`, `remote`, or an authored
revision), and finalizes with `merge commit`, which advances only the local ref.
They then push again. Non-overlapping notes compose automatically; competing
revisions, bindings, or mappings must be resolved explicitly. See the
[same-source divergence section of the Quickstart](quickstart.md#resolve-same-source-divergence)
for the full command sequence.
