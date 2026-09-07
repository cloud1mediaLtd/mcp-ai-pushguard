package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
  pushguard enqueue --repo ./c1m-server --cmd "go test ./..." --auto-push
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

	req := core.EnqueueRequest{
		RepoPath: *repo,
		Ref:      *ref,
		Command:  cmd,
		AutoPush: *autoPush,
		Timeout:  time.Duration(*timeoutSec) * time.Second,
	}

	c := client.New("")

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

	logs, err := c.GetLogs(jobID, 40)
	if err == nil && logs != "" {
		fmt.Println("\n--- Logs (last 40 lines) ---")
		fmt.Println(logs)
	}
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

func handleDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socketPath := fs.String("socket", daemon.DefaultSocketPath(), "Path to Unix socket")
	httpAddr := fs.String("http", "", "Optional HTTP address to bind (e.g. 127.0.0.1:8942)")
	concurrency := fs.Int("concurrency", 1, "Max concurrent test workers")
	_ = fs.Parse(args)

	memStore := store.NewMemoryStore(200)
	wm, err := git.NewWorktreeManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize worktrees: %s\n", err)
		os.Exit(1)
	}
	r, _ := runner.NewLocalRunner("")
	n := notifier.NewMultiNotifier(notifier.NewMacOSNotifier(), &notifier.TerminalBellNotifier{})

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

	// Wait for terminate signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down PushGuard daemon...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = server.Stop(ctx)
	engine.Stop()
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
