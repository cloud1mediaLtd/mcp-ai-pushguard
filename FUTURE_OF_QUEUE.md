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

### Phase 4: Local Dashboard & Visual Queue
- [ ] Embedded web interface on `localhost:8942` displaying real-time queue status, CPU metrics, and streaming logs.
- [ ] Interactive task cancellation, log downloading, and rerun triggers.

### Phase 5: Remote Cloud Bursting
- [ ] Remote worker agent daemon installable on remote servers.
- [ ] Secure SSH/mTLS tunneling for offloading heavy jobs to cloud infrastructure.

---

## 5. Summary

PushGuard provides the rock-solid, production-tested foundation for pre-push tests. By preserving this foundation and methodically layering execution modes and resource pools, we are building the definitive **local async compute engine for the AI coding era**.
