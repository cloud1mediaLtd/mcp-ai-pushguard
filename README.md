# PushGuard

**Status:** Active development — protocol 2
**Owner:** Platform
**Last reviewed:** 2026-09-08

PushGuard queues expensive local checks in isolated Git worktrees. A shared
daemon limits concurrency while developers and coding agents continue working.
Every job reports its source, tested commit, command and completed verdict.

## Submit a check

```bash
pushguard enqueue --repo . --ref HEAD --cmd "go test ./..."
pushguard await pg_example --timeout 1500
pushguard status pg_example
pushguard status # daemon/client agreement and queue state
pushguard list --ref <committed-sha> --limit 10
```

Default enqueue resolves committed HEAD when submitted. Branch movement and
later working-tree edits cannot change that job. `--snapshot` explicitly tests
tracked and nonignored untracked changes through a scratch index, preserving
the caller's index. It cannot be combined with `--ref`. Snapshot failures are
errors; a snapshot result does not verify its parent commit.

Explicit `--cmd` strings run as shell commands with quoting preserved. Without
one, PushGuard detects a check; no detected command is an error. There is no
successful placeholder check.

Enqueue normally returns immediately after acceptance. Start the printed await
command as a tracked background process, continue work, and collect the verdict
when it completes. Desktop notifications reach a human, not an agent. Use
`enqueue --wait --wait-timeout 1500` when the result blocks your next step.
Execution defaults to 900 seconds (`--timeout`); waiting defaults to 1500 seconds
and includes queue time. Expiring a wait does not cancel the daemon job.

| CLI exit | Meaning |
|---|---|
| 0 | Passed, or asynchronous submission accepted (still collect its verdict) |
| 1 | Failed check or command error |
| 2 | Wait expired; the daemon job continues |
| 3 | Cancelled/interrupted without a completed verdict |
| 4 | Submission rejected before acceptance |

The underlying command exit code is reported separately. Do not automatically
rerun a failed accepted job as a submission fallback.

`pushguard status` with no job ID reports daemon build, PID, uptime, queue
depth, active jobs, configured concurrency, and client/daemon agreement. A
build or protocol mismatch blocks enqueue before acceptance. Use `list --ref`
to find a queued or completed verification for an exact tested SHA or requested
ref before submitting duplicate work. Job logs include the daemon build that
executed them.

Active jobs with the same verification identity are reused instead of creating
another worktree. The default identity is repository, immutable SHA, and exact
command. Use `--dedupe-key <key>` when commands contain per-run isolation
values but represent the same gate; snapshots are never deduplicated, and a
completed job is reported by `list --ref` rather than silently reused.

## Daemon and artifacts

The daemon auto-starts, defaults to one worker, and shuts down after 30 idle
minutes. An exclusive ownership lock protects shared recovery and cleanup;
startup also refuses a live legacy socket. Hourly cleanup prunes old disk
artifacts even while the daemon stays busy. The shared Go build cache is
reclaimed while idle if it reaches 20 GiB or free disk falls below 20 GiB; the
same check runs before idle shutdown. Active jobs are never evicted by
completed-history limits. Completed records and logs have 48-hour retention.

Default locations:

- Socket, lock and job records: `~/.pushguard/`.
- Worktrees and logs: `~/.cache/pushguard/`.
- `PUSHGUARD_STATE_DIR`: optional private directory containing socket, lock,
  jobs, worktrees and logs, useful for isolated tests.

Auto-start failure uses a private fallback engine. CLI fallback runs to
completion synchronously; MCP fallback jobs belong to that MCP process and
must be collected through its tools. A separate CLI cannot see those jobs.
Fallback engines do not share the daemon's concurrency limit or durable history.

Worktrees isolate files, not databases, ports, sibling repositories or other
external dependencies. Commands can use `PUSHGUARD_JOB_ID` and
`PUSHGUARD_ORIGIN_REPO` to coordinate repository-owned checks. The workspace
backend wrapper derives its database suffix and widely spread smoke-port slot
from `PUSHGUARD_JOB_ID`; the ports are not reserved, so the default concurrency
remains one.

Cancellation stops the job's Unix process group; inherited output waits are
bounded. Raw output streams to disk, with at most 2000 chunks of at most 64 KiB
in the memory tail. Output and cleanup errors remain visible.

## MCP

Configure the executable as a stdio server:

```json
{"mcpServers":{"pushguard":{"command":"pushguard","args":["mcp"]}}}
```

Tools: `pushguard_enqueue`, `pushguard_status`, `pushguard_list`,
`pushguard_cancel`, `pushguard_await`, `pushguard_clean`.

Enqueue accepts `repo_path`, `ref` or `snapshot`, `command`, `name`, and integer
`timeout_seconds`. Await accepts `job_id` and `timeout_seconds`. Cancelling a
running job requests its termination; await the terminal result before assuming
resources are released. Embedded jobs remain routed to their original engine
when a shared daemon becomes reachable.

## Hooks and remote actions

```bash
pushguard hook install --repo .
```

Synchronous hooks verify the actual pushed SHAs, peel annotated tags,
deduplicate commits and skip deletions. Existing unrelated or legacy async
hooks are preserved. `core.hooksPath` and linked worktrees are supported.

Auto-push and asynchronous hook installation are disabled. `--auto-push`,
`auto_push: true` and `hook install --async` fail explicitly. A green job never
pushes, dispatches CI or deploys. Those actions require separate authorization.

## Build, validate and upgrade

```bash
make build
go test -race ./...
go vet ./...
```

These build and test the checkout; `make install` explicitly replaces the
installed CLI by atomic rename. Check the queue before any upgrade and never
replace or restart a busy installed daemon. A running daemon retains its old
binary. New clients refuse incompatible submission protocols or builds; reads
of retained old results remain supported. Health exposes protocol, build, PID,
uptime and queue state.
Protocol 2 uses integer seconds for request timeouts and explicit snapshots;
legacy clients cannot submit old timeout units to it.

Tests use private repositories, sockets and artifacts. They do not enqueue
against the installed daemon. `PUSHGUARD_NO_AUTOSPAWN=1` prevents automatic
startup in standalone test contexts.

The central [operating runbook and audit](../docs/content/docs/ops/pushguard.mdx)
records workflow details and deferred work. The broader, unimplemented
expansion roadmap is in [FUTURE_OF_QUEUE.md](FUTURE_OF_QUEUE.md).

Licensed under the [MIT License](LICENSE).
