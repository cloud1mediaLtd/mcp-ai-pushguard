# PushGuard 🛡️

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

**Short answer:** No, but running the daemon in the background gives the best multi-agent experience.

### Option A: Daemon Mode (Recommended for Multi-Agent / Multi-Tab Setups)
Run the daemon in the background:
```bash
pushguard daemon --concurrency 1
```
* **Why do this?** A single background daemon coordinates all your terminal tabs, Claude Code sessions, ChatGPT tasks, and git hooks into **one global queue**. It strictly throttles concurrency so your machine stays quiet and responsive.
* **How to run in background**:
  ```bash
  # Quick background run
  pushguard daemon --concurrency 1 > ~/.pushguard/daemon.log 2>&1 &
  ```

### Option B: On-Demand / Embedded Mode (Zero Maintenance)
If the daemon is NOT running, PushGuard automatically spins up an in-process engine whenever Claude, an agent, or a CLI command invokes it. No manual background process required!

---

## 🚀 Installation & Setup

### 1. Build the Binary

```bash
git clone https://github.com/pushguard/pushguard.git
cd pushguard
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

PushGuard is designed for 24/7 laptop usage with minimal memory footprint (< 1 MB heap):

1. **Log Truncation**: In-memory logs are capped at 2,000 lines per job. Full logs are written to disk (`~/.cache/pushguard/logs/<job_id>.log`).
2. **Circular Job History**: In-memory job store defaults to a 100-job circular capacity, automatically evicting oldest jobs.
3. **One-Command Cleanup**:
   ```bash
   pushguard clean
   ```
   Or via the agent tool `pushguard_clean`:
   * Flushes completed jobs from memory.
   * Prunes orphaned worktrees older than 1 hour.
   * Deletes test logs older than 48 hours.
   * Forces Go runtime garbage collection (`runtime.GC()`).

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

## 💻 CLI Command Reference

```bash
# Enqueue tests for current repo (auto-detects Go, Node/pnpm/npm, Cargo, Makefile)
pushguard enqueue

# Enqueue with custom command and auto-push
pushguard enqueue --repo ./c1m-server --cmd "go test -v ./..." --auto-push

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
