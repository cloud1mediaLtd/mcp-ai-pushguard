package core

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
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

// DetectorFunc analyzes a directory to detect an appropriate test command.
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
	MaxConcurrent int
	DefaultTimeout time.Duration
	BaseWorktreeDir string
	BaseLogDir string
}

// Engine orchestrates background testing in isolated git worktrees.
type Engine struct {
	config        EngineConfig
	store         StoreInterface
	worktrees     *git.WorktreeManager
	runner        RunnerInterface
	notifier      NotifierInterface
	detectCommand DetectorFunc

	queue         chan *Job
	cancels       map[string]context.CancelFunc
	cancelsMu     sync.Mutex
	stopChan      chan struct{}
	wg            sync.WaitGroup

	// lastActivity is a UnixNano timestamp, updated whenever a job is
	// enqueued or finishes. It backs the daemon's idle-shutdown timer.
	lastActivity atomic.Int64
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

// ActiveJobCount returns the number of jobs currently queued or running.
func (e *Engine) ActiveJobCount() int {
	e.cancelsMu.Lock()
	running := len(e.cancels)
	e.cancelsMu.Unlock()
	return running + len(e.queue)
}

// Start launches the worker pool.
func (e *Engine) Start() {
	for i := 0; i < e.config.MaxConcurrent; i++ {
		e.wg.Add(1)
		go e.workerLoop(i)
	}
}

// Stop signals workers to stop and waits for in-flight tasks.
func (e *Engine) Stop() {
	close(e.stopChan)
	e.wg.Wait()
}

// Enqueue validates the request, inspects git state, schedules the job, and returns its queue position.
func (e *Engine) Enqueue(req EnqueueRequest) (*Job, int, error) {
	if req.RepoPath == "" {
		req.RepoPath = "."
	}
	repoRoot, err := git.GetRepoRoot(req.RepoPath)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid git repository at %q: %w", req.RepoPath, err)
	}

	branch, _ := git.GetCurrentBranch(repoRoot)
	headSHA, _ := git.GetHeadSHA(repoRoot)

	targetRef := req.Ref
	if targetRef == "" || targetRef == "HEAD" {
		// Check for uncommitted work-in-progress changes
		dirty, _ := git.HasUncommittedChanges(repoRoot)
		if dirty {
			stashSHA, err := git.CreateDirtySnapshotCommit(repoRoot)
			if err == nil && stashSHA != "" {
				targetRef = stashSHA
			} else {
				targetRef = headSHA
			}
		} else {
			targetRef = headSHA
		}
	}

	// Auto-detect command if unspecified
	cmd := req.Command
	if len(cmd) == 0 && e.detectCommand != nil {
		cmd = e.detectCommand(repoRoot)
	}
	if len(cmd) == 0 {
		cmd = []string{"echo", "No tests defined"}
	}

	name := req.Name
	if name == "" {
		name = filepath.Base(repoRoot)
		if branch != "" {
			name = fmt.Sprintf("%s (%s)", name, branch)
		}
	}

	job := &Job{
		ID:        GenerateJobID(),
		Name:      name,
		RepoPath:  repoRoot,
		Ref:       targetRef,
		CommitSHA: headSHA,
		Branch:    branch,
		Command:   cmd,
		AutoPush:  req.AutoPush,
		Status:    StatusQueued,
		CreatedAt: time.Now(),
	}

	if err := e.store.Save(job); err != nil {
		return nil, 0, fmt.Errorf("failed to save job: %w", err)
	}

	select {
	case e.queue <- job:
		e.touchActivity()
		position := len(e.queue)
		return job, position, nil
	default:
		return nil, 0, fmt.Errorf("queue is full")
	}
}

func (e *Engine) workerLoop(workerID int) {
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
	// If already cancelled while waiting in queue
	if job.IsFinished() {
		return
	}

	job.setRunning(time.Now())
	_ = e.store.Update(job)

	ctx, cancel := context.WithTimeout(context.Background(), e.config.DefaultTimeout)
	e.cancelsMu.Lock()
	e.cancels[job.ID] = cancel
	e.cancelsMu.Unlock()

	defer func() {
		cancel()
		e.cancelsMu.Lock()
		delete(e.cancels, job.ID)
		e.cancelsMu.Unlock()
	}()

	// 1. Create worktree
	worktreeDir, err := e.worktrees.CreateWorktree(job.RepoPath, job.ID, job.Ref)
	if err != nil {
		job.setFailed(fmt.Sprintf("worktree setup failed: %s", err))
		e.finalizeJob(job)
		return
	}
	job.setWorktreeDir(worktreeDir)

	// Guarantee worktree removal
	defer func() {
		_ = e.worktrees.RemoveWorktree(job.RepoPath, worktreeDir)
	}()

	// 2. Run test execution
	exitCode, runErr := e.runner.Run(ctx, job, worktreeDir)

	switch {
	case ctx.Err() == context.Canceled:
		job.setResult(exitCode, StatusCancelled, "job was cancelled")
	case ctx.Err() == context.DeadlineExceeded:
		job.setResult(exitCode, StatusFailed, "test run timed out")
	case exitCode == 0 && runErr == nil:
		job.setResult(exitCode, StatusPassed, "")

		// Auto-push if requested and branch is valid
		if job.AutoPush && job.Branch != "" && job.Branch != "HEAD" {
			pushErr := git.Push(job.RepoPath, job.Branch)
			if pushErr != nil {
				job.AppendLog(fmt.Sprintf("[pushguard] auto-push failed: %s", pushErr))
			} else {
				job.AppendLog(fmt.Sprintf("[pushguard] successfully pushed %s to origin", job.Branch))
			}
		}
	default:
		errMsg := ""
		if runErr != nil {
			errMsg = runErr.Error()
		}
		job.setResult(exitCode, StatusFailed, errMsg)
	}

	e.finalizeJob(job)
}

func (e *Engine) finalizeJob(job *Job) {
	job.finalize(time.Now())

	_ = e.store.Update(job)
	e.touchActivity()

	if e.notifier != nil {
		_ = e.notifier.Notify(job)
	}
}

// Cancel cancels a running or queued job.
func (e *Engine) Cancel(jobID string) error {
	job, err := e.store.Get(jobID)
	if err != nil {
		return err
	}

	e.cancelsMu.Lock()
	cancel, ok := e.cancels[jobID]
	e.cancelsMu.Unlock()

	if ok {
		cancel()
	}

	if !job.setCancelled("cancelled by user") {
		// Already in a terminal state — e.g. it passed/failed a moment ago.
		// Report that instead of clobbering a real result with "cancelled".
		return fmt.Errorf("job %s is already finished", jobID)
	}
	_ = e.store.Update(job)
	return nil
}

// GetJob fetches a job by ID.
func (e *Engine) GetJob(id string) (*Job, error) {
	return e.store.Get(id)
}

// ListJobs returns the most recent jobs.
func (e *Engine) ListJobs(limit int) []*Job {
	return e.store.List(limit)
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
			return job, nil
		}

		select {
		case <-ctx.Done():
			return job, ctx.Err()
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

