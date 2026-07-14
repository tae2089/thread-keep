# Thread Keep CLI Contract

Run commands in the target Git worktree or pass `--repo <path>`.

```sh
thread-keep --json status
thread-keep --json search <query>
thread-keep --json context get <entity-key>
thread-keep --json note add <entity-key> --kind <kind> --body <body> --origin agent
thread-keep --json note revise <note-id> --body <body> --origin agent
thread-keep --json diff
```

JSON success is written to stdout as `{"version":1,"data":...}`. JSON errors are written to stderr as `{"version":1,"error":{"code":...,"message":...}}`.

Important error codes:

- `not_initialized`: run `thread-keep init`, then update a clean committed worktree.
- `repository_state`: worktree is not a supported Git state; `update` also requires a clean worktree.
- `stale_working_set`: run `thread-keep update` before adding context after source or branch changes.
- `entity_not_found`: search/update before selecting an entity key.
- `validation` from `note revise`: only a committed note can receive a successor revision; preserve and report an existing pending note instead of duplicating it.
- `working_set_dirty` or `concurrent_update`: preserve and show pending state; do not commit or overwrite it.
