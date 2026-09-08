---
name: pushguard
description: Queue expensive local verification in isolated Git worktrees and collect its result through the PushGuard CLI or MCP server.
---

# PushGuard

Use for expensive local checks when work can continue while they run. Keep
focused, fast checks local during editing.

## Submit and collect

1. Commit the coherent change, then submit an explicit SHA when it matters
   which commit is verified. Default enqueue resolves committed HEAD; it does
   not include dirty files. In MML use `make server-pre-push-q`.
2. Enqueue returns a job ID. Continue useful work and attach a tracked
   background `pushguard await <job-id> --timeout 1500` process so its exit
   brings the agent back to collect the verdict. A macOS notification reaches
   the human, not the agent. Do not build status-polling loops.
3. Confirm source kind, tested SHA, command and verdict before relying on it.
   Later edits require a new gate. CLI codes: `0` passed (or submission accepted),
   `1` failed, `2` wait expired (job continues), `3` cancelled/interrupted without
   a completed verdict, `4` submission rejected. An accepted job still needs
   its verdict collected. Never rerun a failed accepted job merely because
   enqueue returned nonzero.

For unfinished work explicitly request `--snapshot` or MCP `snapshot: true`.
This snapshots tracked and untracked nonignored files using a scratch index;
it does not change the caller's index. It is mutually exclusive with `ref`.
Snapshot errors fail submission. A snapshot result does not verify its parent
commit.

```bash
pushguard enqueue --repo . --ref HEAD --cmd "go test ./..."
pushguard await pg_example --timeout 1500
pushguard status pg_example
pushguard status # daemon build, agreement and queue state
pushguard list --ref <committed-sha> --limit 10 # avoid duplicating an existing gate
```

Explicit command strings run as shell commands, preserving quoting. No detected
check is an error. Execution defaults to 900 seconds (`--timeout` / MCP
`timeout_seconds`). `enqueue --wait --wait-timeout 1500` waits synchronously;
use this when the result blocks the next step.

## Lifecycle and isolation

The shared daemon auto-starts and defaults to one worker, shuts down after 30
idle minutes, and prunes old disk artifacts hourly even when busy. Active jobs
are retained independently of the completed-history limit. Completed records
and logs have 48-hour retention. `clean` clears memory history separately.

An incompatible daemon build or protocol rejects new submissions: finish its
jobs before an idle upgrade. `pushguard status` with no ID reports the daemon
build, PID, uptime, queue depth, concurrency, and client/daemon agreement.
Never rebuild, replace or stop a busy installed daemon. Read-only status and
stored results remain available across protocol upgrades.

If auto-start fails, CLI fallback runs synchronously to completion in private
artifact directories; MCP fallback runs in its own process. This is degraded,
per-session concurrency, not the shared queue. Its jobs cannot outlive that
process. Collect MCP fallback jobs with that session's MCP await/status tools;
a separate CLI cannot see them. `PUSHGUARD_STATE_DIR` isolates daemon sockets,
records, logs and worktrees for tests.

Worktrees isolate files, not databases, ports, sibling repos or environment.
Use repository-owned isolation arguments. MML's wrapper derives a database
suffix and a widely spread smoke-port slot from `PUSHGUARD_JOB_ID`; ports are
still not reserved, so leave concurrency at one unless the gate itself can
coordinate its external resources.

## Tools and authority

MCP tools: `pushguard_enqueue`, `pushguard_status`, `pushguard_list`,
`pushguard_cancel`, `pushguard_await`, `pushguard_clean`. Enqueue accepts
`repo_path`, `ref` or `snapshot`, `command`, `name`, and `timeout_seconds`.
Await accepts `job_id` and `timeout_seconds` (default 1500). Cancellation
requests stop; await the terminal result before assuming resources are free.
Pass `ref` to `pushguard_list` before enqueueing to find a matching tested SHA
or requested ref. A waiting agent must keep `pushguard_await` active through
its own background-process facility; the daemon cannot resume an unrelated
Codex or Claude session directly.

Use `dedupe_key` (MCP) or `--dedupe-key` (CLI) when a shared gate command has
per-run database or port values. PushGuard reuses only active jobs with the
same key; completed gates remain discoverable through `list --ref`, and
snapshots always run independently.

Auto-push and asynchronous hook installation are disabled. Verification never
implies permission to push, run remote CI or deploy. Apply the workspace's
separate authorization rules for those actions.
