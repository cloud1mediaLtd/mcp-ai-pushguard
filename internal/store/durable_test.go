package store

import (
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
)

func queuedJob(id string) *core.Job {
	return &core.Job{ID: id, RepoPath: "/repo", Ref: "HEAD", Status: core.StatusQueued, CreatedAt: time.Now()}
}

// The scenario this store exists for, end to end: enqueue, walk away, daemon
// restarts, come back and ask.
func TestDurableStoreAnswersAfterARestartInsteadOfLosingTheJob(t *testing.T) {
	dir := t.TempDir()

	first := NewDurable(NewMemoryStore(10), dir)
	job := queuedJob("pg_walkedaway")
	if err := first.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A brand-new daemon: fresh memory, same directory.
	second := NewDurable(NewMemoryStore(10), dir)

	if _, err := second.inner.Get("pg_walkedaway"); err == nil {
		t.Fatal("precondition failed: the in-memory store should not know this job")
	}
	got, err := second.Get("pg_walkedaway")
	if err != nil {
		t.Fatalf("a job enqueued before the restart must still answer, got: %v", err)
	}
	if got.ID != "pg_walkedaway" || got.RepoPath != "/repo" {
		t.Fatalf("recovered job does not match what was saved: %+v", got)
	}
}

// "Not found" is the wrong answer for a job the daemon dropped: it is
// indistinguishable from a typo, and sends the caller looking for a bug in
// their own command rather than re-running.
func TestRecoverOrphansTurnsInterruptedJobsIntoAnExplicitReason(t *testing.T) {
	dir := t.TempDir()
	first := NewDurable(NewMemoryStore(10), dir)

	running := queuedJob("pg_running")
	running.Status = core.StatusRunning
	_ = first.Save(running)
	_ = first.Save(queuedJob("pg_queued"))

	finished := queuedJob("pg_finished")
	finished.Status = core.StatusPassed
	_ = first.Save(finished)

	orphans := NewDurable(NewMemoryStore(10), dir).RecoverOrphans()
	if len(orphans) != 2 {
		t.Fatalf("recovered %d orphans, want the queued and the running one", len(orphans))
	}

	third := NewDurable(NewMemoryStore(10), dir)
	for _, id := range []string{"pg_running", "pg_queued"} {
		job, err := third.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if job.Status != core.StatusCancelled {
			t.Errorf("%s status = %q, want cancelled", id, job.Status)
		}
		if job.Error == "" || job.FinishedAt == nil {
			t.Errorf("%s carries no reason or no finish time: %+v", id, job)
		}
	}

	// The message must name the state the job was actually in -- saying
	// "cancelled while this job was cancelled" tells the reader nothing.
	running2, _ := third.Get("pg_running")
	if got := running2.Error; got == "" || !contains(got, "running") {
		t.Errorf("orphan reason should name the interrupted state, got: %q", got)
	}
	queued2, _ := third.Get("pg_queued")
	if got := queued2.Error; got == "" || !contains(got, "queued") {
		t.Errorf("orphan reason should name the interrupted state, got: %q", got)
	}

	// A job that genuinely finished is not an orphan and must not be rewritten.
	done, _ := third.Get("pg_finished")
	if done.Status != core.StatusPassed {
		t.Errorf("a finished job was rewritten to %q", done.Status)
	}
}

func TestDurableStoreClearRemovesTheRecordsItDrops(t *testing.T) {
	dir := t.TempDir()
	s := NewDurable(NewMemoryStore(10), dir)

	done := queuedJob("pg_done")
	done.Status = core.StatusPassed
	_ = s.Save(done)
	_ = s.Save(queuedJob("pg_open"))

	s.Clear(true)

	if _, err := NewDurable(NewMemoryStore(10), dir).Get("pg_done"); err == nil {
		t.Error("a cleared job left its record behind; the directory would grow forever")
	}
	if _, err := NewDurable(NewMemoryStore(10), dir).Get("pg_open"); err != nil {
		t.Error("clear(completedOnly) removed a job that had not completed")
	}
}

// An id that could climb out of the directory must never reach the filesystem.
func TestDurableStoreRefusesAnIDThatEscapesItsDirectory(t *testing.T) {
	s := NewDurable(NewMemoryStore(10), t.TempDir())
	for _, id := range []string{"../escape", "a/b", "", "..", "x.json"} {
		if path := s.path(id); path != "" {
			t.Errorf("id %q resolved to a writable path %q", id, path)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 ||
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}())
}
