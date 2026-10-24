package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/pushguard/pushguard/internal/verification/application"
	"github.com/pushguard/pushguard/pkg/api"
	"github.com/pushguard/pushguard/pkg/client"
)

const Version = "0.2.0"

// BuildCommit is injected by release builds. A source build stays explicit so
// a daemon and client built from different source revisions cannot look equal.
var BuildCommit = "dev"

func buildID() string { return Version + "+" + BuildCommit }

func printUsage() {
	fmt.Printf(`PushGuard (v%s)
Async local CI & pre-push queue with Git worktree isolation and MCP support.

Usage:
  pushguard <command> [options]

Commands:
  enqueue, run   Enqueue checks for a repository in an isolated worktree
  await, wait    Wait for a completed verdict (default: 1500 seconds)
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
	pushguard enqueue --repo ./backend --cmd "go test ./..." --ref HEAD
	pushguard status pg_a1b2c3d4
	pushguard list --ref <commit> --limit 10
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

const exitSubmissionFailed = 4

func handleEnqueue(args []string) { os.Exit(runEnqueue(args)) }

func daemonClient(socket string) *client.Client {
	return client.New(socket, client.WithBuild(buildID()))
}

func runEnqueue(args []string) int { return runEnqueueWithClient(args, daemonClient("")) }

func runEnqueueWithClient(args []string, c *client.Client) int {
	fs := flag.NewFlagSet("enqueue", flag.ContinueOnError)
	repo := fs.String("repo", ".", "Path to repository")
	ref := fs.String("ref", "", "Committed Git ref (default: HEAD)")
	snapshot := fs.Bool("snapshot", false, "Explicitly snapshot working-tree changes; cannot combine with --ref")
	cmdStr := fs.String("cmd", "", "Shell command to run (auto-detected if empty)")
	autoPush := fs.Bool("auto-push", false, "Disabled: pushing requires a separate action")
	wait := fs.Bool("wait", false, "Wait for completion")
	timeoutSec := fs.Int("timeout", 900, "Execution timeout in seconds")
	waitSec := fs.Int("wait-timeout", 1500, "Seconds to wait, including queue time")
	dedupeKey := fs.String("dedupe-key", "", "Reuse an active job with this verification identity")
	if err := fs.Parse(args); err != nil {
		return exitSubmissionFailed
	}
	if *autoPush || (*snapshot && *ref != "") || *timeoutSec <= 0 || *waitSec <= 0 || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Invalid submission: auto-push is disabled; snapshot and ref are exclusive; timeouts must be positive; unexpected operands are not accepted.")
		return exitSubmissionFailed
	}
	repoPath, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSubmissionFailed
	}
	var command []string
	if strings.TrimSpace(*cmdStr) != "" {
		command = []string{*cmdStr}
	}
	req := api.EnqueueRequest{RepoPath: repoPath, Ref: *ref, Snapshot: *snapshot, Command: command, Timeout: time.Duration(*timeoutSec) * time.Second, DedupeKey: *dedupeKey}
	if warning, err := core.CheckDiskSpace(worktreeBaseDir()); err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to enqueue: %s\n", err)
		return exitSubmissionFailed
	} else if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	if err := c.EnsureDaemon(client.DefaultConcurrency()); err != nil {
		if errors.Is(err, client.ErrIncompatible) {
			fmt.Fprintln(os.Stderr, err)
			return exitSubmissionFailed
		}
		fmt.Fprintf(os.Stderr, "Shared daemon unavailable (%s); running standalone to completion.\n", err)
	}
	if c.IsDaemonRunning() {
		job, pos, err := c.Enqueue(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitSubmissionFailed
		}
		if job.Reused {
			fmt.Printf("Job ID: %s\nReused active verification for tested SHA: %s\n", job.ID, job.Ref)
		} else {
			fmt.Printf("Job ID: %s\nRepository: %s\nSource: %s\nTested SHA: %s\nRequested ref: %s\nBase SHA: %s\nPosition: %d\n", job.ID, job.RepoPath, job.SourceKind, job.Ref, job.RequestedRef, job.BaseSHA, pos)
		}
		fmt.Printf("Worktree: Clean & unlocked (you can keep coding)\nNotify: pushguard await %s --timeout 1500 (run in the background)\n", job.ID)
		if !*wait {
			return 0
		}
		return awaitResult(c, job.ID, time.Duration(*waitSec)*time.Second)
	}
	base, err := os.MkdirTemp("", "pushguard-standalone-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSubmissionFailed
	}
	wm, err := git.NewWorktreeManager(filepath.Join(base, "worktrees"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSubmissionFailed
	}
	r, err := runner.NewLocalRunner(filepath.Join(base, "logs"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSubmissionFailed
	}
	engine := core.NewEngine(core.EngineConfig{DefaultTimeout: time.Duration(*timeoutSec) * time.Second, Build: buildID()}, store.NewMemoryStore(100), wm, r, nil,
		func(dir string) []string { return runner.DetectCommand(dir).Command })
	engine.Start()
	defer engine.Stop()
	job, _, err := engine.Enqueue(core.EnqueueRequest{RepoPath: req.RepoPath, Ref: req.Ref, Command: req.Command, Name: req.Name, AutoPush: req.AutoPush, Timeout: req.Timeout, Snapshot: req.Snapshot, DedupeKey: req.DedupeKey})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSubmissionFailed
	}
	fmt.Printf("Standalone job %s: %s %s (logs: %s)\n", job.ID, job.SourceKind, job.Ref, filepath.Join(base, "logs"))
	// Standalone has no process to outlive us: supervise until execution finishes.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	result, err := engine.AwaitJob(ctx, job.ID, 100*time.Millisecond)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitJobNoVerdict
	}
	logs := result.TailLogs(400)
	apiResult := application.Snapshot(result)
	reportFinishedJob(apiResult, logs)
	return exitCodeFor(apiResult)
}

func awaitResult(c *client.Client, jobID string, timeout time.Duration) int {
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, timeout)
	defer cancel()
	job, err := c.Await(ctx, jobID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Await failed: %s\n", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return exitAwaitTimeout
		}
		if errors.Is(err, client.ErrNoVerdict) || errors.Is(err, context.Canceled) {
			return exitJobNoVerdict
		}
		return exitJobFailed
	}
	reportFinishedJob(*job, readJobLog(jobID))
	return exitCodeFor(*job)
}

func handleStatus(args []string) {
	if len(args) == 0 {
		if !showDaemonStatus(daemonClient("")) {
			os.Exit(1)
		}
		return
	}
	jobID := args[0]
	c := daemonClient("")

	if !c.IsDaemonRunning() {
		statusFromDisk(jobID)
		return
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
	printProvenance(*job)
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
	if failure := core.ExtractFailure(logs, 30); failure != "" && job.Status != api.StatusPassed {
		if names := core.FailureNames(logs); len(names) > 0 {
			fmt.Printf("\n--- %d failing test(s): %s\n", len(names), strings.Join(names, ", "))
		}
		fmt.Println("\n--- Why it failed (excerpt; read the full log for the rest) ---")
		fmt.Println(failure)
	}
	fmt.Println("\n--- Logs (last 40 lines) ---")
	fmt.Println(tailLines(logs, 40))
}

func showDaemonStatus(c *client.Client) bool {
	health, err := c.Health(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "PushGuard daemon is not running: %s\n", err)
		return false
	}
	agreement := "yes"
	if c.Build() != "" && c.Build() != health.Build {
		agreement = "NO"
	}
	fmt.Printf("Daemon build: %s | Client build: %s | Agreement: %s\n", health.Build, c.Build(), agreement)
	fmt.Printf("PID: %d | Uptime: %s | Queue depth: %d | Active jobs: %d | Concurrency: %d\n", health.PID, time.Duration(health.UptimeSeconds)*time.Second, health.QueueDepth, health.ActiveJobs, health.Concurrency)
	if agreement == "NO" {
		fmt.Fprintln(os.Stderr, "Submission is blocked until active jobs finish and the daemon is restarted.")
		return false
	}
	return true
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
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	ref := fs.String("ref", "", "Only show jobs for this tested SHA or requested ref")
	limit := fs.Int("limit", 30, "Maximum jobs to show")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *limit < 1 {
		fmt.Fprintln(os.Stderr, "Usage: pushguard list [--ref <commit-or-ref>] [--limit <count>]")
		return
	}
	c := daemonClient("")
	if !c.IsDaemonRunning() {
		listFromDisk(*limit, *ref)
		return
	}

	jobs, err := c.ListJobsByRef(*limit, *ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	if len(jobs) == 0 {
		fmt.Println("No jobs found.")
		return
	}

	// Ordered running → queued (in the order they will run) → finished, because
	// the questions a second agent actually has are "what is running", "what is
	// ahead of mine", and only then "what happened recently".
	application.SortForQueueView(jobs)
	renderJobTable(jobs)
}

// renderJobTable prints the queue view. Split out of handleList so the
// offline path below renders identically -- a second table that drifted from
// this one would be worse than no offline listing at all.
func renderJobTable(jobs []api.Job) {
	fmt.Printf("%-14s %-10s %-4s %-10s %-24s %-10s %s\n", "JOB ID", "STATUS", "POS", "REF", "REPOSITORY", "DURATION", "COMMAND")
	fmt.Println(strings.Repeat("-", 108))
	position := 0
	for _, j := range jobs {
		// Position is only meaningful for work that has not started. A number
		// beside a finished job would read as history rather than a queue.
		pos := "-"
		if j.Status == api.StatusQueued {
			position++
			pos = fmt.Sprintf("%d", position)
		}
		fmt.Printf("%-14s %-10s %-4s %-10s %-24s %-10s %s\n",
			j.ID,
			j.Status,
			pos,
			shortRef(j),
			truncate(j.Name, 24),
			j.DurationString(),
			truncate(strings.Join(j.Command, " "), 40),
		)
	}
}

// shortRef prefers the tested ref. Legacy records may only carry CommitSHA.
func shortRef(j api.Job) string {
	ref := strings.TrimSpace(j.Ref)
	if ref == "" {
		ref = strings.TrimSpace(j.CommitSHA)
	}
	if ref == "" {
		return "-"
	}
	if len(ref) > 9 {
		return ref[:9]
	}
	return ref
}

// handleAwait blocks until a job finishes. `enqueue --wait` already covers
// "start it and wait", but there was no way to wait on a job that is ALREADY
// queued -- so a caller who enqueued, kept working, and then genuinely needed
// the answer had to invent a polling loop. The MCP surface has had
// pushguard_await from the start; this is the CLI half of it.
// splitPositional lifts a LEADING operand out of the argument list, leaving the
// rest for a flag parser.
//
// Only a leading one, deliberately. A bare word anywhere else is ambiguous: in
// `--timeout 60` the 60 is a flag value, not an operand, and telling the two
// apart needs the flag set's arity. Handling the leading case is enough,
// because that is the only order Go's own parser cannot cope with — with flags
// first it stops at the operand and does the right thing on its own.
func splitPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func handleAwait(args []string) {
	jobID, flags := splitPositional(args)
	fs := flag.NewFlagSet("await", flag.ExitOnError)
	timeoutSec := fs.Int("timeout", 1500, "Seconds to wait before giving up")
	_ = fs.Parse(flags)
	if jobID == "" {
		jobID = fs.Arg(0)
	}
	if jobID == "" || *timeoutSec <= 0 {
		fmt.Fprintln(os.Stderr, "Usage: pushguard await <job-id> [--timeout <positive seconds>]")
		os.Exit(1)
	}
	os.Exit(awaitResult(daemonClient(""), jobID, time.Duration(*timeoutSec)*time.Second))
}

func handleCancel(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pushguard cancel <job-id>")
		os.Exit(1)
	}
	jobID := args[0]
	c := daemonClient("")

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
	c := daemonClient("")
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

	// Standalone cleanup must own shared state before touching artifacts.
	ownership, err := daemon.AcquireOwnership(filepath.Dir(daemonJobDir()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot clean daemon state: %s\n", err)
		os.Exit(1)
	}
	defer ownership.Close()
	if err := daemon.AssertSocketAvailable(daemon.DefaultSocketPath()); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot clean daemon state: %s\n", err)
		os.Exit(1)
	}
	wm, err := git.NewWorktreeManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize worktrees: %s\n", err)
		os.Exit(1)
	}
	r, err := runner.NewLocalRunner("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logs: %s\n", err)
		return
	}
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
// worktreeBaseDir is where worktrees are created, and so the filesystem whose
// free space actually matters. Mirrors git.NewWorktreeManager's default.
func worktreeBaseDir() string {
	if base := os.Getenv("PUSHGUARD_STATE_DIR"); base != "" {
		return filepath.Join(base, "worktrees")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, ".cache", "pushguard", "worktrees")
}

func daemonJobDir() string { return store.DefaultJobDir() }

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

func handleDaemon(args []string) { os.Exit(runDaemon(args)) }

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	socketPath := fs.String("socket", daemon.DefaultSocketPath(), "Path to Unix socket")
	httpAddr := fs.String("http", "", "Optional HTTP address to bind (e.g. 127.0.0.1:8942)")
	// Default from PUSHGUARD_CONCURRENCY so the value survives the daemon's
	// idle-shutdown and the auto-respawn that follows it; an explicit flag
	// still wins for a hand-started daemon.
	concurrency := fs.Int("concurrency", client.DefaultConcurrency(), "Max concurrent test workers (default from PUSHGUARD_CONCURRENCY)")
	idleMinutes := fs.Int("idle-shutdown-minutes", 30, "Self-shutdown after this many idle minutes with no jobs (0 disables)")
	redisAddr := fs.String("redis-addr", defaultRedisAddr(), "Redis address for job-event pub/sub (empty disables); overridable via PUSHGUARD_REDIS_ADDR")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *concurrency <= 0 || *idleMinutes < 0 {
		fmt.Fprintln(os.Stderr, "Invalid daemon concurrency/idle timeout")
		return 1
	}
	idleShutdown := time.Duration(*idleMinutes) * time.Minute

	// Mirror jobs to disk. The daemon is long-lived and callers are encouraged
	// to enqueue and walk away, so a restart that erased every queued and
	// running job would be invisible to exactly the people relying on it.
	ownership, err := daemon.AcquireOwnership(filepath.Dir(daemonJobDir()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot own daemon state: %s\n", err)
		return 1
	}
	defer ownership.Close()
	if err := daemon.AssertSocketAvailable(*socketPath); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot recover daemon state: %s\n", err)
		return 1
	}
	jobStore := store.NewDurable(store.NewMemoryStore(200), daemonJobDir())
	orphans := jobStore.RecoverOrphans()

	memStore := jobStore
	wm, err := git.NewWorktreeManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize worktrees: %s\n", err)
		return 1
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
			if err := wm.RemoveWorktree(orphan.RepoPath, worktreeDir); err != nil {
				fmt.Fprintf(os.Stderr, "Orphan cleanup: %s\n", err)
			}
		}
	}
	r, err := runner.NewLocalRunner("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logs: %s\n", err)
		return 1
	}
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
			Build:          buildID(),
		},
		memStore, wm, r, n,
		func(dir string) []string { return runner.DetectCommand(dir).Command },
	)

	server := daemon.NewServer(application.NewEngineService(engine), daemon.Config{
		SocketPath:  *socketPath,
		HTTPAddr:    *httpAddr,
		Build:       buildID(),
		Concurrency: *concurrency,
	})

	engine.Start()
	defer engine.Stop()
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start daemon: %s\n", err)
		return 1
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
	defer signal.Stop(sigChan)
	var maintenance sync.WaitGroup

	// Self-shutdown when idle: a daemon nobody is using should not sit
	// around forever on a personal machine. It cleans up after itself
	// (same sweep as `pushguard clean`) right before exiting, and the next
	// enqueue call simply respawns it.
	idleDone := make(chan struct{})
	maintenance.Add(1)
	go func() {
		defer maintenance.Done()
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
				if engine.TryDrainIdle(idleShutdown) {
					if before, after, ran := engine.ReclaimDiskIfIdle(context.Background(), worktreeBaseDir()); ran {
						fmt.Printf("\n[pushguard] reclaimed the Go build cache before idle shutdown: %s free -> %s free\n",
							core.HumanBytes(before), core.HumanBytes(after))
					}
					report := engine.Clean()
					fmt.Printf("\n[pushguard] idle for %s with no jobs — cleaning up and shutting down "+
						"(jobs flushed=%d, worktrees pruned=%d, logs pruned=%d)\n",
						idleShutdown, report.JobsFlushed, report.WorktreesPruned, report.LogsPruned)
					select {
					case sigChan <- syscall.SIGTERM:
					default:
					}
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
	maintenance.Add(1)
	go func() {
		defer maintenance.Done()
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-diskPruneDone:
				return
			case <-ticker.C:
				worktreesPruned, logsPruned := engine.PruneDiskArtifacts()
				jobRecordsPruned, err := jobStore.PruneOld(48 * time.Hour)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[pushguard] scheduled job-record prune failed: %s\n", err)
				}
				if worktreesPruned > 0 || logsPruned > 0 || jobRecordsPruned > 0 {
					fmt.Printf("[pushguard] scheduled disk prune: worktrees pruned=%d, logs pruned=%d, job records pruned=%d\n",
						worktreesPruned, logsPruned, jobRecordsPruned)
				}
				// Worktrees and logs are usually not where the space went. The
				// Go build cache has no native size limit and grew to 171 GB on
				// 2026-09-09, filling a 926 GB volume and failing three gates
				// with misattributed "[build failed]" errors. Reclaim it at its
				// own ceiling or before enqueue thresholds start refusing work.
				if before, after, ran := engine.ReclaimDiskIfIdle(context.Background(), worktreeBaseDir()); ran {
					fmt.Printf("[pushguard] reclaimed the Go build cache: %s free -> %s free\n",
						core.HumanBytes(before), core.HumanBytes(after))
				}
			}
		}
	}()

	<-sigChan
	close(idleDone)
	close(diskPruneDone)
	maintenance.Wait()

	fmt.Println("\nShutting down PushGuard daemon...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Stop(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Listener shutdown: %s\n", err)
	}

	engine.Stop()

	fmt.Println("Daemon stopped cleanly.")
	return 0
}

func handleMCP() {
	server := mcp.NewServer(buildID())
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
	async := fs.Bool("async", false, "Disabled: asynchronous hook installation is unavailable")
	_ = fs.Parse(args[1:])

	hookPath, err := hook.InstallPrePushHook(*repo, *async)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to install hook: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Pre-push hook installed at: %s\n", hookPath)
	fmt.Println("   Mode: SYNC (verifies the pushed commit SHAs in isolated worktrees)")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// offlineStore opens the daemon's mirrored job records for the CLI paths that
// must still answer when no daemon is up. It wraps a throwaway memory store:
// nothing on this path writes.
func offlineStore() *store.DurableStore { return store.OpenReadOnly() }

// offlineBanner is printed to stderr, not stdout, so it never lands in the
// middle of output something else is parsing.
//
// The wording matters. A record mirrored while a job was queued or running
// keeps that status forever if the daemon died mid-flight, and reading
// "RUNNING" off a stopped daemon would send someone waiting for a result that
// is never coming. Recovery only rewrites those records when a daemon next
// starts (DurableStore.RecoverOrphans), which is exactly the moment this path
// is not taken.
func offlineBanner() {
	fmt.Fprintln(os.Stderr, "PushGuard daemon is not running — reading stored records from disk.")
	fmt.Fprintln(os.Stderr, "A job shown as QUEUED or RUNNING is not running now; it did not finish. Re-enqueue it.")
}

// statusFromDisk answers `pushguard status` with no daemon up.
//
// The daemon idles out after thirty minutes with an empty queue, which is
// routine rather than exceptional -- and "enqueue, then walk away" means the
// caller is, by design, back later. Refusing outright turned a stored result
// into "daemon is not running", which reads as a fault to fix rather than an
// answer to collect; a real session lost two gate results to that.
//
// Logs are read straight from the runner's log directory, so a failed gate
// still explains itself here. That is the whole reason to come back.
func statusFromDisk(jobID string) {
	job, err := offlineStore().Get(jobID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PushGuard daemon is not running, and there is no stored record for %s.\n", jobID)
		fmt.Fprintln(os.Stderr, "Records are pruned 48 hours after a job finishes; start the daemon with `pushguard daemon` for live jobs.")
		os.Exit(1)
	}
	offlineBanner()
	fmt.Println()

	fmt.Printf("Job:      %s\n", job.ID)
	fmt.Printf("Status:   %s\n", strings.ToUpper(string(job.Status)))
	fmt.Printf("Repo:     %s\n", job.RepoPath)
	fmt.Printf("Branch:   %s\n", job.Branch)
	publicJob := application.Snapshot(job)
	printProvenance(publicJob)
	fmt.Printf("Ref:      %s\n", shortRef(publicJob))
	fmt.Printf("Command:  %s\n", strings.Join(job.Command, " "))
	fmt.Printf("Duration: %s\n", job.DurationString())
	if job.ExitCode != 0 {
		fmt.Printf("Exit:     %d\n", job.ExitCode)
	}
	if job.Error != "" {
		fmt.Printf("Error:    %s\n", job.Error)
	}

	logs := readJobLog(job.ID)
	if logs == "" {
		return
	}
	if failure := core.ExtractFailure(logs, 30); failure != "" && job.Status != core.StatusPassed {
		if names := core.FailureNames(logs); len(names) > 0 {
			fmt.Printf("\n--- %d failing test(s): %s\n", len(names), strings.Join(names, ", "))
		}
		fmt.Println("\n--- Why it failed (excerpt; read the full log for the rest) ---")
		fmt.Println(failure)
	}
	fmt.Println("\n--- Logs (last 40 lines) ---")
	fmt.Println(tailLines(logs, 40))
}

// readJobLog reads a job's log file directly. The runner's default location is
// duplicated rather than shared because the runner creates its directory on
// construction, and a read-only status call must not.
func readJobLog(jobID string) string {
	base := os.Getenv("PUSHGUARD_LOG_DIR")
	if base == "" && os.Getenv("PUSHGUARD_STATE_DIR") != "" {
		base = filepath.Join(os.Getenv("PUSHGUARD_STATE_DIR"), "logs")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache", "pushguard", "logs")
	}
	data, err := os.ReadFile(filepath.Join(base, jobID+".log"))
	if err != nil {
		return ""
	}
	return string(data)
}

// listFromDisk answers `pushguard list` with no daemon up.
//
// An empty directory still exits 1: with nothing to show and no daemon, "the
// daemon is down" is the only true answer, and printing an empty table would
// claim the queue is empty when it is merely unreadable.
func listFromDisk(limit int, ref string) {
	jobs := offlineStore().ListFromDisk(0)
	if ref != "" {
		filtered := jobs[:0]
		for _, job := range jobs {
			if job.Ref == ref || job.CommitSHA == ref || job.RequestedRef == ref || job.BaseSHA == ref {
				filtered = append(filtered, job)
			}
		}
		jobs = filtered
	}
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running, and no stored job records were found.")
		fmt.Fprintln(os.Stderr, "Start it with `pushguard daemon` (or `make pushguard-start`).")
		os.Exit(1)
	}
	offlineBanner()
	fmt.Println()
	publicJobs := make([]api.Job, 0, len(jobs))
	for _, job := range jobs {
		publicJobs = append(publicJobs, application.Snapshot(job))
	}
	application.SortForQueueView(publicJobs)
	renderJobTable(publicJobs)
}

// Exit codes for a finished job. Only a gate that actually passed exits 0.
//
//	0  passed
//	1  failed — the checks ran and something was red
//	2  timed out waiting (await only; the job may still be running)
//	3  no verdict — cancelled, or interrupted before completion.
//
// The third code is the point. A cancelled job records ExitCode 0 for the
// mundane reason that no process ever returned one, and both `enqueue --wait`
// and a daemon that vanished mid-await used to pass that straight through. A
// shell caller branching on $?, or an agent reading the exit status, then read
// an unrun gate as a clean one -- the worst possible direction for a tool whose
// whole job is to say whether the checks passed. A peer nearly reported a gate
// as green on exactly this, having cancelled it themselves seconds earlier.
//
// A failed job always exits 1 rather than forwarding the runner's own code,
// which could otherwise collide with 2 or 3 and turn a red gate into "timed
// out" or "never ran". The runner's exit code is printed in the output, where
// it is unambiguous.
const (
	exitJobFailed    = 1
	exitAwaitTimeout = 2
	exitJobNoVerdict = 3
)

func exitCodeFor(job api.Job) int {
	switch job.Status {
	case api.StatusPassed:
		return 0
	case api.StatusCancelled:
		return exitJobNoVerdict
	default:
		return exitJobFailed
	}
}

// reportFinishedJob prints a finished job's verdict, its failure excerpt, and
// then the verdict AGAIN as the final line.
//
// The repetition is deliberate. An agent's harness reports the exit status of
// the whole shell command, so `await ... ; echo done` or `await ... | tail -20`
// reports 0 for a red gate -- the trailing command's status replaces await's.
// The reader then falls back on the output, and the output used to end in
// several screens of "--- Why it failed ---" test noise with the single FAILED
// line scrolled off the top. Two sessions each misread a red gate as green in
// one evening on exactly that.
//
// A terminal verdict line means a tailed, truncated or piped read still ends on
// the answer. One line, and it closes the gap the exit code alone could not.
func reportFinishedJob(job api.Job, logs string) {
	printProvenance(job)
	fmt.Printf("%s (exit %d) in %s\n", strings.ToUpper(string(job.Status)), job.ExitCode, job.DurationString())
	// A passed job has nothing to explain, even if its logs print errors.
	if logs != "" && job.Status != api.StatusPassed {
		// Name every failing test before showing the excerpt. ExtractFailure is
		// a bounded window that drops from the front, so on a run with several
		// failures it shows the last of them and says nothing about the rest —
		// two readers of one job once came away with different subsets, each
		// believing they had the whole.
		if names := core.FailureNames(logs); len(names) > 0 {
			fmt.Printf("\n--- %d failing test(s): %s\n", len(names), strings.Join(names, ", "))
		}
		if failure := core.ExtractFailure(logs, 30); failure != "" {
			fmt.Println("\n--- Why it failed (excerpt; read the full log for the rest) ---")
			fmt.Println(failure)
		}
	}
	fmt.Printf("\n=== GATE %s === job %s exited %d\n",
		strings.ToUpper(string(job.Status)), job.ID, exitCodeFor(job))
}

func printProvenance(job api.Job) {
	kind := job.SourceKind
	if kind == "" {
		kind = "legacy (provenance not recorded)"
	}
	fmt.Printf("Tested ref: %s | source: %s | requested ref: %s | base SHA: %s\n", job.Ref, kind, job.RequestedRef, job.BaseSHA)
}
