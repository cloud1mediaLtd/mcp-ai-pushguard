# The Future of PushGuard: Expanding into `mcpq` 🚀

> **Vision & Architectural Roadmap for an Asynchronous Background Task Queue for AI Coding Agents and Developers.**

---

## 1. Executive Summary & The Core Thesis

Today's AI coding assistants (Claude Code, ChatGPT/Codex, Antigravity, Cursor, Cline) are fundamentally **synchronous and blocking**. When an agent runs a command or calls an MCP tool, the entire conversational context is pinned waiting on standard output.

If a task takes longer than 15–30 seconds, three critical failures occur:
1. **Context/Turn Freezing**: The developer and agent cannot converse, inspect files, or take action until the task finishes.
2. **Connection Timeouts**: MCP clients drop tool calls after fixed timeouts (typically 60s–300s).
3. **Hardware Contention**: Parallel agent turns or multiple terminal tabs launch resource-intensive tasks simultaneously, degrading machine performance.

**PushGuard** began by solving this specifically for pre-push git tests using isolated Git worktrees. However, the exact same primitive—**an asynchronous, throttled, local background queue with native MCP interfaces**—is required across all heavy developer workflows.

This document outlines the architectural roadmap for evolving PushGuard into **`mcpq`** (Model Context Protocol Queue).

---

## 2. The Four Pillars of `mcpq`

```
                ┌─────────────────────────────────────────────────┐
                │          AI Agents & Developer Clients          │
                │     Claude Code • ChatGPT • Antigravity • CLI    │
                └────────────────────────┬────────────────────────┘
                                         │ JSON-RPC (MCP) / Unix Socket
                                         ▼
                ┌─────────────────────────────────────────────────┐
                │                   mcpq Daemon                   │
                │  - Bounded Concurrency Pools (CPU / IO / Mutex) │
                │  - Memory & Log Ring Buffers                    │
                │  - Process Lifecycle & Auto-Kill TTLs           │
                └───────┬───────────────┬─────────────────┬───────┘
                        │               │                 │
           ┌────────────▼─────┐  ┌──────▼──────┐  ┌───────▼───────────┐
           │ Worktree Runner  │  │ In-Situ     │  │ Ephemeral Service │
           │ - Isolated tests │  │ - Scaffolds │  │ - Mock APIs       │
           │ - Clean builds   │  │ - Migrates  │  │ - Dev servers     │
           └──────────────────┘  └─────────────┘  └───────────────────┘
```

### Pillar 1: Multi-Tier Execution Modes
Different developer tasks require different isolation levels:

* **Mode `worktree`**: Detached git worktrees. Leaves the user's working directory completely unlocked. Ideal for test suites, pre-pushes, and static binary builds.
* **Mode `in-situ`**: Runs directly inside the target repository. Ideal for database migrations, codemods, scaffolding, or dependency installations that *should* modify the working tree.
* **Mode `sandbox`**: Lightweight container or Firecracker / Apple Virtualization sandboxes for untrusted scripts, heavy pip/npm installations, or foreign toolchains.
* **Mode `service`**: Long-lived background processes (e.g. mock backend servers, local proxies, live reload watchers) governed by an automatic time-to-live (TTL) to prevent process leaks.

### Pillar 2: Named Concurrency Pools
Laptops must not freeze. `mcpq` will enforce segregated resource pools:

* **`cpu` Pool**: Capped at 1 or 2 concurrent workers (compilations, test suites, video/asset rendering).
* **`io` Pool**: Capped at 5–10 concurrent workers (bulk git fetches, API downloads, web crawlers, document indexers).
* **`mutex` Pool**: Strictly serialized (1 worker globally across all agents) for stateful operations like database migrations or port allocations.

### Pillar 3: Bidirectional Events & Ping-Backs
Currently, agents can poll via `pushguard_status` or receive local macOS notifications. Future versions will support:
* **MCP Server-Sent Events (SSE)**: Pushing completion events directly back into active agent conversations.
* **Local Webhooks**: Triggering external HTTP endpoints or orchestrator bots upon completion.
* **Rich Native UI Alerts**: macOS Actionable Banners with click-to-view logs and terminal focus.

### Pillar 4: Hybrid Cloud Bursting (Distributed Offload)
When a developer is traveling on battery power or working on a lightweight laptop:
* `mcpq` can seamlessly forward heavy jobs (e.g., large Docker builds, full mono-repo test suites) to a remote server or Hetzner VPS over SSH/WireGuard, streaming the results back locally.

---

## 3. Generalized Agent Toolset (`mcpq_*`)

In the expanded architecture, PushGuard becomes a specialized preset within the broader toolset:

```json
// Example: Offloading a heavy compilation or build
{
  "name": "mcpq_submit",
  "arguments": {
    "command": "pnpm build",
    "working_dir": "./saas",
    "mode": "worktree",
    "pool": "cpu",
    "timeout_seconds": 900,
    "tags": ["build", "saas-dash"],
    "notify": true
  }
}
```

### Universal Tool Suite:
1. **`mcpq_submit`**: Submits any shell command, build script, or preset with customizable isolation, pool, and timeout.
2. **`mcpq_status`**: Queries status (`queued`, `running`, `passed`, `failed`), duration, exit code, and live log tail.
3. **`mcpq_logs`**: Streams or retrieves full paginated output logs.
4. **`mcpq_list`**: Filters active and completed tasks by tag, status, or repository.
5. **`mcpq_cancel`**: Gracefully terminates running processes and unlinks allocated worktrees.
6. **`mcpq_clean`**: Flushes finished tasks, frees heap memory, and prunes stale resources.
7. **`mcpq_prepush`** *(PushGuard Preset)*: Retains the instant pre-push hook and auto-push workflow.

---

## 4. Phased Implementation Roadmap

### Phase 1: PushGuard Core (Current Milestone) ✅
- [x] Detached Git worktree manager with automated unlinking.
- [x] In-memory job queue with bounded worker pool.
- [x] Zero-dependency pure Go standard library implementation.
- [x] Stdout/stderr streaming to on-disk logs and memory buffers.
- [x] Desktop notifications via AppleScript (`osascript`) with audio chimes.
- [x] Git pre-push hook auto-installer with sync and async modes.
- [x] Complete JSON-RPC 2.0 stdio MCP server for Claude Code, ChatGPT, and Antigravity.
- [x] Memory management, job eviction, stale worktree pruning, and runtime garbage collection.

### Phase 2: Generalized Task Engine (Next)
- [ ] Implement `mcpq_submit` supporting `worktree` vs `in-situ` execution modes.
- [ ] Add named concurrency pools (`cpu`, `io`, `mutex`).
- [ ] Support priority task queues (e.g., critical user tests jumping ahead of background indexers).
- [ ] Add configurable task tags and metadata.

### Phase 3: Ephemeral Services & Long-Running Daemons
- [ ] Support background mock servers and dev servers with auto-kill TTLs.
- [ ] Port allocation manager to prevent port collisions between simultaneous runs.
- [ ] Healthcheck probes (`http_get`, `tcp_ping`) for validating when a background service is ready.

### Phase 3.5: Richer Notifications (small, do before the dashboard)
- [x] **Click-to-open-logs, and grouping by repo — shipped** (`internal/notifier/notifier.go`). Root cause of the old "clicking a notification opens an empty folder" behavior: plain AppleScript's `display notification` has no click action at all — that's a hard limitation of the mechanism, not a bug that was fixable in place. Fix: prefer `terminal-notifier` (`brew install terminal-notifier`) when installed — its `-open file://<path>` opens the job's actual log file on click, and `-group pushguard-<repo>` collapses repeated notifications from the same repo into one instead of stacking up individually across dozens of runs a day. Falls back to the old plain banner (no click action, same as before) if `terminal-notifier` isn't installed — never worse than before, just not better without it.
- [ ] "Rerun" as a second notification action (re-enqueues the same job) — not yet built; `terminal-notifier` supports multiple actions via `-actions`, this just needs a handler.
- [ ] Once Phase 4's dashboard exists, "View Logs" could open the job's page there instead of a flat file.

### Phase 4: Local Web Dashboard & Visual Queue
The daemon already serves HTTP (`--http 127.0.0.1:PORT`, currently opt-in for the JSON API) — the dashboard is a static page + a couple of new endpoints on that same server, not a new service:
- [ ] **Serve it from the daemon itself**, embedded via `go:embed` as a single static bundle (plain HTML/CSS/JS, no Node build step) — keeps the "zero-dependency pure Go" property intact. Bind to `127.0.0.1` only; this is a personal-machine tool, never expose it on `0.0.0.0` or the LAN without adding real auth first.
- [x] **Job events now publish to Redis** (`internal/notifier/redis_notifier.go`) — every finished job's full JSON is `PUBLISH`ed to `pushguard:events` on `127.0.0.1:6379` (`--redis-addr` flag / `PUSHGUARD_REDIS_ADDR` env var to change; empty disables). Verified live: `redis-cli subscribe pushguard:events` while a job runs shows the event the instant it finishes. Connects once at daemon startup with a 2s timeout — if Redis isn't reachable, publishing is skipped with a log line, never blocks daemon startup or job execution. The workspace's Redis was standardized to one `redis8` container (Redis 8.10.1, port 6379, named volume, `--restart unless-stopped`) as part of adding this.
- [ ] **Live queue view**: an `/api/v1/events` Server-Sent-Events endpoint that itself just `SUBSCRIBE`s to `pushguard:events` and forwards to the browser — the daemon→Redis leg above is done, this is only the Redis→browser leg. No new engine plumbing needed, decoupled from job execution already.
- [ ] **Per-job page**: streaming log tail (tail the same file the CLI/MCP tools already read), status, duration, exit code, cancel/rerun buttons calling the existing `/api/v1/jobs/{id}/cancel` and a new rerun endpoint.
- [ ] **Daemon health panel**: concurrency setting, current idle duration vs. the idle-shutdown threshold ("shuts down in ~12m if idle"), in-memory job count vs. the 100-job ring buffer cap, last clean-sweep results — makes the whole lifecycle this session just built legible instead of log-file archaeology.
- [ ] **Idle-shutdown must treat an open dashboard as activity.** Today idle-shutdown only looks at `ActiveJobCount()` — a human staring at the dashboard with zero jobs running would otherwise get the daemon pulled out from under them. Simplest fix: have the SSE handler call the same `touchActivity()` the engine uses internally whenever a client is connected (or on each heartbeat), so an open dashboard tab keeps the timer from expiring.
- [x] **Job history surviving a restart — solved, without Redis.** `internal/store/durable.go`'s `DurableStore` wraps `MemoryStore` and mirrors every job to `~/.pushguard/jobs/<id>.json` (write-then-rename, so a reader never sees a half-written record); `RecoverOrphans()` runs once at daemon startup and turns any job disk still says was queued/running into an explicit "daemon restarted, re-run it" cancellation instead of a confusing "job not found". This is what the daemon actually uses today — no Redis dependency needed for this particular problem after all.
  - Redis remains a reasonable *alternative* backend later if history ever needs to be readable from a second machine or a separate process that shouldn't share `~/.pushguard/jobs/` — `StoreInterface` (`internal/core/engine.go`, four methods: `Save`/`Get`/`List`/`Update`/`Clear`) is the seam for a `RedisStore` drop-in if that need materializes. Not needed to build the dashboard; `DurableStore` already covers it.
  - Worth deciding explicitly rather than defaulting to SQLite/JSONL as this doc originally suggested: Redis only helps if it's something this machine can rely on being up when PushGuard needs it (if Redis itself needs to "be running" as a prerequisite, that's another moving part to keep alive) — a local SQLite file has no such dependency. Pick based on whether Redis is already a reliably-running fixture of this dev setup, not just "available."

### Phase 5: Remote Cloud Bursting
- [ ] Remote worker agent daemon installable on remote servers.
- [ ] Secure SSH/mTLS tunneling for offloading heavy jobs to cloud infrastructure.

---

## 5. Summary

PushGuard provides the rock-solid, production-tested foundation for pre-push tests. By preserving this foundation and methodically layering execution modes and resource pools, we are building the definitive **local async compute engine for the AI coding era**.
