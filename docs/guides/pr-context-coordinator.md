# PR Context Planning and Landing

This is an advanced, opt-in operator guide. You do not need the coordinator or
runner for local notes, MCP drafting, or ordinary remote synchronization. Start
with the [Quickstart](quickstart.md) and [Team server](team-server.md) first.

Current support is deliberately bounded:

| Capability | Status |
| --- | --- |
| Durable GitHub webhook intake and queued work | Implemented |
| Preview planning and informational Checks | Implemented |
| Opt-in automatic landing plus manual recovery | Implemented |
| Process, in-process, Docker, and Kubernetes Job runners | Implemented |
| Multi-coordinator HA | Not implemented; `durable_single` only |
| Live Kubernetes distribution/storage-class certification | Opt-in per target cluster |

Deploy server, coordinator, and runner from the same release. The coordinator and
runner are GitHub Release/container artifacts rather than local PyPI-wheel commands.

Thread Keep can run an opt-in GitHub PR coordinator beside the existing immutable
object/ref remote. GitHub remains the source-merge authority. The coordinator only plans
context changes, publishes an informational Check, and—when explicitly enabled—lands a
schema-v4 context snapshot after GitHub reports the source merge.

The production topology has two long-running deployables plus the isolated runner executable:

- `thread-keep-server` serves the context remote, authenticated candidate/plan/recovery APIs, and the
  GitHub webhook route. Webhook handling verifies signatures and repository bindings, then atomically
  writes the normalized event and `process_webhook` job before returning `202`. It does not poll or
  execute planning jobs.
- `thread-keep-coordinator` owns the bounded control/planning worker pools and dispatches each
  planning job through the configured `process`, `in_process`, `docker`, or `kubernetes_job`
  Runner backend.

The initial mode is **Durable Single Coordinator**: webhook acceptance and queued work survive
coordinator restarts, but planning pauses while that one coordinator is unavailable. It is not
Coordinator HA. The `ha` mode is rejected until lease renewal and multi-coordinator
partition/fencing tests are implemented.

## Prerequisites

- A GitHub App installation for the configured repository.
- App permissions: metadata read, contents read, pull requests read, and checks write.
  Source write and pull-request merge permissions are not required.
- `thread-keep-server`, `thread-keep-coordinator`, and `thread-keep-runner` from the same release.
- Shared PostgreSQL for every server replica and the coordinator. Embedded SQLite is for
  local/single-process development, not active-active ingress.
- Object storage readable and writable by both the server and coordinator.
- An initialized canonical context ref for each configured target branch.
- Complete official indexer coverage for the repository languages.

Each process reads only its required secrets at startup:

- Planning-enabled context server: `THREAD_KEEP_GITHUB_WEBHOOK_SECRET` for webhook verification and
  `THREAD_KEEP_GITHUB_APP_PRIVATE_KEY_FILE` for authoritative candidate metadata. It executes no
  Runner Process.
- Coordinator: `THREAD_KEEP_GITHUB_APP_PRIVATE_KEY_FILE` only, plus short-lived installation
  tokens minted in memory for one provider operation or Runner Process checkout.

Use `examples/server.env.example` and `examples/coordinator.env.example` separately. Do not load the
Server-only webhook secret into the Coordinator process.

Do not place secret values in the JSON config, command arguments, logs, or repository.

### Runner backends

`process` is the compatibility default and uses the existing bounded stdin/stdout worker protocol.
`in_process` is an explicit development option; cancellation is cooperative because Go cannot
force-stop one goroutine. Remote backends use durable attempt rows, deterministic external resource
identity, claim fencing and the file protocol. A terminal attempt is cleanup-pending in the same DB
transition that records its result.

Docker requires an explicit Engine endpoint, immutable image, network, CPU/memory/workspace bounds
and cleanup TTL. The Coordinator never discovers or mounts a socket implicitly. The image must contain
`/usr/local/bin/thread-keep-runner`. The container uses a non-root user, read-only rootfs, dropped
capabilities, `no-new-privileges`, no restart policy, a bounded checkout tmpfs and separate request,
credential and result tmpfs mounts. Because Docker Archive APIs cannot access tmpfs, fixed-path,
unprivileged Engine exec streams upload request/credential files and download the bounded result.
Credential delivery is attempted once per container; an ambiguous response fails that attempt.

```json
{
  "runner": {
    "backend": "docker",
    "timeout_seconds": 120,
    "reconcile_interval_seconds": 30,
    "artifacts": {"max_request_bytes": 1048576, "max_result_bytes": 16777216},
    "docker": {
      "endpoint": "unix:///run/user/1000/docker.sock",
      "image": "registry.example/thread-keep-runner@sha256:replace-with-64-hex-digest",
      "network": "thread-keep-runner",
      "cpu_limit_millis": 2000,
      "memory_limit_bytes": 1073741824,
      "workspace_limit_bytes": 2147483648,
      "cleanup_ttl_seconds": 600
    }
  }
}
```

Prefer a protected rootless Engine endpoint. A Docker socket gives the Coordinator broad authority
over that Engine. For a TLS TCP endpoint, configure the endpoint in JSON and provide client
certificates through Docker's standard `DOCKER_CERT_PATH` and `DOCKER_TLS_VERIFY` environment
variables; never put certificate contents in JSON.

`kubernetes_job` uses in-cluster client credentials, a dedicated namespace, a Coordinator Service
Account allowed to manage namespaced Jobs and temporary Secrets, and a separate token-free Job
Service Account. Standard RBAC cannot restrict `create` by label; use a dedicated namespace and,
if required, admission policy. The artifact directory must be the Coordinator mount of the named RWX
PVC and use the same fsGroup as Jobs. Kubernetes Secret data is encrypted only if the cluster has
etcd encryption at rest configured.

```json
{
  "runner": {
    "backend": "kubernetes_job",
    "timeout_seconds": 120,
    "reconcile_interval_seconds": 30,
    "artifacts": {
      "directory": "/run/thread-keep-artifacts",
      "max_request_bytes": 1048576,
      "max_result_bytes": 16777216
    },
    "kubernetes_job": {
      "image": "registry.example/thread-keep-runner@sha256:replace-with-64-hex-digest",
      "namespace": "thread-keep-runners",
      "job_service_account": "thread-keep-runner-job",
      "artifact_claim": "thread-keep-runner-artifacts",
      "artifact_fs_group": 65532,
      "cpu_request_millis": 500,
      "cpu_limit_millis": 2000,
      "memory_request_bytes": 536870912,
      "memory_limit_bytes": 1073741824,
      "ttl_seconds_after_finished": 600
    }
  }
}
```

Each Job has `backoffLimit=0`, `restartPolicy=Never`, an active deadline, finished-Job TTL and
restricted Pod/container security contexts. Coordinator owns logical retry. The temporary immutable
Secret is deleted after result observation or cleanup. A matching atomic result in the RWX store is
authoritative even if the TTL controller has already deleted the Job.

The mandatory Kubernetes verification uses the official client-go fake client to lock Job, Secret,
artifact, rediscovery, TTL-race and cleanup contracts. A live cluster E2E is opt-in because it creates
a namespace, ServiceAccounts, Role/RoleBinding, PVC, Secret and Job. The current implementation is not
claiming live Kubernetes distribution or storage-class coverage until that gate is run in the target
cluster. The Docker backend additionally has an opt-in real-Engine transport and native-runner E2E.

## Configuration

```json
{
  "github_api_base_url": "https://api.github.com",
  "github_app": {
    "app_id": 12345,
    "installation_id": 67890
  },
  "repositories": {
    "my-repo": {
      "github_owner": "acme",
      "github_repo": "service",
      "context_repository_id": "git-roots:replace-with-repository-id",
      "planning": {
        "enabled": true,
        "target_branches": ["main"],
        "check_mode": "informational",
        "automatic_landing": false,
        "context_schema": 4,
        "max_attempts": 3
      }
    }
  }
}
```

The current implementation accepts exactly one target branch per remote repository key. Use a
separate key when a deployment needs another target branch. `automatic_landing` is
false by default; this enables preview planning without canonical context writes.

Start both long-running processes against the same PostgreSQL database. The object root shown for server and
coordinator must denote the same shared storage view. `--runner-path` is a `process`-backend override;
omit it for `in_process`, `docker`, and `kubernetes_job`:

```sh
thread-keep-server \
  --listen 0.0.0.0:8320 \
  --storage /var/lib/thread-keep \
  --config /etc/thread-keep/server.json \
  --db-dsn postgres://thread-keep@postgres/thread_keep

thread-keep-coordinator \
  --storage /var/lib/thread-keep \
  --config /etc/thread-keep/server.json \
  --db-dsn postgres://thread-keep@postgres/thread_keep \
  --runner-path /usr/local/bin/thread-keep-runner \
  --mode durable_single \
  --replicas 1 \
  --workers 2
```

Configure the GitHub App webhook URL as:

```text
https://context.example.com/v1/providers/github/webhooks
```

Subscribe to pull request events. Delivery signatures and provider/installation/repository/target
branch bindings are checked before a process job is created. A successful `202` means the delivery
and its process job committed in one transaction; it does not mean GitHub refresh or planning has
already run. Duplicate delivery IDs with the same payload are no-ops, while ID reuse with a
different payload is rejected.

## Rollout

1. Expand the database schema by deploying a release that can read durable payload version 1.
   Upgrade server, CLI, coordinator, and runner together. Confirm the capabilities response includes
   context object v4, candidate context v2, planning, and recovery.
2. Start `thread-keep-coordinator` with `--mode durable_single --replicas 1`. A DB-clock
   heartbeat rejects an overlapping coordinator instance. This is an overlap guard, not HA.
3. Route the GitHub webhook to at least two `thread-keep-server` replicas backed by the shared
   PostgreSQL database. Stop the coordinator briefly and verify webhook `202` responses continue while
   the durable backlog grows.
4. Set `planning.enabled` to true and leave `automatic_landing` false. Observe preview
   Checks and worker coverage without allowing canonical writes.
5. Publish PR-authored context when applicable:

   ```sh
   thread-keep candidate publish origin --change github:acme/service#42
   ```

6. Verify backup/restore of the object store and ref/coordinator database. Both are needed:
   objects alone do not retain job and intent state, while the database alone does not
   retain immutable snapshots.
7. Enable `automatic_landing` for a test repository. Exercise successful, duplicate,
   conflicting, and recovery landings before wider opt-in.

## Recovery

When automatic landing reaches `blocked`, check out the exact GitHub merge commit in a
clean worktree. The recovery endpoint requires repository push permission because it
claims the intent for recovery.

```sh
thread-keep landing list origin
thread-keep landing show origin <landing-id>
thread-keep landing recover origin <landing-id>
thread-keep landing session show <session-id>
```

If conflicts exist, resolve each one with the stable conflict ID:

```sh
thread-keep landing resolve <session-id> <conflict-id> --use canonical
thread-keep landing resolve <session-id> <conflict-id> --use candidate
thread-keep landing resolve <session-id> <conflict-id> --use authored --file note.json
```

Commit and push after the session becomes `ready`:

```sh
thread-keep landing commit <session-id> -m "Recover context landing" --author <name>
thread-keep remote push origin
```

The local commit contains the same `LandingReceipt` contract as automatic landing. The
ordinary push detects that receipt and atomically advances the server ref, indexes the
receipt, and marks the durable intent `landed`. Retrying the same push is a no-op.

Recovery deliberately does not weaken ordinary pull rules. Only the recovery session may
fast-forward context across the source-merge SHA gap, and only when the local context tip
is an ancestor of the server canonical tip.

## Operations

- The coordinator has one control worker and 1..32 planning workers (default 2). Each goroutine claims
  only when it is free; leased jobs are never prefetched into a local queue. Final jobs have higher
  priority than preview jobs.
- The required timeout order is
  `executor_timeout + finalize_margin < job_timeout < lease_duration`; defaults are
  `2m + 30s < 3m < 4m`.
- Durable execution is at-least-once. Monotonic claim fencing rejects stale completions, preview
  publication verifies the current generation, and landing uses receipt/ref CAS as the final
  correctness gate.
- Desired GitHub Check state is committed with planning or landing state. Provider publication is
  a separate durable job. Reconciliation looks up the stable external ID, persists the canonical
  Check run ID, and marks duplicate runs superseded. A GitHub outage after landing never repeats
  the landing transaction.
- Durable payloads use an explicit schema version. An unsupported version becomes an
  `incompatible` terminal job instead of retrying forever. Expand readers before advancing writers,
  and contract old readers only after the old backlog is empty.
- Checkout credentials are short-lived and passed only to one Runner Process. Hooks,
  submodules, LFS, credential helpers, global/system Git configuration, and redirects are
  disabled in runner checkouts.
- Remote Runner rows persist only non-secret request/spec digests and versioned handles. Docker keeps
  credentials only in tmpfs; Kubernetes stores them in one temporary namespaced Secret. Neither raw
  requests nor credential bytes are stored in the coordinator database.
- Coordinator worker logs contain only typed error codes. Server logs do not contain webhook payloads;
  neither process logs note bodies, access tokens, or secret values.
- Invalid evidence, incomplete coverage, authorization denial, and stale merged state are
  terminal blocks. Transient provider, worker, object, or database failures use bounded
  retries and then become recoverable blocks.
- A published object that loses the final ref CAS is a harmless orphan and may be collected
  by the existing grace-period GC after no ref or retained ancestry reaches it.

Before an upgrade or restore, stop the coordinator or make the service read-only,
snapshot the SQL database and object root consistently, then restore both before accepting
webhooks. After restore, duplicate GitHub deliveries and existing receipts safely converge
without a second context landing.
