package core

import (
	"crypto/rand"
	"encoding/hex"
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
	return j.Status == StatusPassed || j.Status == StatusFailed || j.Status == StatusCancelled
}

// DurationString returns human-readable runtime of the job.
func (j *Job) DurationString() string {
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
