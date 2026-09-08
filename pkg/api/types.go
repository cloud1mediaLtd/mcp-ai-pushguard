// Package api defines the stable contracts shared by PushGuard clients and
// transports. It deliberately contains no daemon, filesystem, or runtime
// dependencies, so it can be used by external Go callers.
package api

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// ProtocolVersion is the version of the daemon HTTP submission contract.
	ProtocolVersion = 2
	// ProtocolHeader carries ProtocolVersion on mutating daemon requests.
	ProtocolHeader = "X-PushGuard-Protocol"
	// BuildHeader identifies the client binary submitting a mutating request.
	BuildHeader = "X-PushGuard-Build"
)

// JobStatus is the externally visible state of a verification job.
type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusPassed    JobStatus = "passed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

// Job is an immutable snapshot returned by the verification service. Runtime
// state such as cancellation functions and in-memory log buffers is never
// exposed through this contract.
type Job struct {
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	RepoPath     string        `json:"repo_path"`
	Ref          string        `json:"ref"`
	RequestedRef string        `json:"requested_ref,omitempty"`
	SourceKind   string        `json:"source_kind,omitempty"`
	BaseSHA      string        `json:"base_sha,omitempty"`
	Timeout      time.Duration `json:"timeout_ns,omitempty"`
	CommitSHA    string        `json:"commit_sha,omitempty"`
	Branch       string        `json:"branch,omitempty"`
	Command      []string      `json:"command"`
	AutoPush     bool          `json:"auto_push"`
	Status       JobStatus     `json:"status"`
	ExitCode     int           `json:"exit_code"`
	Error        string        `json:"error,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
	Duration     time.Duration `json:"duration_ns,omitempty"`
	WorktreeDir  string        `json:"worktree_dir,omitempty"`
	LogFilePath  string        `json:"log_file_path,omitempty"`
	DaemonBuild  string        `json:"daemon_build,omitempty"`
	DedupeKey    string        `json:"dedupe_key,omitempty"`
	Reused       bool          `json:"reused,omitempty"`
}

// IsFinished reports whether this snapshot has a terminal verdict.
func (j Job) IsFinished() bool {
	return j.Status == StatusPassed || j.Status == StatusFailed || j.Status == StatusCancelled
}

// DurationString returns a human-readable runtime for command-line output.
func (j Job) DurationString() string {
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

// EnqueueRequest describes a verification request. Timeout is encoded as
// whole seconds to preserve the daemon's wire contract.
type EnqueueRequest struct {
	RepoPath  string        `json:"repo_path"`
	Ref       string        `json:"ref,omitempty"`
	Command   []string      `json:"command,omitempty"`
	Name      string        `json:"name,omitempty"`
	AutoPush  bool          `json:"auto_push,omitempty"`
	Timeout   time.Duration `json:"-"`
	Snapshot  bool          `json:"snapshot,omitempty"`
	DedupeKey string        `json:"dedupe_key,omitempty"`
}

func (r EnqueueRequest) MarshalJSON() ([]byte, error) {
	if r.Timeout < 0 || r.Timeout%time.Second != 0 {
		return nil, fmt.Errorf("timeout must be a nonnegative whole number of seconds")
	}
	type alias EnqueueRequest
	return json.Marshal(struct {
		alias
		TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	}{alias: alias(r), TimeoutSeconds: int64(r.Timeout / time.Second)})
}

func (r *EnqueueRequest) UnmarshalJSON(data []byte) error {
	type alias EnqueueRequest
	var wire struct {
		alias
		TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.TimeoutSeconds < 0 || wire.TimeoutSeconds > math.MaxInt64/int64(time.Second) {
		return fmt.Errorf("timeout_seconds is out of range")
	}
	*r = EnqueueRequest(wire.alias)
	r.Timeout = time.Duration(wire.TimeoutSeconds) * time.Second
	return nil
}

// CleanReport summarizes daemon maintenance without exposing its internals.
type CleanReport struct {
	JobsFlushed     int    `json:"jobs_flushed"`
	WorktreesPruned int    `json:"worktrees_pruned"`
	LogsPruned      int    `json:"logs_pruned"`
	AllocBytesAfter uint64 `json:"alloc_bytes_after"`
}

// Health is the daemon compatibility response.
type Health struct {
	Status        string   `json:"status"`
	Protocol      int      `json:"protocol_version"`
	Build         string   `json:"build"`
	Capabilities  []string `json:"capabilities"`
	Time          string   `json:"time"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	QueueDepth    int      `json:"queue_depth"`
	ActiveJobs    int      `json:"active_jobs"`
	PID           int      `json:"pid"`
	Concurrency   int      `json:"concurrency"`
}

// HasCapability reports whether the daemon advertised capability.
func (h Health) HasCapability(capability string) bool {
	for _, got := range h.Capabilities {
		if strings.TrimSpace(got) == capability {
			return true
		}
	}
	return false
}
