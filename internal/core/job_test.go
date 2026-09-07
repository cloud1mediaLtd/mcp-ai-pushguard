package core

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestJob_DurationAndLogs(t *testing.T) {
	job := &Job{
		ID:     "pg_test123",
		Status: StatusRunning,
	}

	if job.IsFinished() {
		t.Errorf("expected job to not be finished")
	}

	now := time.Now()
	job.StartedAt = &now
	time.Sleep(10 * time.Millisecond)

	finished := time.Now()
	job.FinishedAt = &finished
	job.Duration = finished.Sub(now)
	job.Status = StatusPassed

	if !job.IsFinished() {
		t.Errorf("expected job to be finished")
	}

	job.AppendLog("line 1")
	job.AppendLog("line 2")
	job.AppendLog("line 3")

	tail := job.TailLogs(2)
	expected := "line 2\nline 3"
	if tail != expected {
		t.Errorf("got %q, want %q", tail, expected)
	}
}

// TestJob_ConcurrentMarshalDuringMutation is a regression test for a real
// data race found via `go test -race` in internal/daemon: a worker goroutine
// mutating a Job's fields (via the exact sequence executeJob uses) while
// another goroutine concurrently JSON-encodes the same *Job — as an HTTP
// handler serving a status/list response would. Before MarshalJSON and the
// set* mutators took job.mu, `go test -race` failed this reliably.
func TestJob_ConcurrentMarshalDuringMutation(t *testing.T) {
	job := &Job{ID: "pg_race_test", Status: StatusQueued}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader: hammers MarshalJSON, standing in for an HTTP handler encoding
	// this job into a response while it's in flight.
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := json.Marshal(job); err != nil {
					t.Errorf("MarshalJSON failed: %v", err)
					return
				}
			}
		}
	})

	// Writer: the same field-mutation sequence executeJob performs.
	job.setRunning(time.Now())
	job.setWorktreeDir("/tmp/whatever")
	for range 100 {
		job.setResult(0, StatusRunning, "")
	}
	job.setResult(0, StatusPassed, "")
	job.finalize(time.Now())

	close(stop)
	wg.Wait()

	if !job.IsFinished() {
		t.Fatalf("expected job to be finished after finalize")
	}
}

func TestGenerateJobID(t *testing.T) {
	id1 := GenerateJobID()
	id2 := GenerateJobID()

	if len(id1) == 0 || len(id2) == 0 {
		t.Fatalf("job id should not be empty")
	}
	if id1 == id2 {
		t.Fatalf("job IDs should be unique")
	}
}
