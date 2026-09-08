package store

import (
	"os"
	"sync"
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

// Clear evicts from memory only. It used to delete the on-disk record too, and
// the combination destroyed the very thing this store exists for: idle shutdown
// calls engine.Clean() -> Clear(), so half an hour after the queue emptied,
// every completed job's record was gone and a caller who had walked away got
// "job not found". A real session lost two gate results to it.
//
// PruneOld is the retention mechanism — age-based, and it never touches a
// queued or running record. Keeping both meant keeping neither.
func TestDurableStoreClearLeavesTheRecordOnDisk(t *testing.T) {
	dir := t.TempDir()
	s := NewDurable(NewMemoryStore(10), dir)

	done := queuedJob("pg_done")
	done.Status = core.StatusPassed
	_ = s.Save(done)
	_ = s.Save(queuedJob("pg_open"))

	if dropped := s.Clear(true); dropped != 1 {
		t.Fatalf("Clear(completedOnly) evicted %d from memory, want 1", dropped)
	}

	// Gone from memory...
	if _, err := s.inner.Get("pg_done"); err == nil {
		t.Error("Clear did not evict the finished job from memory")
	}
	// ...but still answerable, which is the whole point.
	fresh := NewDurable(NewMemoryStore(10), dir)
	if _, err := fresh.Get("pg_done"); err != nil {
		t.Error("a finished job's record was deleted by Clear; walking away for 30 minutes would lose the result")
	}
	if _, err := fresh.Get("pg_open"); err != nil {
		t.Error("an unfinished job's record was removed")
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

// PruneOld is what keeps ~/.pushguard/jobs bounded on a daemon that's busy
// all day and never idles long enough to trigger a full Clean() — it must
// remove only jobs both finished AND older than maxAge, never a queued or
// running one regardless of age, and never touch the in-memory store.
func TestDurableStore_PruneOldRemovesOnlyOldFinishedJobs(t *testing.T) {
	dir := t.TempDir()
	s := NewDurable(NewMemoryStore(10), dir)

	old := queuedJob("pg_old_finished")
	old.Status = core.StatusPassed
	oldFinish := time.Now().Add(-72 * time.Hour)
	old.FinishedAt = &oldFinish
	if err := s.Save(old); err != nil {
		t.Fatalf("save old: %v", err)
	}

	recent := queuedJob("pg_recent_finished")
	recent.Status = core.StatusFailed
	recentFinish := time.Now().Add(-1 * time.Hour)
	recent.FinishedAt = &recentFinish
	if err := s.Save(recent); err != nil {
		t.Fatalf("save recent: %v", err)
	}

	stillRunning := queuedJob("pg_old_but_running")
	stillRunning.Status = core.StatusRunning
	stillRunning.CreatedAt = time.Now().Add(-72 * time.Hour)
	if err := s.Save(stillRunning); err != nil {
		t.Fatalf("save running: %v", err)
	}

	pruned, err := s.PruneOld(48 * time.Hour)
	if err != nil {
		t.Fatalf("PruneOld failed: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected exactly 1 pruned record, got %d", pruned)
	}

	if _, err := s.readFromDisk("pg_old_finished"); err == nil {
		t.Fatal("expected the old finished job's disk record to be gone")
	}
	if _, err := s.readFromDisk("pg_recent_finished"); err != nil {
		t.Fatalf("recent finished job should survive: %v", err)
	}
	if _, err := s.readFromDisk("pg_old_but_running"); err != nil {
		t.Fatalf("a still-running job must never be pruned regardless of age: %v", err)
	}

	// In-memory presence (what pushguard_list/status actually reads from in
	// the common case) must be completely untouched by a disk-only prune.
	if _, err := s.inner.Get("pg_old_finished"); err != nil {
		t.Fatalf("PruneOld must not touch the in-memory store: %v", err)
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

func TestDurableStoreRejectsUnwritableAdmission(t *testing.T) {
	// A regular file is deterministically unusable as a job directory, including as root.
	dir := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(dir, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	inner := NewMemoryStore(1)
	s := NewDurable(inner, dir)
	job := queuedJob("pg_rejected")
	if err := s.Save(job); err == nil {
		t.Fatal("durability failure reported success")
	}
	if _, err := inner.Get(job.ID); err != ErrJobNotFound {
		t.Fatal("failed admission leaked into memory")
	}
}

func TestDurableStorePersistsEvictedTerminalUpdate(t *testing.T) {
	dir := t.TempDir()
	s := NewDurable(NewMemoryStore(1), dir)
	job := queuedJob("pg_history")
	job.Status = core.StatusPassed
	if err := s.Save(job); err != nil {
		t.Fatal(err)
	}
	s.Clear(true)
	job.Error = "updated history"
	if err := s.Update(job); err != nil {
		t.Fatal(err)
	}
	disk, err := s.readFromDisk(job.ID)
	if err != nil || disk.Error != job.Error {
		t.Fatalf("evicted update lost: %+v %v", disk, err)
	}
}

func TestDurableStoreConcurrentUpdatesPreserveCurrentState(t *testing.T) {
	s := NewDurable(NewMemoryStore(1), t.TempDir())
	job := queuedJob("pg_concurrent")
	if err := s.Save(job); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				if err := s.Update(job); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	job.MarkOrphaned("cancelled", time.Now())
	if err := s.Update(job); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	disk, err := s.readFromDisk(job.ID)
	if err != nil || disk.Status != core.StatusCancelled {
		t.Fatalf("persisted stale state: %+v %v", disk, err)
	}
}

func TestDurableStoreListMergesRestartedHistoryAndLiveState(t *testing.T) {
	dir := t.TempDir()
	first := NewDurable(NewMemoryStore(1), dir)
	previous := queuedJob("pg_previous")
	previous.Status = core.StatusPassed
	if err := first.Save(previous); err != nil {
		t.Fatal(err)
	}
	second := NewDurable(NewMemoryStore(1), dir)
	live := queuedJob("pg_live")
	if err := second.Save(live); err != nil {
		t.Fatal(err)
	}
	jobs := second.List(0)
	if len(jobs) != 2 || jobs[0] != live || jobs[1].ID != previous.ID {
		t.Fatalf("history/live merge: %+v", jobs)
	}
	if got := len(second.List(1)); got != 1 {
		t.Fatalf("limit ignored: %d", got)
	}
}
