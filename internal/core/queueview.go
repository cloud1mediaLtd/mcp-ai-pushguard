package core

import "sort"

// SortForQueueView orders jobs the way someone sharing the queue reads them.
//
// The default order is recency, which is right for a log and wrong for a queue:
// with several agents enqueueing against one repository, cancelled and finished
// jobs interleave with pending ones and "what runs next" stops being answerable
// from the output at all.
//
// Running first (what is happening now), then queued in the order they will
// actually run, then everything finished by recency. Ties break on ID so the
// view is stable between calls rather than shuffling under a reader.
func SortForQueueView(jobs []*Job) {
	rank := func(j *Job) int {
		if j == nil {
			return 3
		}
		switch j.Status {
		case StatusRunning:
			return 0
		case StatusQueued:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(jobs, func(a, b int) bool {
		ra, rb := rank(jobs[a]), rank(jobs[b])
		if ra != rb {
			return ra < rb
		}
		if jobs[a] == nil || jobs[b] == nil {
			return jobs[a] != nil
		}
		// Queued jobs run oldest-first, so that is their real order. Finished
		// jobs are history, so the newest is the interesting one.
		if ra == 1 {
			return jobs[a].CreatedAt.Before(jobs[b].CreatedAt)
		}
		if jobs[a].CreatedAt.Equal(jobs[b].CreatedAt) {
			return jobs[a].ID < jobs[b].ID
		}
		return jobs[a].CreatedAt.After(jobs[b].CreatedAt)
	})
}
