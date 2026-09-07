package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
	"github.com/pushguard/pushguard/internal/git"
	"github.com/pushguard/pushguard/internal/hook"
	"github.com/pushguard/pushguard/internal/mcp"
	"github.com/pushguard/pushguard/internal/notifier"
	"github.com/pushguard/pushguard/internal/runner"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/pkg/client"
)

const Version = "0.1.0"

func printUsage() {
	fmt.Printf(`PushGuard (v%s)
Async local CI & pre-push queue with Git worktree isolation and MCP support.

Usage:
  pushguard <command> [options]

Commands:
  enqueue, run   Enqueue checks for a repository in an isolated worktree
  status         Check status, results, and logs for a job
  list           List queued, running, and recent jobs
  cancel         Cancel a queued or active job
  daemon         Start the local background queue daemon
  mcp            Start stdio MCP server for Claude Code / Antigravity / AI agents
  hook install   Install Git pre-push hook into a repository
  clean          Prune finished jobs, stale worktrees, old logs, and reclaim memory
  version        Display PushGuard version

Examples:
  pushguard enqueue --repo .
  pushguard enqueue --repo ./backend --cmd "go test ./..." --auto-push
  pushguard status pg_a1b2c3d4
  pushguard clean
  pushguard mcp
  pushguard daemon --concurrency 2
`, Version)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "enqueue", "run":
		handleEnqueue(args)
	case "status":
		handleStatus(args)
	case "list", "ls":
		handleList(args)
	case "await", "wait":
		handleAwait(os.Args[2:])
	case "cancel":
		handleCancel(args)
	case "clean":
		handleClean()
	case "daemon":
		handleDaemon(args)
	case "mcp":
		handleMCP()
	case "hook":
		handleHook(args)
	case "version", "-v", "--version":
		fmt.Printf("pushguard version %s\n", Version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func handleEnqueue(args []string) {
	fs := flag.NewFlagSet("enqueue", flag.ExitOnError)
	repo := fs.String("repo", ".", "Path to repository")
	ref := fs.String("ref", "HEAD", "Git ref or commit (default: HEAD / dirty snapshot)")
	cmdStr := fs.String("cmd", "", "Command to run (auto-detected if empty)")
	autoPush := fs.Bool("auto-push", false, "Automatically git push upon green test run")
	wait := fs.Bool("wait", false, "Wait synchronously for completion")
	timeoutSec := fs.Int("timeout", 600, "Timeout in seconds")
	_ = fs.Parse(args)

	var cmd []string
	if *cmdStr != "" {
		cmd = strings.Fields(*cmdStr)
	}

	// Resolve the repo path HERE, against the caller's working directory.
	// The daemon is a separate long-lived process with its own cwd, so a
	// relative path sent as-is is resolved against wherever the daemon happens
	// to have been started -- which produced "invalid git repository at \".\""
	// for a caller sitting in a perfectly good repository.
	repoPath := *repo
	if abs, err := filepath.Abs(repoPath); err == nil {
		repoPath = abs
	}

	req := core.EnqueueRequest{
		RepoPath: repoPath,
		Ref:      *ref,
		Command:  cmd,
		AutoPush: *autoPush,
		Timeout:  time.Duration(*timeoutSec) * time.Second,
	}

	c := client.New("")

	// Try to get (or auto-start) the shared daemon so this job coordinates
	// with everything else on the machine instead of running in isolation.
	if err := c.EnsureDaemon(1); err != nil {
		fmt.Fprintf(os.Stderr, "ℹ️  Could not start shared daemon (%s); running standalone.\n", err)
	}

	// If daemon is running, use client
	if c.IsDaemonRunning() {
		job, pos, err := c.Enqueue(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Job enqueued to local daemon!\n")
		fmt.Printf("   Job ID:    %s\n", job.ID)
		fmt.Printf("   Repo:      %s\n", job.RepoPath)
		fmt.Printf("   Ref:       %s\n", job.Ref)
		fmt.Printf("   Position:  #%d in queue\n", pos)
		fmt.Printf("   Worktree:  Clean & unlocked (you can keep coding)\n")
		// Nothing here notifies a caller that walked away: the desktop
		// notification reaches a human, and this command has already exited as
		// far as any process watching it is concerned. Name the follow-up, so
		// "enqueue and carry on" does not quietly become "enqueue and forget".
		fmt.Printf("   Notify:    pushguard await %s --timeout 1500   (run it in the background)\n", job.ID)

		if *wait {
			fmt.Printf("Waiting for job to finish...\n")
			for {
				time.Sleep(1 * time.Second)
				j, err := c.GetJob(job.ID)
				if err != nil {
					continue
				}
				if j.IsFinished() {
					fmt.Printf("Result: %s (exit %d) in %s\n", strings.ToUpper(string(j.Status)), j.ExitCode, j.DurationString())
					if j.Status != core.StatusPassed {
						logs, _ := c.GetLogs(j.ID, 30)
						fmt.Println("\n--- Last 30 lines of logs ---")
						fmt.Println(logs)
						os.Exit(j.ExitCode)
					}
					os.Exit(0)
				}
			}
		}
		return
	}

	// Daemon is not running -> run inline with local embedded engine
	fmt.Println("ℹ️  Daemon not detected. Running with local standalone engine...")
	memStore := store.NewMemoryStore(100)
	wm, err := git.NewWorktreeManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing worktrees: %s\n", err)
		os.Exit(1)
	}
	r, _ := runner.NewLocalRunner("")
	n := notifier.NewMultiNotifier(notifier.NewMacOSNotifier(), &notifier.TerminalBellNotifier{})

	engine := core.NewEngine(
		core.EngineConfig{MaxConcurrent: 1, DefaultTimeout: time.Duration(*timeoutSec) * time.Second},
		memStore, wm, r, n,
		func(dir string) []string { return runner.DetectCommand(dir).Command },
	)
	engine.Start()
	defer engine.Stop()

	job, _, err := engine.Enqueue(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Job %s scheduled in isolated worktree.\n", job.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	finishedJob, err := engine.AwaitJob(ctx, job.ID, 500*time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Wait error: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("Finished: %s in %s (exit code %d)\n",
		strings.ToUpper(string(finishedJob.Status)),
		finishedJob.DurationString(),
		finishedJob.ExitCode,
	)

	if finishedJob.Status != core.StatusPassed {
		fmt.Println("\n--- Output Log Snippet ---")
		fmt.Println(finishedJob.TailLogs(30))
		os.Exit(finishedJob.ExitCode)
	}
}

func handleStatus(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pushguard status <job-id>")
		os.Exit(1)
	}
	jobID := args[0]
	c := client.New("")

	if !c.IsDaemonRunning() {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running.")
		os.Exit(1)
	}

	job, err := c.GetJob(jobID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("Job:      %s\n", job.ID)
	fmt.Printf("Status:   %s\n", strings.ToUpper(string(job.Status)))
	fmt.Printf("Repo:     %s\n", job.RepoPath)
	fmt.Printf("Branch:   %s\n", job.Branch)
	fmt.Printf("Command:  %s\n", strings.Join(job.Command, " "))
	fmt.Printf("Duration: %s\n", job.DurationString())
	if job.ExitCode != 0 {
		fmt.Printf("Exit:     %d\n", job.ExitCode)
	}
	if job.Error != "" {
		fmt.Printf("Error:    %s\n", job.Error)
	}

	logs, err := c.GetLogs(jobID, 400)
	if err != nil || logs == "" {
		return
	}
	// Lead with why it failed. A failed pre-push run is several hundred lines
	// of "ok" and a handful that matter, and the tail is whatever finished
	// last -- which is rarely the thing that broke.
	if failure := core.ExtractFailure(logs, 30); failure != "" {
		fmt.Println("\n--- Why it failed ---")
		fmt.Println(failure)
	}
	fmt.Println("\n--- Logs (last 40 lines) ---")
	fmt.Println(tailLines(logs, 40))
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func handleList(args []string) {
	c := client.New("")
	if !c.IsDaemonRunning() {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running.")
		os.Exit(1)
	}

	jobs, err := c.ListJobs(20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	if len(jobs) == 0 {
		fmt.Println("No jobs found.")
		return
	}

	fmt.Printf("%-14s %-10s %-25s %-10s %s\n", "JOB ID", "STATUS", "REPOSITORY", "DURATION", "COMMAND")
	fmt.Println(strings.Repeat("-", 80))
	for _, j := range jobs {
		fmt.Printf("%-14s %-10s %-25s %-10s %s\n",
			j.ID,
			j.Status,
			truncate(j.Name, 25),
			j.DurationString(),
			truncate(strings.Join(j.Command, " "), 25),
		)
	}
}

// handleAwait blocks until a job finishes. `enqueue --wait` already covers
// "start it and wait", but there was no way to wait on a job that is ALREADY
// queued -- so a caller who enqueued, kept working, and then genuinely needed
// the answer had to invent a polling loop. The MCP surface has had
// pushguard_await from the start; this is the CLI half of it.
func handleAwait(args []string) {
	fs := flag.NewFlagSet("await", flag.ExitOnError)
	timeoutSec := fs.Int("timeout", 900, "Seconds to wait before giving up")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pushguard await <job-id> [--timeout <seconds>]")
		os.Exit(1)
	}
	jobID := fs.Arg(0)

	c := client.New("")
	if !c.IsDaemonRunning() {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running.")
		os.Exit(1)
	}

	deadline := time.Now().Add(time.Duration(*timeoutSec) * time.Second)
	for {
		job, err := c.GetJob(jobID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
		if job.IsFinished() {
			fmt.Printf("%s (exit %d) in %s\n", strings.ToUpper(string(job.Status)), job.ExitCode, job.DurationString())
			if logs, err := c.GetLogs(jobID, 400); err == nil {
				if failure := core.ExtractFailure(logs, 30); failure != "" {
					fmt.Println("\n--- Why it failed ---")
					fmt.Println(failure)
				}
			}
			// Exit non-zero on a red job so a shell caller can branch on it.
			if job.Status != core.StatusPassed {
				os.Exit(1)
			}
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "Timed out after %ds; job %s is still %s.\n", *timeoutSec, jobID, job.Status)
			os.Exit(2)
		}
		time.Sleep(5 * time.Second)
	}
}

func handleCancel(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pushguard cancel <job-id>")
		os.Exit(1)
	}
	jobID := args[0]
	c := client.New("")

	if !c.IsDaemonRunning() {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running.")
		os.Exit(1)
	}

	if err := c.CancelJob(jobID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
	fmt.Printf("Job %s cancelled.\n", jobID)
}

func handleClean() {
	c := client.New("")
	if c.IsDaemonRunning() {
		report, err := c.Clean()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Clean error: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("🧹 Daemon queue cleaned successfully:\n")
		fmt.Printf("   Jobs Flushed:      %d\n", report.JobsFlushed)
		fmt.Printf("   Worktrees Pruned:  %d\n", report.WorktreesPruned)
		fmt.Printf("   Logs Pruned:       %d\n", report.LogsPruned)
		fmt.Printf("   Heap Memory:       %.2f MB\n", float64(report.AllocBytesAfter)/(1024*1024))
		return
	}

	// Standalone clean
	wm, _ := git.NewWorktreeManager("")
	r, _ := runner.NewLocalRunner("")
	memStore := store.NewMemoryStore(100)
	engine := core.NewEngine(core.EngineConfig{MaxConcurrent: 1}, memStore, wm, r, nil, nil)
	report := engine.Clean()

	fmt.Printf("🧹 Local workspace cleaned successfully:\n")
	fmt.Printf("   Worktrees Pruned:  %d\n", report.WorktreesPruned)
	fmt.Printf("   Logs Pruned:       %d\n", report.LogsPruned)
	fmt.Printf("   Heap Memory:       %.2f MB\n", float64(report.AllocBytesAfter)/(1024*1024))
}

// daemonJobDir is where job records are mirrored. Alongside the socket and
// logs, so `pushguard clean` and the existing ~/.pushguard layout stay one
// place.
func daemonJobDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pushguard", "jobs")
}

// defaultRedisAddr lets PUSHGUARD_REDIS_ADDR override the standard local
// Redis address without requiring the --redis-addr flag on every invocation
// (matches how DOPPLER_TOKEN-style env overrides work elsewhere in this
// workspace). Empty stays empty: an explicit "" disables Redis publishing.
func defaultRedisAddr() string {
	if v, ok := os.LookupEnv("PUSHGUARD_REDIS_ADDR"); ok {
		return v
	}
	return "127.0.0.1:6379"
}

func handleDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socketPath := fs.String("socket", daemon.DefaultSocketPath(), "Path to Unix socket")
	httpAddr := fs.String("http", "", "Optional HTTP address to bind (e.g. 127.0.0.1:8942)")
	concurrency := fs.Int("concurrency", 1, "Max concurrent test workers")
	idleMinutes := fs.Int("idle-shutdown-minutes", 30, "Self-shutdown after this many idle minutes with no jobs (0 disables)")
	redisAddr := fs.String("redis-addr", defaultRedisAddr(), "Redis address for job-event pub/sub (empty disables); overridable via PUSHGUARD_REDIS_ADDR")
	_ = fs.Parse(args)
	idleShutdown := time.Duration(*idleMinutes) * time.Minute

	// Mirror jobs to disk. The daemon is long-lived and callers are encouraged
	// to enqueue and walk away, so a restart that erased every queued and
	// running job would be invisible to exactly the people relying on it.
	jobStore := store.NewDurable(store.NewMemoryStore(200), daemonJobDir())
	orphans := jobStore.RecoverOrphans()

	memStore := jobStore
	wm, err := git.NewWorktreeManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize worktrees: %s\n", err)
		os.Exit(1)
	}

	// An interrupted job also leaves its worktree behind, and the age-based
	// sweep in Clean will not touch one for an hour. A checkout of any size is
	// expensive to leave lying around -- a large monorepo's worktree can easily be several hundred MB, so a
	// few interrupted runs fill a disk, and the resulting ENOSPC surfaces as
	// unrelated build failures in whatever runs next.
	//
	// Nothing can be running at startup, so every recovered orphan's worktree
	// is safe to remove now rather than in an hour. Removing it through the
	// recorded RepoPath is what lets git drop its .git/worktrees metadata too;
	// the generic sweep only has the directory, so it can only unlink the files.
	for _, orphan := range orphans {
		fmt.Printf("⚠️  %s (%s): %s\n", orphan.ID, orphan.RepoPath, orphan.Error)
		// WorktreeDir is assigned to the in-memory job the instant the worktree
		// is created, but the record on disk may predate that write -- a job
		// killed in its first second has a persisted WorktreeDir of "". The
		// path is derivable, because CreateWorktree always builds
		// BaseDir/<job id>, so fall back to that rather than leaving the
		// directory behind for the age-based sweep.
		worktreeDir := orphan.WorktreeDir
		if worktreeDir == "" {
			worktreeDir = filepath.Join(wm.BaseDir, orphan.ID)
		}
		if orphan.RepoPath != "" {
			_ = wm.RemoveWorktree(orphan.RepoPath, worktreeDir)
		}
	}
	r, _ := runner.NewLocalRunner("")
	notifiers := []notifier.Notifier{notifier.NewMacOSNotifier(), &notifier.TerminalBellNotifier{}}
	if *redisAddr != "" {
		if rn, err := notifier.NewRedisNotifier(*redisAddr, ""); err != nil {
			fmt.Fprintf(os.Stderr, "[pushguard] Redis event publishing disabled (%s unreachable: %s)\n", *redisAddr, err)
		} else {
			defer rn.Close()
			notifiers = append(notifiers, rn)
			fmt.Printf("   Redis events: publishing to %q on %s\n", notifier.DefaultRedisEventsChannel, *redisAddr)
		}
	}
	n := notifier.NewMultiNotifier(notifiers...)

	engine := core.NewEngine(
		core.EngineConfig{
			MaxConcurrent:  *concurrency,
			DefaultTimeout: 15 * time.Minute,
		},
		memStore, wm, r, n,
		func(dir string) []string { return runner.DetectCommand(dir).Command },
	)

	server := daemon.NewServer(engine, daemon.Config{
		SocketPath: *socketPath,
		HTTPAddr:   *httpAddr,
	})

	engine.Start()
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start daemon: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("🚀 PushGuard daemon running (workers=%d)\n", *concurrency)
	fmt.Printf("   Unix Socket: %s\n", *socketPath)
	if *httpAddr != "" {
		fmt.Printf("   HTTP API:    http://%s\n", *httpAddr)
	}
	if idleShutdown > 0 {
		fmt.Printf("   Idle shutdown: after %s with no queued/running jobs\n", idleShutdown)
	} else {
		fmt.Printf("   Idle shutdown: disabled\n")
	}

	// Wait for terminate signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Self-shutdown when idle: a daemon nobody is using should not sit
	// around forever on a personal machine. It cleans up after itself
	// (same sweep as `pushguard clean`) right before exiting, and the next
	// enqueue call simply respawns it.
	idleDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-idleDone:
				return
			case <-ticker.C:
				if idleShutdown <= 0 {
					continue
				}
				if engine.ActiveJobCount() == 0 && engine.IdleDuration() >= idleShutdown {
					report := engine.Clean()
					fmt.Printf("\n[pushguard] idle for %s with no jobs — cleaning up and shutting down "+
						"(jobs flushed=%d, worktrees pruned=%d, logs pruned=%d)\n",
						idleShutdown, report.JobsFlushed, report.WorktreesPruned, report.LogsPruned)
					sigChan <- syscall.SIGTERM
					return
				}
			}
		}
	}()

	// Prune stale worktrees/logs on a fixed schedule, independent of the idle
	// timer above. A daemon queued dozens of times a day may never go idle
	// long enough to trigger idle-shutdown's full Clean(), which would
	// otherwise leave old worktrees/logs piling up all day. This only
	// touches disk state (not the in-memory job store), so it never makes a
	// still-relevant job disappear from pushguard_list/status.
	diskPruneDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-diskPruneDone:
				return
			case <-ticker.C:
				worktreesPruned, logsPruned := engine.PruneDiskArtifacts()
				if worktreesPruned > 0 || logsPruned > 0 {
					fmt.Printf("[pushguard] scheduled disk prune: worktrees pruned=%d, logs pruned=%d\n",
						worktreesPruned, logsPruned)
				}
			}
		}
	}()

	<-sigChan
	close(idleDone)
	close(diskPruneDone)

	fmt.Println("\nShutting down PushGuard daemon...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = server.Stop(ctx)

	// Cancel in-flight work BEFORE stopping, or this never returns.
	//
	// Engine.Stop() is wg.Wait(): it waits for the worker loops, and a worker
	// running an ~8 minute pre-push gate will hold it for the full ~8 minutes.
	// So `pkill pushguard daemon` did not stop the daemon at all — it left it
	// hanging until the job it was already running finished, which looks
	// exactly like a daemon that ignored the signal.
	//
	// Cancelling first kills the job processes, the workers return, and Stop()
	// completes promptly.
	inFlight := []*core.Job{}
	for _, job := range jobStore.List(0) {
		if job != nil && !job.IsFinished() {
			inFlight = append(inFlight, job)
			_ = engine.Cancel(job.ID)
		}
	}
	if len(inFlight) > 0 {
		fmt.Printf("Cancelling %d in-flight job(s)...\n", len(inFlight))
	}

	// Only now is it safe to touch a worktree: deleting a directory out from
	// under a running compiler produces a failure that looks like a code error
	// and is not.
	engine.Stop()

	// Reclaim what the interrupted jobs were using.
	//
	// The idle-shutdown path already cleaned up on its way out; a signal did
	// not, so `pkill pushguard daemon` left every worktree behind for the
	// age-based sweep to find an hour later — and a large monorepo's worktree
	// can easily be several hundred MB, so a few interrupted runs fill a
	// disk. The resulting ENOSPC then surfaces as unrelated build failures in
	// whatever runs next, which is a genuinely confusing thing to debug.
	//
	// This is the graceful half. It cannot be the whole answer: SIGKILL runs no
	// code at all, so RecoverOrphans at startup stays as the backstop. Belt and
	// braces on purpose — the two cover different failures.
	reclaimed := 0
	for _, job := range inFlight {
		// Cancel already moved these to cancelled; restate WHY, because
		// "cancelled" alone reads as somebody having cancelled it on purpose.
		job.Error = "daemon shut down while this job was in flight; it did not run to completion — re-enqueue it"
		if job.FinishedAt == nil {
			finished := time.Now()
			job.FinishedAt = &finished
		}
		job.Status = core.StatusCancelled
		_ = jobStore.Update(job)

		worktreeDir := job.WorktreeDir
		if worktreeDir == "" {
			worktreeDir = filepath.Join(wm.BaseDir, job.ID)
		}
		if job.RepoPath != "" {
			_ = wm.RemoveWorktree(job.RepoPath, worktreeDir)
			reclaimed++
		}
	}
	if reclaimed > 0 {
		fmt.Printf("Reclaimed %d worktree(s) from interrupted job(s); re-enqueue them.\n", reclaimed)
	}

	fmt.Println("Daemon stopped cleanly.")
}

func handleMCP() {
	server := mcp.NewServer()
	if err := server.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %s\n", err)
		os.Exit(1)
	}
}

func handleHook(args []string) {
	if len(args) == 0 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "Usage: pushguard hook install [--repo <path>] [--async]")
		os.Exit(1)
	}
	fs := flag.NewFlagSet("hook install", flag.ExitOnError)
	repo := fs.String("repo", ".", "Repository root")
	async := fs.Bool("async", false, "Use async mode with auto-push")
	_ = fs.Parse(args[1:])

	hookPath, err := hook.InstallPrePushHook(*repo, *async)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to install hook: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Pre-push hook installed at: %s\n", hookPath)
	if *async {
		fmt.Println("   Mode: ASYNC (Pre-push pushes automatically when isolated tests pass)")
	} else {
		fmt.Println("   Mode: SYNC (Runs in isolated worktree without locking your main files)")
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
