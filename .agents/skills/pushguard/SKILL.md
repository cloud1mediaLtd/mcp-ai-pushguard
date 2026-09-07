---
name: pushguard
description: "Asynchronous, worktree-isolated local CI & pre-push queue with MCP support for AI coding assistants (Claude Code, ChatGPT, Antigravity, Cursor) and developers. Use when running tests, pre-pushes, or heavy verification so the agent does not freeze its context or lock the working directory. Covers daemon startup, MCP tools, CLI usage, queue management, and memory cleanup."
---

# PushGuard Skill — Async Local CI & Pre-Push Queue

PushGuard is an asynchronous local test queue and background runner designed specifically to keep AI coding agents (Claude Code, ChatGPT/Codex, Antigravity, Cursor) fast, responsive, and unblocked.

Instead of running slow tests synchronously in the working tree (which burns turns, risks tool timeouts, and locks files), PushGuard offloads the run to an isolated **Git Worktree** in the background and pings back locally.

---

## When to Use PushGuard

Trigger this skill whenever:
1. **Running tests before git commit/push**: Avoid executing long test commands (`go test ./...`, `pnpm test`, `cargo test`) synchronously in the main directory.
2. **Pre-push hooks take too long**: Prevent git push hooks from timing out agent sessions.
3. **Multitasking while tests run**: The user or agent wants to continue editing code or working on other files without dirty files contaminating the test run.
4. **Preventing machine thrashing**: You need tests to queue up sequentially (or capped at 1–2 concurrent runs) rather than spiking laptop CPU/RAM.

---

## Architecture & Modes

PushGuard can operate in two modes:

### 1. Daemon Mode (Recommended for Multi-Agent & Terminal Coordination)
A single background daemon runs on the machine:
```bash
pushguard daemon --concurrency 1
```
* **Shared Queue**: All terminal tabs, Claude Code instances, and git hooks communicate over `~/.pushguard/pushguard.sock`.
* **Bounded Concurrency**: Limits concurrent heavy test runs (default: 1 worker) to protect laptop battery and CPU.

### 2. Standalone / Embedded Mode (Zero-Config)
If the daemon is NOT running, running `pushguard mcp` or `pushguard enqueue` automatically boots an embedded, in-process engine. No manual daemon startup required.

---

## Agent MCP Tools Reference

When PushGuard is configured as an MCP server, agents have access to:

| Tool | Purpose | Key Arguments |
| :--- | :--- | :--- |
| `pushguard_enqueue` | Schedules test run in an isolated worktree. Returns immediately with `job_id` and position. | `repo_path` (string), `ref` (string, default: HEAD/dirty state), `command` (string, optional), `auto_push` (bool) |
| `pushguard_status` | Checks progress, status (`queued`, `running`, `passed`, `failed`), duration, and log tail. | `job_id` (string, required), `log_lines` (int, default: 50) |
| `pushguard_list` | Lists all active, queued, and recently finished jobs. | `limit` (int, default: 10) |
| `pushguard_cancel` | Aborts a pending or executing job and tears down its worktree. | `job_id` (string, required) |
| `pushguard_await` | Synchronously waits up to `timeout_seconds` if the agent explicitly needs the test outcome before continuing. | `job_id` (string, required), `timeout_seconds` (int, default: 300) |
| `pushguard_clean` | Reclaims memory: purges finished jobs from memory, prunes stale worktrees, deletes old logs, and runs GC. | none |

---

## Agent Usage Guidelines

### 1. Offloading Tests Instead of Blocking
When the user asks to test or verify changes before pushing:
* **DO NOT** run `go test -v ./...` directly in the shell if it takes more than 15 seconds.
* **DO** call `pushguard_enqueue`:
  ```json
  {
    "repo_path": "/path/to/repo",
    "command": "go test ./...",
    "auto_push": false
  }
  ```
* Tell the user: *"I've enqueued the tests in an isolated worktree (`pg_...`). You and I can continue editing files immediately while PushGuard tests in the background."*

### 2. Auto-Push on Pass
If the user authorized a push upon green tests:
* Set `"auto_push": true` in `pushguard_enqueue`. PushGuard will execute `git push origin <branch>` automatically once isolated tests succeed, alerting the user via macOS desktop banner.

### 3. Queue Hygiene & Memory Management
PushGuard retains up to 100 recent jobs and automatically truncates logs in memory to 2,000 lines. However, after extensive sessions:
* Call `pushguard_clean` (or CLI `pushguard clean`) to purge completed jobs, prune stale worktrees, and release heap memory back to the OS.

---

## CLI Quick Reference

```bash
# Check if daemon is active or view jobs
pushguard list

# Enqueue from CLI
pushguard enqueue --repo . --cmd "go test ./..."

# Inspect a job
pushguard status pg_a1b2c3d4

# Clean queue and free memory
pushguard clean

# Start background daemon (if not already running)
pushguard daemon --concurrency 1 &
```
