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

	// Refuse before committing to a long run rather than dying part-way
	// through it. A worktree is a full checkout, and an ENOSPC mid-gate does
	// not surface as a disk error — the Go build cache starts evicting entries
	// it is writing and the run fails with a toolchain error that points
	// nowhere near the real cause.
	if warning, err := core.CheckDiskSpace(worktreeBaseDir()); err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to enqueue: %s\n", err)
		os.Exit(1)
	} else if warning != "" {
		fmt.Fprintf(os.Stderr, "⚠️  %s\n", warning)
	}

	c := client.New("")

	// Try to get (or auto-start) the shared daemon so this job coordinates
	// with everything else on the machine instead of running in isolation.
	if err := c.EnsureDaemon(client.DefaultConcurrency()); err != nil {
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
		os.Exit(exitCodeFor(finishedJob))
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
		listFromDisk()
		return
	}

	jobs, err := c.ListJobs(30)
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
	core.SortForQueueView(jobs)
	renderJobTable(jobs)
}

// renderJobTable prints the queue view. Split out of handleList so the
// offline path below renders identically -- a second table that drifted from
// this one would be worse than no offline listing at all.
func renderJobTable(jobs []*core.Job) {
	fmt.Printf("%-14s %-10s %-4s %-10s %-24s %-10s %s\n", "JOB ID", "STATUS", "POS", "REF", "REPOSITORY", "DURATION", "COMMAND")
	fmt.Println(strings.Repeat("-", 108))
	position := 0
	for _, j := range jobs {
		// Position is only meaningful for work that has not started. A number
		// beside a finished job would read as history rather than a queue.
		pos := "-"
		if j.Status == core.StatusQueued {
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

// shortRef is the commit a job is actually testing.
//
// Job.Ref is that commit: the ref the caller asked for, or the resolved HEAD or
// working-tree snapshot when they asked for neither. Job.CommitSHA is something
// else entirely — the branch tip at the moment of enqueue, recorded regardless
// of --ref.
//
// Preferring CommitSHA, as this first did, reports the tip for a job that is
// deliberately gating an older SHA. That is the dangerous direction: it tells
// you a release gate covered the tip when it covered a commit from before the
// fix you are relying on. Caught when a peer enqueued --ref d7ab0d01a, saw the
// enqueue echo it back correctly, and then saw this column claim 635ee5063.
func shortRef(j *core.Job) string {
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
	// Split the job id out BEFORE parsing flags.
	//
	// Go's flag package stops at the first non-flag argument, so
	// `await <job-id> --timeout 2400` parsed nothing and silently used the
	// default — which is the order this command's own usage string documents,
	// and it cost a 900-second wait that reported a timeout on a job that was
	// merely queued behind others.
	//
	// Accepting either order is the fix; a positional-then-flag command line is
	// what anyone would type.
	jobID, flags := splitPositional(args)
	fs := flag.NewFlagSet("await", flag.ExitOnError)
	timeoutSec := fs.Int("timeout", 900, "Seconds to wait before giving up")
	_ = fs.Parse(flags)
	if jobID == "" {
		// Flags came first, so the parser reached the operand itself.
		jobID = fs.Arg(0)
	}
	if jobID == "" {
		fmt.Fprintln(os.Stderr, "Usage: pushguard await <job-id> [--timeout <seconds>]")
		os.Exit(1)
	}

	c := client.New("")
	if !c.IsDaemonRunning() {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running.")
		os.Exit(1)
	}

	deadline := time.Now().Add(time.Duration(*timeoutSec) * time.Second)
	for {
		job, err := c.GetJob(jobID)
		if err != nil {
			// The daemon can go away mid-wait (idle shutdown racing the last
			// job, a crash, someone rebuilding). Its last mirrored record is
			// still on disk and may well be the finished verdict we are here
			// for, so read that before giving up.
			if stored, storeErr := store.OpenReadOnly().Get(jobID); storeErr == nil && stored.IsFinished() {
				job = stored
			} else {
				fmt.Fprintf(os.Stderr, "Error: %s\n", err)
				// Not a failed gate: nothing was decided. Exiting 1 here would
				// be indistinguishable from a red run.
				os.Exit(exitJobNoVerdict)
			}
		}
		if job.IsFinished() {
			fmt.Printf("%s (exit %d) in %s\n", strings.ToUpper(string(job.Status)), job.ExitCode, job.DurationString())
			if logs, err := c.GetLogs(jobID, 400); err == nil {
				if failure := core.ExtractFailure(logs, 30); failure != "" {
					fmt.Println("\n--- Why it failed ---")
					fmt.Println(failure)
				}
			}
			// Exit non-zero on a red job so a shell caller can branch on it,
			// and give "no verdict" its own code so it cannot read as either.
			if job.Status != core.StatusPassed {
				os.Exit(exitCodeFor(job))
			}
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "Timed out after %ds; job %s is still %s.\n", *timeoutSec, jobID, job.Status)
			os.Exit(exitAwaitTimeout)
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
// worktreeBaseDir is where worktrees are created, and so the filesystem whose
// free space actually matters. Mirrors git.NewWorktreeManager's default.
func worktreeBaseDir() string {
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

func handleDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socketPath := fs.String("socket", daemon.DefaultSocketPath(), "Path to Unix socket")
	httpAddr := fs.String("http", "", "Optional HTTP address to bind (e.g. 127.0.0.1:8942)")
	// Default from PUSHGUARD_CONCURRENCY so the value survives the daemon's
	// idle-shutdown and the auto-respawn that follows it; an explicit flag
	// still wins for a hand-started daemon.
	concurrency := fs.Int("concurrency", client.DefaultConcurrency(), "Max concurrent test workers (default from PUSHGUARD_CONCURRENCY)")
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
				jobRecordsPruned, err := jobStore.PruneOld(48 * time.Hour)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[pushguard] scheduled job-record prune failed: %s\n", err)
				}
				if worktreesPruned > 0 || logsPruned > 0 || jobRecordsPruned > 0 {
					fmt.Printf("[pushguard] scheduled disk prune: worktrees pruned=%d, logs pruned=%d, job records pruned=%d\n",
						worktreesPruned, logsPruned, jobRecordsPruned)
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
		job.MarkInterrupted("daemon shut down while this job was in flight; it did not run to completion — re-enqueue it", time.Now())
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
	fmt.Printf("Ref:      %s\n", shortRef(job))
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
	if failure := core.ExtractFailure(logs, 30); failure != "" {
		fmt.Println("\n--- Why it failed ---")
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
func listFromDisk() {
	jobs := offlineStore().ListFromDisk(30)
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "PushGuard daemon is not running, and no stored job records were found.")
		fmt.Fprintln(os.Stderr, "Start it with `pushguard daemon` (or `make pushguard-start`).")
		os.Exit(1)
	}
	offlineBanner()
	fmt.Println()
	core.SortForQueueView(jobs)
	renderJobTable(jobs)
}

// Exit codes for a finished job. Only a gate that actually passed exits 0.
//
//	0  passed
//	1  failed — the checks ran and something was red
//	2  timed out waiting (await only; the job may still be running)
//	3  no verdict — cancelled, or orphaned by a daemon restart. Nothing ran.
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

func exitCodeFor(job *core.Job) int {
	switch job.Status {
	case core.StatusPassed:
		return 0
	case core.StatusCancelled:
		return exitJobNoVerdict
	default:
		return exitJobFailed
	}
}
