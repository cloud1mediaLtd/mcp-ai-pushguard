package core

import (
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
