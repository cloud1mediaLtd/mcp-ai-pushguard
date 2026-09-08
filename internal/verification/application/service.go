// Package application contains PushGuard's verification use cases and their
// ports. Transports depend on this package instead of the execution details in
// internal/core.
package application

import (
	"context"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/pkg/api"
)

// Service is the verification boundary used by HTTP, MCP, and command-line
// adapters. Every returned Job is a detached API snapshot.
type Service interface {
	Enqueue(api.EnqueueRequest) (api.Job, int, error)
	GetJob(string) (api.Job, error)
	ListJobs(int) []api.Job
	Cancel(string) error
	AwaitJob(context.Context, string, time.Duration) (api.Job, error)
	Logs(string, int) (string, error)
	Clean() api.CleanReport
	Start()
	Stop()
	ActiveJobCount() int
	IdleDuration() time.Duration
	TryDrainIdle(time.Duration) bool
	PruneDiskArtifacts() (int, int)
	QueueDepth() int
	Uptime() time.Duration
}

// EngineService adapts the existing isolated-worktree executor to the
// application boundary. It is the only package outside core that translates
// between runtime state and public API values.
type EngineService struct{ engine *core.Engine }

func NewEngineService(engine *core.Engine) *EngineService { return &EngineService{engine: engine} }

func (s *EngineService) Enqueue(req api.EnqueueRequest) (api.Job, int, error) {
	job, position, err := s.engine.Enqueue(core.EnqueueRequest{
		RepoPath: req.RepoPath, Ref: req.Ref, Command: append([]string(nil), req.Command...),
		Name: req.Name, AutoPush: req.AutoPush, Timeout: req.Timeout, Snapshot: req.Snapshot, DedupeKey: req.DedupeKey,
	})
	if err != nil {
		return api.Job{}, 0, err
	}
	return jobSnapshot(job), position, nil
}

func (s *EngineService) GetJob(id string) (api.Job, error) {
	job, err := s.engine.GetJob(id)
	if err != nil {
		return api.Job{}, err
	}
	return jobSnapshot(job), nil
}

func (s *EngineService) ListJobs(limit int) []api.Job {
	jobs := s.engine.ListJobs(limit)
	out := make([]api.Job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, jobSnapshot(job))
	}
	return out
}

func (s *EngineService) Cancel(id string) error { return s.engine.Cancel(id) }

func (s *EngineService) AwaitJob(ctx context.Context, id string, interval time.Duration) (api.Job, error) {
	job, err := s.engine.AwaitJob(ctx, id, interval)
	if err != nil {
		return api.Job{}, err
	}
	return jobSnapshot(job), nil
}

func (s *EngineService) Logs(id string, lines int) (string, error) {
	job, err := s.engine.GetJob(id)
	if err != nil {
		return "", err
	}
	return job.TailLogs(lines), nil
}

func (s *EngineService) Clean() api.CleanReport {
	r := s.engine.Clean()
	return api.CleanReport{JobsFlushed: r.JobsFlushed, WorktreesPruned: r.WorktreesPruned, LogsPruned: r.LogsPruned, AllocBytesAfter: r.AllocBytesAfter}
}
func (s *EngineService) Start()                               { s.engine.Start() }
func (s *EngineService) Stop()                                { s.engine.Stop() }
func (s *EngineService) ActiveJobCount() int                  { return s.engine.ActiveJobCount() }
func (s *EngineService) IdleDuration() time.Duration          { return s.engine.IdleDuration() }
func (s *EngineService) TryDrainIdle(idle time.Duration) bool { return s.engine.TryDrainIdle(idle) }
func (s *EngineService) PruneDiskArtifacts() (int, int)       { return s.engine.PruneDiskArtifacts() }
func (s *EngineService) QueueDepth() int                      { return s.engine.QueueDepth() }
func (s *EngineService) Uptime() time.Duration                { return s.engine.Uptime() }

func jobSnapshot(job *core.Job) api.Job {
	j := job.Snapshot()
	return api.Job{ID: j.ID, Name: j.Name, RepoPath: j.RepoPath, Ref: j.Ref, RequestedRef: j.RequestedRef,
		SourceKind: j.SourceKind, BaseSHA: j.BaseSHA, Timeout: j.Timeout, CommitSHA: j.CommitSHA,
		Branch: j.Branch, Command: append([]string(nil), j.Command...), AutoPush: j.AutoPush,
		Status: api.JobStatus(j.Status), ExitCode: j.ExitCode, Error: j.Error, CreatedAt: j.CreatedAt,
		StartedAt: copyTime(j.StartedAt), FinishedAt: copyTime(j.FinishedAt), Duration: j.Duration,
		WorktreeDir: j.WorktreeDir, LogFilePath: j.LogFilePath, DaemonBuild: j.DaemonBuild, DedupeKey: j.DedupeKey, Reused: j.Reused}
}

// Snapshot converts an internal execution record to the detached public
// contract. It is used by the embedded fallback adapter until that adapter is
// migrated fully onto Service.
func Snapshot(job *core.Job) api.Job { return jobSnapshot(job) }

// CleanSnapshot converts internal maintenance data to the public contract.
func CleanSnapshot(r core.CleanReport) api.CleanReport {
	return api.CleanReport{JobsFlushed: r.JobsFlushed, WorktreesPruned: r.WorktreesPruned, LogsPruned: r.LogsPruned, AllocBytesAfter: r.AllocBytesAfter}
}

// SortForQueueView orders immutable snapshots in the same priority order as
// the verification queue: running, queued, then finished.
func SortForQueueView(jobs []api.Job) {
	priority := func(status api.JobStatus) int {
		switch status {
		case api.StatusRunning:
			return 0
		case api.StatusQueued:
			return 1
		default:
			return 2
		}
	}
	for i := 1; i < len(jobs); i++ {
		for j := i; j > 0 && (priority(jobs[j].Status) < priority(jobs[j-1].Status) || (priority(jobs[j].Status) == priority(jobs[j-1].Status) && jobs[j].CreatedAt.After(jobs[j-1].CreatedAt))); j-- {
			jobs[j], jobs[j-1] = jobs[j-1], jobs[j]
		}
	}
}

func copyTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
