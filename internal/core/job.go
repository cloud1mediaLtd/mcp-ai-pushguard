package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// JobStatus represents the lifecycle state of a test job.
type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusPassed    JobStatus = "passed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

// Job represents a queued or executed verification run.
type Job struct {
	ID          string        `json:"id"`
	Name        string        `json:"name,omitempty"`
	RepoPath    string        `json:"repo_path"`
	Ref         string        `json:"ref"`
	CommitSHA   string        `json:"commit_sha,omitempty"`
	Branch      string        `json:"branch,omitempty"`
	Command     []string      `json:"command"`
	AutoPush    bool          `json:"auto_push"`
	Status      JobStatus     `json:"status"`
	ExitCode    int           `json:"exit_code"`
	Error       string        `json:"error,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	StartedAt   *time.Time    `json:"started_at,omitempty"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Duration    time.Duration `json:"duration_ns,omitempty"`
	WorktreeDir string        `json:"worktree_dir,omitempty"`
	LogFilePath string        `json:"log_file_path,omitempty"`

	mu   sync.RWMutex
	logs []string
}

// EnqueueRequest encapsulates parameters to schedule a new test job.
type EnqueueRequest struct {
	RepoPath string        `json:"repo_path"`
	Ref      string        `json:"ref,omitempty"`
	Command  []string      `json:"command,omitempty"`
	Name     string        `json:"name,omitempty"`
	AutoPush bool          `json:"auto_push,omitempty"`
	Timeout  time.Duration `json:"timeout_seconds,omitempty"`
}

// GenerateJobID generates a unique identifier for a job: e.g. pg_a1b2c3d4e5
func GenerateJobID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("pg_%d", time.Now().UnixNano()%1000000)
	}
	return "pg_" + hex.EncodeToString(b)
}

// IsFinished returns true if the job has completed, failed, or was cancelled.
func (j *Job) IsFinished() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.isFinishedLocked()
}

func (j *Job) isFinishedLocked() bool {
	return j.Status == StatusPassed || j.Status == StatusFailed || j.Status == StatusCancelled
}

// DurationString returns human-readable runtime of the job.
func (j *Job) DurationString() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.Duration > 0 {
		return j.Duration.Round(time.Millisecond).String()
	}
	if j.StartedAt != nil {
		if j.FinishedAt != nil {
			return j.FinishedAt.Sub(*j.StartedAt).Round(time.Millisecond).String()
		}
		return time.Since(*j.StartedAt).Round(time.Second).String()
	}
	return "0s"
}

// MarshalJSON takes a read lock before encoding so a job's fields can never
// be read mid-write — the concrete bug this guards against: a worker
// goroutine setting Status/ExitCode/etc. in executeJob while an HTTP handler
// concurrently JSON-encodes the same *Job for a status/list response. The
// jobAlias local type strips Job's methods (this one included) so
// json.Marshal doesn't recurse into MarshalJSON forever.
func (j *Job) MarshalJSON() ([]byte, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	type jobAlias Job
	return json.Marshal((*jobAlias)(j))
}

// setRunning marks the job started. Caller (Engine, same package) must not
// hold any lock of its own — this acquires job.mu itself.
func (j *Job) setRunning(startedAt time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.StartedAt = &startedAt
	j.Status = StatusRunning
}

// setWorktreeDir records where the job's isolated checkout lives.
func (j *Job) setWorktreeDir(dir string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.WorktreeDir = dir
}

// setFailed marks the job failed with a message, without touching ExitCode
// (used for failures — like worktree setup — that never got far enough to
// produce one).
func (j *Job) setFailed(errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = StatusFailed
	j.Error = errMsg
}

// setResult records the outcome of an actually-executed command: its exit
// code, and the terminal status/error derived from that exit code and the
// run context (cancelled, timed out, or genuinely passed/failed).
func (j *Job) setResult(exitCode int, status JobStatus, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ExitCode = exitCode
	j.Status = status
	j.Error = errMsg
}

// setCancelled marks the job cancelled with a reason, refusing to override a
// job that already reached a terminal state (this is also used by the
// public Cancel path, where the job may have finished on its own between
// the caller's IsFinished check and this call).
func (j *Job) setCancelled(reason string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.isFinishedLocked() {
		return false
	}
	j.Status = StatusCancelled
	j.Error = reason
	return true
}

// finalize records completion time and, if the job ever started, its total
// duration. Returns the finish time so callers that need it (e.g. to hand a
// FinishedAt to the store) don't have to re-lock to read it back.
func (j *Job) finalize(finishedAt time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.FinishedAt = &finishedAt
	if j.StartedAt != nil {
		j.Duration = finishedAt.Sub(*j.StartedAt)
	}
}

// MarkOrphaned marks a job that on-disk state says was queued or running as
// cancelled, with an explanatory reason and a finish timestamp. Exported
// (unlike the set* helpers above) because it's used by callers outside this
// package that recover jobs left behind by a crashed or restarted daemon —
// DurableStore.RecoverOrphans reads jobs back from disk before any engine
// exists to route the mutation through, so it needs a safe way to touch a
// *Job's fields without reaching into its private lock. Refuses to overwrite
// a job that's already terminal (returns false), matching the same guard as
// the in-process Cancel path.
func (j *Job) MarkOrphaned(reason string, finishedAt time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.isFinishedLocked() {
		return false
	}
	j.Status = StatusCancelled
	j.Error = reason
	j.FinishedAt = &finishedAt
	return true
}

// MarkInterrupted overwrites an already-cancelled job's error message to
// explain that a daemon shutdown (not a person) caused the cancellation, and
// backfills FinishedAt if a prior Cancel call somehow didn't set one. Unlike
// MarkOrphaned this deliberately does overwrite a terminal job: it exists
// for the exact sequence "Cancel a running job during shutdown, then restate
// why" — without the second step, a caller who walked away sees a bare
// "cancelled" that reads as intentional.
func (j *Job) MarkInterrupted(reason string, finishedAt time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = StatusCancelled
	j.Error = reason
	if j.FinishedAt == nil {
		j.FinishedAt = &finishedAt
	}
}

// AppendLog adds a log line to memory in a concurrency-safe manner.
func (j *Job) AppendLog(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.logs = append(j.logs, line)
	if len(j.logs) > 2000 {
		j.logs = j.logs[len(j.logs)-2000:]
	}
}

// TailLogs returns the last n log lines.
func (j *Job) TailLogs(n int) string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if len(j.logs) == 0 {
		return ""
	}
	start := 0
	if len(j.logs) > n {
		start = len(j.logs) - n
	}
	return strings.Join(j.logs[start:], "\n")
}
