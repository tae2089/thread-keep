# Thread Keep Read-Only CLI Contract

Run commands in the target Git worktree or pass `--repo <path>`.

```sh
thread-keep --json status
thread-keep --json search <query>
thread-keep --json context get <entity-key>
```

JSON success is written to stdout as `{"version":1,"data":...}`. JSON errors are written to stderr as `{"version":1,"error":{"code":...,"message":...}}`.

Important error handling:

- `not_initialized`: state that durable context is unavailable; do not initialize during review.
- `stale_working_set`: do not treat returned context as current or run `update` during a read-only review.
- `entity_not_found`: search for the exact indexed entity key; if still absent, report the context gap.
- Missing language coverage: report which changed language lacks indexed context.

Search and context output are evidence surfaces. Do not infer that an unreturned or inactive note does not exist in history.
