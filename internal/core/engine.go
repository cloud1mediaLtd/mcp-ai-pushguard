package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pushguard/pushguard/internal/git"
)

// RunnerInterface abstracts running tests in a worktree.
type RunnerInterface interface {
	Run(ctx context.Context, job *Job, workDir string) (int, error)
}

// LogPruner is an optional interface for runners that support log file pruning.
type LogPruner interface {
	PruneOldLogs(maxAge time.Duration) (int, error)
}

// DetectorFunc analyzes selected-commit root metadata to detect a test command.
type DetectorFunc func(dir string) []string

// NotifierInterface alerts upon job termination.
type NotifierInterface interface {
	Notify(job *Job) error
}

// StoreInterface abstracts storage.
type StoreInterface interface {
	Save(job *Job) error
	Get(id string) (*Job, error)
	List(limit int) []*Job
	Update(job *Job) error
	Clear(completedOnly bool) int
}

// EngineConfig configures the test queue engine.
type EngineConfig struct {
	MaxConcurrent   int
	DefaultTimeout  time.Duration
	BaseWorktreeDir string
	BaseLogDir      string
	Build           string
}

// Engine orchestrates background testing in isolated git worktrees.
type Engine struct {
	config        EngineConfig
	store         StoreInterface
	worktrees     *git.WorktreeManager
	runner        RunnerInterface
	notifier      NotifierInterface
	detectCommand DetectorFunc

	queue     chan *Job
	cancels   map[string]context.CancelFunc
	contexts  map[string]context.Context
	draining  bool
	started   bool
	stopOnce  sync.Once
	cancelsMu sync.Mutex
	stopChan  chan struct{}
	wg        sync.WaitGroup

	// lastActivity is a UnixNano timestamp, updated whenever a job is
	// enqueued or finishes. It backs the daemon's idle-shutdown timer.
	lastActivity atomic.Int64
	startedAt    atomic.Int64
}

// NewEngine creates and initializes a PushGuard engine.
func NewEngine(
	cfg EngineConfig,
	s StoreInterface,
	wm *git.WorktreeManager,
	r RunnerInterface,
	n NotifierInterface,
	df DetectorFunc,
) *Engine {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}

	e := &Engine{
		config:        cfg,
		store:         s,
		worktrees:     wm,
		runner:        r,
		notifier:      n,
		detectCommand: df,
		queue:         make(chan *Job, 200),
		cancels:       make(map[string]context.CancelFunc),
		contexts:      make(map[string]context.Context),
		stopChan:      make(chan struct{}),
	}
	e.lastActivity.Store(time.Now().UnixNano())
	return e
}

// touchActivity records that the engine did something, resetting the idle clock.
func (e *Engine) touchActivity() {
	e.lastActivity.Store(time.Now().UnixNano())
}

// IdleDuration reports how long it has been since a job was last enqueued or finished.
func (e *Engine) IdleDuration() time.Duration {
	return time.Since(time.Unix(0, e.lastActivity.Load()))
}

// ActiveJobCount includes every accepted job until its cleanup and finalization finish.
func (e *Engine) ActiveJobCount() int {
	e.cancelsMu.Lock()
	defer e.cancelsMu.Unlock()
	return len(e.cancels)
}

// TryDrainIdle atomically closes admission if the engine is idle long enough.
func (e *Engine) TryDrainIdle(idle time.Duration) bool {
	e.cancelsMu.Lock()
	defer e.cancelsMu.Unlock()
	if e.draining || len(e.cancels) != 0 || e.IdleDuration() < idle {
		return false
	}
	e.draining = true
	return true
}

// Start launches the worker pool at most once.
func (e *Engine) Start() {
	e.cancelsMu.Lock()
	defer e.cancelsMu.Unlock()
	if e.started || e.draining {
		return
	}
	e.started = true
	e.startedAt.CompareAndSwap(0, time.Now().UnixNano())
	for i := 0; i < e.config.MaxConcurrent; i++ {
		e.wg.Add(1)
		go e.workerLoop(i)
	}
}

// QueueDepth reports jobs waiting for a worker. It excludes a currently
// running job, which is included in ActiveJobCount.
func (e *Engine) QueueDepth() int { return len(e.queue) }

// Uptime reports how long workers have been available in this daemon process.
func (e *Engine) Uptime() time.Duration {
	started := e.startedAt.Load()
	if started == 0 {
		return 0
	}
	return time.Since(time.Unix(0, started))
}

// Stop closes admission, cancels accepted work, and waits for cleanup. It is idempotent.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		e.cancelsMu.Lock()
		e.draining = true
		for _, cancel := range e.cancels {
			cancel()
		}
		close(e.stopChan)
		e.cancelsMu.Unlock()
		e.wg.Wait()
		// Engines can be stopped without ever starting their workers.
		for {
			select {
			case job := <-e.queue:
				e.executeJob(job)
			default:
				return
			}
		}
	})
}

// Enqueue freezes the requested commit before publishing an accepted job.
func (e *Engine) Enqueue(req EnqueueRequest) (*Job, int, error) {
	if req.AutoPush {
		return nil, 0, fmt.Errorf("auto-push is disabled; push the verified commit explicitly")
	}
	if req.Snapshot && req.Ref != "" {
		return nil, 0, fmt.Errorf("snapshot and ref cannot be combined")
	}
	if req.Timeout < 0 {
		return nil, 0, fmt.Errorf("timeout cannot be negative")
	}
	if req.RepoPath == "" {
		req.RepoPath = "."
	}
	repoRoot, err := git.GetRepoRoot(req.RepoPath)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid git repository at %q: %w", req.RepoPath, err)
	}
	branch, err := git.GetCurrentBranch(repoRoot)
	if err != nil {
		return nil, 0, err
	}
	baseSHA, err := git.ResolveCommit(repoRoot, "HEAD")
	if err != nil {
		return nil, 0, err
	}
	requestedRef := req.Ref
	if requestedRef == "" {
		requestedRef = "HEAD"
	}
	targetRef := baseSHA
	if requestedRef != "HEAD" {
		targetRef, err = git.ResolveCommit(repoRoot, requestedRef)
		if err != nil {
			return nil, 0, err
		}
	}
	sourceKind := "commit"
	if req.Snapshot {
		sourceKind = "snapshot"
		snapshot, err := git.CreateSnapshotCommit(repoRoot, baseSHA)
		if err != nil {
			return nil, 0, fmt.Errorf("create working-tree snapshot: %w", err)
		}
		if snapshot != "" {
			targetRef = snapshot
		}
	}
	cmd := append([]string(nil), req.Command...)
	if len(cmd) == 0 && e.detectCommand != nil {
		detectionDir, err := git.DetectionDir(repoRoot, targetRef)
		if err != nil {
			return nil, 0, err
		}
		cmd = append([]string(nil), e.detectCommand(detectionDir)...)
		if err := os.RemoveAll(detectionDir); err != nil {
			return nil, 0, fmt.Errorf("remove temporary verification metadata: %w", err)
		}
	}
	if len(cmd) == 0 || strings.TrimSpace(cmd[0]) == "" {
		return nil, 0, fmt.Errorf("no verification command found; provide an explicit command")
	}
	name := req.Name
	if name == "" {
		name = filepath.Base(repoRoot)
		if branch != "" {
			name = fmt.Sprintf("%s (%s)", name, branch)
		}
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = e.config.DefaultTimeout
	}
	snapshotBase := ""
	if req.Snapshot {
		snapshotBase = baseSHA
	}
	dedupeKey := ""
	if !req.Snapshot {
		dedupeKey = strings.TrimSpace(req.DedupeKey)
	}
	if dedupeKey == "" && !req.Snapshot {
		// The default identity is deliberately exact: same checkout, immutable
		// commit, and command. Snapshot jobs always run independently because
		// their generated commit is the only trustworthy identity.
		dedupeKey = repoRoot + "\x00" + targetRef + "\x00" + strings.Join(cmd, "\x00")
	}
	job := &Job{ID: GenerateJobID(), Name: name, RepoPath: repoRoot, Ref: targetRef,
		RequestedRef: requestedRef, SourceKind: sourceKind, BaseSHA: snapshotBase,
		CommitSHA: targetRef, Branch: branch, Command: cmd, Timeout: timeout,
		Status: StatusQueued, CreatedAt: time.Now(), DaemonBuild: e.config.Build, DedupeKey: dedupeKey}
	// Admission, persistence and publication share a lock with idle shutdown.
	e.cancelsMu.Lock()
	defer e.cancelsMu.Unlock()
	if e.draining {
		return nil, 0, fmt.Errorf("engine is draining; retry with a running daemon")
	}
	if dedupeKey != "" {
		for _, existing := range e.store.List(0) {
			if existing.RepoPath != repoRoot || existing.DedupeKey != dedupeKey || existing.IsFinished() {
				continue
			}
			reused := existing.Snapshot()
			reused.Reused = true
			return reused, 0, nil
		}
	}
	if len(e.queue) == cap(e.queue) {
		return nil, 0, fmt.Errorf("queue is full")
	}
	if err := e.store.Save(job); err != nil {
		return nil, 0, fmt.Errorf("save queued job: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancels[job.ID] = cancel
	e.contexts[job.ID] = ctx
	e.queue <- job
	e.touchActivity()
	return job.Snapshot(), len(e.queue), nil
}

func (e *Engine) workerLoop(_ int) {
	defer e.wg.Done()
	for {
		select {
		case <-e.stopChan:
			return
		case job := <-e.queue:
			e.executeJob(job)
		}
	}
}

func (e *Engine) executeJob(job *Job) {
	e.cancelsMu.Lock()
	parent, ok := e.contexts[job.ID]
	if !ok {
		e.cancelsMu.Unlock()
		return
	} // Queued cancellation already finalized.
	if parent.Err() != nil {
		message := "job was cancelled"
		if e.draining {
			message = "engine shut down while this job was queued; re-enqueue it"
		}
		e.finishLocked(job, 130, StatusCancelled, message)
		e.cancelsMu.Unlock()
		e.notify(job)
		return
	}
	job.setRunning(time.Now())
	if err := e.store.Update(job); err != nil {
		e.finishLocked(job, 1, StatusFailed, "persist running job: "+err.Error())
		e.cancelsMu.Unlock()
		e.notify(job)
		return
	}
	e.cancelsMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, job.Timeout)
	defer cancel()
	worktreeDir, setupErr := e.worktrees.CreateWorktreeContext(ctx, job.RepoPath, job.ID, job.Ref)
	exitCode := 1
	var runErr error
	if setupErr == nil {
		job.setWorktreeDir(worktreeDir)
		if err := e.store.Update(job); err != nil {
			runErr = fmt.Errorf("persist worktree location: %w", err)
		} else {
			exitCode, runErr = e.runner.Run(ctx, job, worktreeDir)
		}
		if err := e.worktrees.RemoveWorktree(job.RepoPath, worktreeDir); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("worktree cleanup failed: %w", err))
		}
	} else {
		runErr = fmt.Errorf("worktree setup failed: %w", setupErr)
	}
	// A terminal result is published only after the checkout has been cleaned.
	e.cancelsMu.Lock()
	status, message := StatusFailed, ""
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		status, message = StatusCancelled, "job was cancelled"
		if e.draining {
			message = "engine shut down while this job was running; re-enqueue it"
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		message = "test run timed out"
	case exitCode == 0 && runErr == nil:
		status = StatusPassed
	case runErr != nil:
		message = runErr.Error()
	}
	if runErr != nil && message != runErr.Error() {
		message += "; " + runErr.Error()
	}
	e.finishLocked(job, exitCode, status, message)
	e.cancelsMu.Unlock()
	e.notify(job)
}

// finishLocked is the single terminal transition; callers hold cancelsMu.
func (e *Engine) finishLocked(job *Job, exitCode int, status JobStatus, message string) {
	result := job.Snapshot()
	result.setResult(exitCode, status, message)
	result.finalize(time.Now())
	if err := e.store.Update(result); err != nil {
		job.setResult(1, StatusFailed, "persist terminal result: "+err.Error())
		job.finalize(time.Now())
		slog.Error("failed to persist job result", "job_id", job.ID, "error", err)
	} else {
		job.setResult(exitCode, status, message)
		job.finalize(*result.FinishedAt)
	}
	if cancel := e.cancels[job.ID]; cancel != nil {
		cancel()
	}
	delete(e.cancels, job.ID)
	delete(e.contexts, job.ID)
	e.touchActivity()
}

func (e *Engine) notify(job *Job) {
	if e.notifier != nil {
		if err := e.notifier.Notify(job.Snapshot()); err != nil {
			slog.Warn("job notification failed", "job_id", job.ID, "error", err)
		}
	}
}

// Cancel requests cancellation. Running jobs finish only after process and worktree cleanup.
func (e *Engine) Cancel(jobID string) error {
	e.cancelsMu.Lock()
	job, err := e.store.Get(jobID)
	if err != nil {
		e.cancelsMu.Unlock()
		return err
	}
	cancel, ok := e.cancels[jobID]
	if !ok || job.IsFinished() {
		e.cancelsMu.Unlock()
		return fmt.Errorf("job %s is already finished", jobID)
	}
	cancel()
	queued := job.StatusValue() == StatusQueued
	if queued {
		e.finishLocked(job, 130, StatusCancelled, "cancelled by user")
	}
	e.cancelsMu.Unlock()
	if queued {
		e.notify(job)
	}
	return nil
}

// GetJob fetches a job by ID.
func (e *Engine) GetJob(id string) (*Job, error) {
	job, err := e.store.Get(id)
	if err != nil {
		return nil, err
	}
	return job.Snapshot(), nil
}

// ListJobs returns the most recent jobs.
func (e *Engine) ListJobs(limit int) []*Job {
	jobs := e.store.List(limit)
	for i, job := range jobs {
		jobs[i] = job.Snapshot()
	}
	return jobs
}

// AwaitJob blocks until the job finishes or ctx expires.
func (e *Engine) AwaitJob(ctx context.Context, jobID string, pollInterval time.Duration) (*Job, error) {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		job, err := e.store.Get(jobID)
		if err != nil {
			return nil, err
		}
		if job.IsFinished() {
			return job.Snapshot(), nil
		}

		select {
		case <-ctx.Done():
			return job.Snapshot(), ctx.Err()
		case <-ticker.C:
		}
	}
}

// CleanReport summarizes resources pruned during queue maintenance.
type CleanReport struct {
	JobsFlushed     int    `json:"jobs_flushed"`
	WorktreesPruned int    `json:"worktrees_pruned"`
	LogsPruned      int    `json:"logs_pruned"`
	AllocBytesAfter uint64 `json:"alloc_bytes_after"`
}

// PruneDiskArtifacts removes stale on-disk state only — orphaned worktrees
// older than 1 hour and job logs older than 48 hours. Unlike Clean, it never
// touches the in-memory job store or forces GC, so it's safe to run on a
// fixed schedule even while the daemon is busy: a job someone queued an hour
// ago and hasn't checked on yet stays visible to pushguard_list/status. Clean
// is for winding down (idle-shutdown, or an explicit request); this is for
// keeping disk usage bounded on a day with no idle gaps at all.
func (e *Engine) PruneDiskArtifacts() (worktreesPruned, logsPruned int) {
	activeJobs := make(map[string]bool)
	e.cancelsMu.Lock()
	for id := range e.cancels {
		activeJobs[id] = true
	}
	e.cancelsMu.Unlock()

	if e.worktrees != nil {
		worktreesPruned, _ = e.worktrees.PruneOrphaned(activeJobs, 1*time.Hour)
	}
	if lp, ok := e.runner.(LogPruner); ok {
		logsPruned, _ = lp.PruneOldLogs(48 * time.Hour)
	}
	return worktreesPruned, logsPruned
}

// Clean prunes finished jobs from memory, removes stale worktrees, deletes old logs, and forces GC.
func (e *Engine) Clean() CleanReport {
	// 1. Identify active jobs
	activeJobs := make(map[string]bool)
	e.cancelsMu.Lock()
	for id := range e.cancels {
		activeJobs[id] = true
	}
	e.cancelsMu.Unlock()

	// 2. Prune orphaned worktrees older than 1 hour
	worktreesPruned := 0
	if e.worktrees != nil {
		worktreesPruned, _ = e.worktrees.PruneOrphaned(activeJobs, 1*time.Hour)
	}

	// 3. Prune old logs (> 48 hours)
	logsPruned := 0
	if lp, ok := e.runner.(LogPruner); ok {
		logsPruned, _ = lp.PruneOldLogs(48 * time.Hour)
	}

	// 4. Flush finished jobs from memory store
	flushed := e.store.Clear(true)

	// 5. Force garbage collection to reclaim memory immediately
	runtime.GC()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return CleanReport{
		JobsFlushed:     flushed,
		WorktreesPruned: worktreesPruned,
		LogsPruned:      logsPruned,
		AllocBytesAfter: m.Alloc,
	}
}
