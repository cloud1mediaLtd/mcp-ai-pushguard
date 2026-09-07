# AI MCP PushGuard 🛡️

> **Asynchronous, worktree-isolated local CI & pre-push queue with native MCP support for AI coding agents (Claude Code, ChatGPT, Antigravity, Cursor) and developers.**

---

## ⚡ The Problem

When AI coding assistants (Claude Code, ChatGPT/Codex, Antigravity, Cursor) or developers run pre-push hooks or heavy test suites:

1. **Context & Turn Blocking**: The agent sits waiting on stdout, burning conversation turns, hitting MCP timeouts (typically 60s–300s), or freezing your terminal.
2. **Working Tree Lockup**: You and the agent cannot edit files, switch git branches, or begin the next feature while tests are running against the current tree.
3. **Hardware Thrashing**: When multiple agent sessions or terminals trigger test suites simultaneously, your laptop CPU and RAM spike, grinding everything to a halt.

---

## 💡 The Solution

**PushGuard** offloads all testing into a managed background queue running inside isolated **Git Worktrees**:

```
┌─────────────────┐       ┌─────────────────┐       ┌─────────────────┐
│   Claude Code   │       │ ChatGPT / Codex │       │ Terminal / Dev  │
└────────┬────────┘       └────────┬────────┘       └────────┬────────┘
         │ (MCP)                   │ (MCP)                   │ (CLI)
         └────────────────┬────────┴─────────────────────────┘
                          ▼
            ┌───────────────────────────┐
            │     PushGuard Daemon      │  (~/.pushguard/pushguard.sock)
            │  - Concurrency throttle   │
            │  - Shared global queue    │
            └─────────────┬─────────────┘
                          ▼
            ┌───────────────────────────┐
            │ Isolated Git Worktree     │  (~/.cache/pushguard/worktrees)
            │  - Clean checkout         │
            │  - Main workspace FREE    │
            └─────────────┬─────────────┘
                          ▼
                 ┌─────────────────┐
                 │    Ping Back    │
                 │ - macOS Alert   │
                 │ - Sound effect  │
                 │ - MCP status    │
                 │ - Auto-push     │
                 └─────────────────┘
```

1. **Instant Return (Under 100ms)**: The agent or hook enqueues the job and gets back a job ID immediately. The conversation never freezes.
2. **Clean Working Tree**: PushGuard spins up a detached git worktree. Your main working directory remains 100% free to edit, branch, or stage changes.
3. **Hardware Protection**: Bounded concurrency (default: 1 or 2 workers) guarantees tests run sequentially without melting your CPU.
4. **Local Ping-Back**: Native macOS notification banners and sound effects (`Glass` on pass, `Basso` on fail) alert you when tests complete.
5. **Auto-Push (Optional)**: If `--auto-push` is set, PushGuard pushes directly to your remote branch upon a green run.

---

## 🚦 Do I Need to Keep It Running?

**Short answer:** No — you never have to start or stop it yourself. Both the MCP server and the CLI's `enqueue` command auto-start the shared daemon on demand, and the daemon self-terminates once it's been idle for a while. This is deliberate: on a personal dev machine, nothing should be a permanent resident consuming memory/CPU when nobody's using it, but also nothing should require you to remember a manual step.

### The lifecycle, precisely
1. **On demand, from any caller.** The first `pushguard_enqueue` MCP call, or the first `pushguard enqueue` CLI invocation, that finds no daemon on `~/.pushguard/pushguard.sock` spawns `pushguard daemon` itself — detached into its own session (so it outlives the process that spawned it), logging to `~/.pushguard/daemon.log`. You do not need `make pushguard-start` or a manual `pushguard daemon &` anymore; those still work, they're just no longer required.
2. **One shared queue, always.** Because every caller auto-starts (or finds) the *same* daemon, all your terminal tabs, Claude Code sessions, and git hooks land on one coordinated queue with one concurrency throttle — there is no more silent per-session "embedded engine" fork happening behind your back in the common case.
3. **Self-shutdown when idle.** The daemon tracks when it last had a job queued or finish. With zero active/queued jobs and nothing happening for `--idle-shutdown-minutes` (default **30**; `0` disables it), it runs the same sweep as `pushguard clean` — flush completed jobs, prune stale worktrees, delete old logs, force GC — logs what it cleaned, and exits cleanly, removing its own socket file. The next enqueue simply respawns it.
4. **Graceful degradation, not a crash.** If a caller (MCP server or CLI) can't auto-start the daemon for some reason (binary not locatable, spawn failure), or the daemon disappears mid-session (idle-shutdown, or someone killing it), that caller falls back to a private in-process engine instead of erroring. That fallback engine's job history and concurrency limit are local to that one process — not shared with the daemon or any other session's fallback — so it's a degraded mode, not the common path.
5. **Auto-spawn never fires from a test binary.** `go test` runs are guarded off (and can be forced off anywhere with `PUSHGUARD_NO_AUTOSPAWN=1`), so running this repo's test suite on any machine can never leave behind a stray daemon process.

**Net effect:** the daemon exists only while there's actual work happening (plus a short idle tail), self-heals if killed, and never accumulates state indefinitely.

---

## 🚀 Installation & Setup

### 1. Build the Binary

```bash
git clone https://github.com/cloud1mediaLtd/mcp-ai-pushguard.git
cd mcp-ai-pushguard
make install
```
*(Installs `pushguard` into `$GOPATH/bin` or `/usr/local/bin`)*

Or build locally:
```bash
go build -o pushguard ./cmd/pushguard
```

---

## 🤖 Configuring for AI Coding Agents (MCP)

PushGuard provides a built-in **Model Context Protocol (MCP)** server over stdio (`pushguard mcp`).

### 1. Claude Code
Add to `~/.claude/mcp.json` or `.mcp.json` in your project root:

```json
{
  "mcpServers": {
    "pushguard": {
      "command": "pushguard",
      "args": ["mcp"]
    }
  }
}
```

Or via the Claude Code CLI:
```bash
claude mcp add pushguard -- pushguard mcp
```

### 2. ChatGPT / Codex / Cursor / Antigravity
Add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "pushguard": {
      "command": "pushguard",
      "args": ["mcp"]
    }
  }
}
```

### Available MCP Tools for Agents

* **`pushguard_enqueue`**: Enqueues tests in an isolated worktree. Returns immediately with `job_id` and position. Working tree remains unlocked.
* **`pushguard_status`**: Checks status (`queued`, `running`, `passed`, `failed`), duration, and log tail.
* **`pushguard_list`**: Views active and recent jobs.
* **`pushguard_cancel`**: Aborts a pending or active job and tears down its worktree.
* **`pushguard_await`**: (Optional) Waits synchronously up to a timeout if the agent explicitly needs the test outcome before proceeding.
* **`pushguard_clean`**: Reclaims memory, purges completed jobs, prunes stale worktrees, and runs garbage collection.

---

## 🧠 Memory Management & Queue Hygiene

PushGuard bounds its own in-memory footprint, and — thanks to the idle-shutdown lifecycle above — also cleans up after itself without you doing anything, as long as the daemon gets a chance to go idle between bursts of work:

1. **Log Truncation**: In-memory logs are capped at 2,000 lines per job. Full logs are written to disk (`~/.cache/pushguard/logs/<job_id>.log`) and are deleted once they're older than 48 hours — automatically, the next time the daemon idle-shuts-down (or whenever you run `clean` manually).
2. **Circular Job History**: In-memory job store defaults to a 100-job circular capacity — the 101st job evicts the oldest even before an idle-shutdown clean runs.
3. **Worktrees**: torn down automatically when a job finishes normally; anything orphaned by a killed daemon or cancelled job gets pruned (if older than 1 hour) on the next idle-shutdown or manual clean.
4. **On-Demand Cleanup** — you don't have to wait for the idle timer if you want it now:
   ```bash
   pushguard clean
   ```
   Or via the agent tool `pushguard_clean`. Both do exactly what the idle-shutdown sweep does: flush completed jobs from memory, prune worktrees older than 1 hour, delete logs older than 48 hours, force GC.

   The one case this doesn't cover: a daemon kept continuously busy (or with `--idle-shutdown-minutes 0`) never idles, so it never runs this sweep on its own — run `pushguard clean` by hand in that scenario.

---

## 📈 Upgrade Path: What To Do When Things Slow Down

PushGuard's defaults (1 worker, in-memory + on-disk job history, no dashboard) are tuned for how it's actually used today: a handful of repos, dozens of jobs a day, one machine. None of that is a hard ceiling — here's the concrete symptom → fix mapping for when it is, so upgrading is a lookup, not a redesign.

| Symptom | What's actually happening | Fix |
|---|---|---|
| Jobs sit `queued` for a while even though nothing else looks busy | `--concurrency 1` means only one job runs at a time, by design, so the machine never thrashes | Raise `--concurrency` — but first make sure your test command itself is safe to run concurrently (a unique test-DB name, unique ports, etc. per invocation) if two concurrent jobs could touch the same shared resource; concurrency without that isolation just moves the collision from "queue" to "runtime" |
| `~/.pushguard/jobs/*.json` (the `DurableStore` restart-recovery files) keeps growing into the thousands | Every `Save`/`Update` writes a file there, but only a full `Clean()` — from idle-shutdown or a manual `pushguard clean` — actually deletes them. The hourly disk-prune ticker (`PruneDiskArtifacts`) only touches worktrees/logs, not this directory. A daemon kept continuously busy all day (never idle, per the row above about `--idle-shutdown-minutes 0`) accumulates these indefinitely | Run `pushguard clean` on a schedule (cron/launchd) if the daemon realistically never idles on your workflow, until `PruneDiskArtifacts` is extended to also age out this directory independent of the full `Clean()` |
| Checking `pushguard_list`/`pushguard_status` by hand is starting to feel like the bottleneck itself | This is exactly the point where polling stops being appropriate | Build Phase 4's local web dashboard (see below) — the Redis `pushguard:events` channel already exists for this, only the browser-facing side is missing |
| You want job history readable from a second machine, or by a process that shouldn't share `~/.pushguard/jobs/` | `DurableStore` is local-disk-only, deliberately — it's what's actually wired in today | Swap in a `RedisStore` implementing `StoreInterface` (`internal/core/engine.go`) — the seam already exists, this is additive, not a rewrite |
| You're using PushGuard for something other than tests (a migration, a scaffold, a long dev-server) and the "isolated worktree" default is wrong for it | Every job today runs in `mode: worktree`. Some tasks need to run in place (`in-situ`) or as a supervised long-lived process (`service`), not a detached checkout | This is the `mcpq` generalization in the roadmap below — multi-mode execution, not something to bolt on ad hoc |
| Other repos you work in have slow local gates too, and agents keep blocking on them the same way your first repo used to | PushGuard itself is already repo-agnostic — the auto-detector and `--repo`/`--cmd` work against any git repository, any language | Wire up the same "queue the gate instead of blocking on it" pattern (a Makefile target and/or AGENTS.md-style guidance) for those repos too, once their gates are confirmed slow enough to be worth queuing |

---

## 🔧 Git Pre-Push Hook Integration

Install PushGuard into any local git repository with one command:

```bash
cd /path/to/my-repo
pushguard hook install
```

### Async Pre-Push (Auto-Push on Pass):
```bash
pushguard hook install --async
```
When you run `git push`, the hook immediately delegates to PushGuard and aborts the blocking terminal push. Once background tests pass in the worktree, PushGuard pushes directly to remote and alerts you via desktop notification!

---

## 📡 Redis Job Events (Optional)

If Redis is reachable at daemon startup (default `127.0.0.1:6379`; override with `--redis-addr` or `PUSHGUARD_REDIS_ADDR`, empty disables it), the daemon publishes every finished job's full JSON to the `pushguard:events` channel:

```bash
redis-cli subscribe pushguard:events
```

This is entirely optional and additive — nothing about enqueueing, running, or querying jobs depends on Redis. If it isn't reachable at startup, publishing is skipped (logged once, not retried) and everything else works exactly as it does today. It exists as the wire format a future local web dashboard (or any other tool) can subscribe to for live updates without polling the daemon's HTTP API.

---

## 💻 CLI Command Reference

```bash
# Enqueue tests for current repo (auto-detects Go, Node/pnpm/npm, Cargo, Makefile)
pushguard enqueue

# Enqueue with custom command and auto-push
pushguard enqueue --repo ./backend --cmd "go test -v ./..." --auto-push

# Check job status & recent logs
pushguard status pg_a1b2c3d4

# List queue and recent job history
pushguard list

# Cancel a job
pushguard cancel pg_a1b2c3d4

# Clean queue and reclaim memory
pushguard clean

# Run background daemon with 2 concurrent workers
pushguard daemon --concurrency 2

# Install git pre-push hook
pushguard hook install --repo .
```

---

## 🗺️ The Future: Expanding to `mcpq`

PushGuard was built to solve the pre-push test blocking problem, but its underlying engine is designed to evolve into a general-purpose **`mcpq` (Model Context Protocol Queue)** for offloading heavy compiles, database migrations, and background agents.

Read our design vision in [FUTURE_OF_QUEUE.md](FUTURE_OF_QUEUE.md).

---

## 🤝 Contributing & License

PushGuard is open source under the [MIT License](LICENSE).
