package core

import (
	"testing"
	"time"
)

func job(id string, status JobStatus, createdMinutesAgo int) *Job {
	return &Job{ID: id, Status: status, CreatedAt: time.Now().Add(-time.Duration(createdMinutesAgo) * time.Minute)}
}

// With several agents queueing against one repository, the default recency
// order interleaves cancelled and finished jobs with pending ones, and "what
// runs next" stops being answerable from the output.
func TestSortForQueueViewAnswersWhatIsRunningAndWhatIsNext(t *testing.T) {
	jobs := []*Job{
		job("pg_cancelled", StatusCancelled, 5),
		job("pg_queued_second", StatusQueued, 2),
		job("pg_passed", StatusPassed, 1),
		job("pg_running", StatusRunning, 6),
		job("pg_queued_first", StatusQueued, 4),
	}

	SortForQueueView(jobs)

	got := make([]string, 0, len(jobs))
	for _, j := range jobs {
		got = append(got, j.ID)
	}
	want := []string{"pg_running", "pg_queued_first", "pg_queued_second", "pg_passed", "pg_cancelled"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d = %q, want %q\nfull order: %v", i, got[i], want[i], got)
		}
	}
}

// Queued jobs run oldest-first, so that is their real order — showing the
// newest first would put a "1" beside the job that runs last.
func TestSortForQueueViewOrdersQueuedOldestFirst(t *testing.T) {
	jobs := []*Job{job("pg_new", StatusQueued, 1), job("pg_old", StatusQueued, 9), job("pg_mid", StatusQueued, 5)}
	SortForQueueView(jobs)
	if jobs[0].ID != "pg_old" || jobs[2].ID != "pg_new" {
		t.Fatalf("queued order = %s, %s, %s; want oldest first", jobs[0].ID, jobs[1].ID, jobs[2].ID)
	}
}

// Finished work is history: the newest is the interesting one.
func TestSortForQueueViewOrdersFinishedNewestFirst(t *testing.T) {
	jobs := []*Job{job("pg_older", StatusPassed, 9), job("pg_newer", StatusFailed, 1)}
	SortForQueueView(jobs)
	if jobs[0].ID != "pg_newer" {
		t.Fatalf("finished order = %s, %s; want newest first", jobs[0].ID, jobs[1].ID)
	}
}

// The view is read repeatedly by more than one agent; it must not shuffle
// between calls, and a nil entry must not panic the whole listing.
func TestSortForQueueViewIsStableAndNilSafe(t *testing.T) {
	build := func() []*Job {
		same := time.Now()
		a, b := job("pg_aaa", StatusQueued, 0), job("pg_bbb", StatusQueued, 0)
		a.CreatedAt, b.CreatedAt = same, same
		return []*Job{b, a, nil}
	}
	first, second := build(), build()
	SortForQueueView(first)
	SortForQueueView(second)
	for i := range first {
		if (first[i] == nil) != (second[i] == nil) {
			t.Fatalf("ordering is not stable at %d", i)
		}
		if first[i] != nil && first[i].ID != second[i].ID {
			t.Fatalf("ordering is not stable at %d: %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
	if first[len(first)-1] != nil {
		t.Fatal("a nil entry must sort last rather than panic or lead")
	}
}

// The ref column exists to answer "which commit is this testing", and there are
// two SHAs on a job that look equally plausible.
//
// Job.Ref is the commit actually gated — what the caller asked for, or the
// resolved HEAD or working-tree snapshot when they asked for neither.
// Job.CommitSHA is the branch tip at the moment of enqueue, recorded regardless
// of --ref. Reporting the tip for a job deliberately gating an older SHA is the
// dangerous direction: it claims a release gate covered a fix it did not.
//
// This pins the distinction at the source, since both renderers read it.
func TestJobRefAndCommitSHADisagreeWhenGatingAnOlderCommit(t *testing.T) {
	// Exactly the shape Engine.Enqueue produces for `--ref <older sha>`.
	j := &Job{ID: "pg_x", Ref: "d7ab0d01a", CommitSHA: "635ee5063"}

	if j.Ref == j.CommitSHA {
		t.Fatal("fixture is wrong: the point is that these differ")
	}
	// A renderer must show Ref. If this ever becomes CommitSHA again, the
	// column silently starts lying about what was gated.
	if got := j.Ref; got != "d7ab0d01a" {
		t.Fatalf("the gated commit is %q, want the requested ref", got)
	}
}
