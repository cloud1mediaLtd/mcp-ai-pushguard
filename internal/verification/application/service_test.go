package application

import (
	"context"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/pkg/api"
)

func newService(t *testing.T) (*EngineService, *store.MemoryStore) {
	t.Helper()
	mem := store.NewMemoryStore(10)
	return NewEngineService(core.NewEngine(core.EngineConfig{MaxConcurrent: 1}, mem, nil, nil, nil, nil)), mem
}

func savedJob(t *testing.T, mem *store.MemoryStore, id string, status core.JobStatus) *core.Job {
	t.Helper()
	job := &core.Job{ID: id, RepoPath: "/repo", Ref: "abc", Command: []string{"true"}, Status: status, CreatedAt: time.Now()}
	job.AppendLog("first")
	job.AppendLog("last")
	if err := mem.Save(job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestServiceReturnsDetachedSnapshots(t *testing.T) {
	svc, mem := newService(t)
	job := savedJob(t, mem, "pg_snapshot", core.StatusPassed)
	got, err := svc.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Command[0] = "mutated"
	again, err := svc.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Command[0] != "true" {
		t.Fatalf("service leaked mutable command: %#v", again.Command)
	}
	if !again.IsFinished() || again.Status != api.StatusPassed {
		t.Fatalf("snapshot = %#v", again)
	}
}

func TestServiceListLogsAwaitAndClean(t *testing.T) {
	svc, mem := newService(t)
	savedJob(t, mem, "pg_old", core.StatusFailed)
	savedJob(t, mem, "pg_new", core.StatusPassed)
	jobs := svc.ListJobs(1)
	if len(jobs) != 1 || jobs[0].ID != "pg_new" {
		t.Fatalf("list = %#v", jobs)
	}
	logs, err := svc.Logs("pg_new", 1)
	if err != nil || logs != "last" {
		t.Fatalf("logs = %q, %v", logs, err)
	}
	got, err := svc.AwaitJob(context.Background(), "pg_new", time.Millisecond)
	if err != nil || got.ID != "pg_new" {
		t.Fatalf("await = %#v, %v", got, err)
	}
	report := svc.Clean()
	if report.JobsFlushed != 2 {
		t.Fatalf("clean = %#v", report)
	}
}

func TestSnapshotAndQueueOrdering(t *testing.T) {
	now := time.Now()
	jobs := []api.Job{{ID: "finished", Status: api.StatusPassed, CreatedAt: now}, {ID: "queued", Status: api.StatusQueued, CreatedAt: now}, {ID: "running", Status: api.StatusRunning, CreatedAt: now}}
	SortForQueueView(jobs)
	if jobs[0].ID != "running" || jobs[1].ID != "queued" {
		t.Fatalf("order = %#v", jobs)
	}
	internal := &core.Job{ID: "pg_copy", Status: core.StatusCancelled, Command: []string{"true"}, CreatedAt: now}
	public := Snapshot(internal)
	if public.Status != api.StatusCancelled || public.ID != internal.ID {
		t.Fatalf("snapshot = %#v", public)
	}
	r := CleanSnapshot(core.CleanReport{JobsFlushed: 3})
	if r.JobsFlushed != 3 {
		t.Fatalf("clean snapshot = %#v", r)
	}
}

func TestServiceLifecycleDelegatesAndRejectsInvalidSubmission(t *testing.T) {
	svc, mem := newService(t)
	if _, _, err := svc.Enqueue(api.EnqueueRequest{AutoPush: true}); err == nil {
		t.Fatal("auto-push request was accepted")
	}
	if err := svc.Cancel("missing"); err == nil {
		t.Fatal("missing job was cancelled")
	}
	if _, err := svc.Logs("missing", 1); err == nil {
		t.Fatal("missing logs were returned")
	}
	if svc.ActiveJobCount() != 0 || svc.IdleDuration() < 0 {
		t.Fatal("unexpected lifecycle state")
	}
	if !svc.TryDrainIdle(0) {
		t.Fatal("idle service did not begin draining")
	}
	svc.Start()
	svc.Stop()
	if worktrees, logs := svc.PruneDiskArtifacts(); worktrees != 0 || logs != 0 {
		t.Fatalf("unexpected prune: %d %d", worktrees, logs)
	}
	_ = mem
}
