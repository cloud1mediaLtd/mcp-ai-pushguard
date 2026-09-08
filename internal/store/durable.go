package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pushguard/pushguard/internal/core"
)

// DurableStore keeps the in-memory store as the fast path and mirrors every job
// to disk so a daemon restart does not silently erase work.
//
// The problem it fixes is specific to how PushGuard is meant to be used. The
// whole point is "enqueue, then walk away" -- so the caller is, by design, not
// watching. If the daemon then restarts (a crash, the idle shutdown racing a
// new job, or somebody rebuilding the binary), every queued and running job
// vanishes from a memory-only store, and the caller who walked away has no way
// to find out: a later status call returns "job not found", which is
// indistinguishable from a typo'd id.
//
// Reads still come from memory first. Disk is the recovery path, not the hot
// path.
type DurableStore struct {
	inner Store
	dir   string
	mu    sync.Mutex
}

// NewDurable wraps inner, mirroring jobs into dir. A dir that cannot be created
// degrades to the in-memory store alone rather than failing to start: losing
// history is bad, refusing to run any checks is worse.
func NewDurable(inner Store, dir string) *DurableStore {
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return &DurableStore{inner: inner, dir: dir}
}

func (s *DurableStore) path(id string) string {
	// Job ids are generated hex, but this store is also handed ids from
	// callers, so refuse anything that could climb out of the directory.
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return ""
	}
	return filepath.Join(s.dir, id+".json")
}

func (s *DurableStore) persist(job *core.Job) {
	if s.dir == "" || job == nil {
		return
	}
	path := s.path(job.ID)
	if path == "" {
		return
	}
	data, err := json.Marshal(job)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Write-then-rename so a reader never sees a half-written record.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

func (s *DurableStore) Save(job *core.Job) error {
	if err := s.inner.Save(job); err != nil {
		return err
	}
	s.persist(job)
	return nil
}

func (s *DurableStore) Update(job *core.Job) error {
	if err := s.inner.Update(job); err != nil {
		return err
	}
	s.persist(job)
	return nil
}

// Get falls through to disk when memory does not have the job, so a job from
// before a restart still answers.
func (s *DurableStore) Get(id string) (*core.Job, error) {
	if job, err := s.inner.Get(id); err == nil {
		return job, nil
	}
	job, err := s.readFromDisk(id)
	if err != nil {
		return nil, ErrJobNotFound
	}
	return job, nil
}

func (s *DurableStore) readFromDisk(id string) (*core.Job, error) {
	path := s.path(id)
	if path == "" {
		return nil, ErrJobNotFound
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrJobNotFound
	}
	var job core.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, ErrJobNotFound
	}
	return &job, nil
}

func (s *DurableStore) List(limit int) []*core.Job { return s.inner.List(limit) }
func (s *DurableStore) Stats() MemoryStats         { return s.inner.Stats() }

// Clear evicts from memory only, delegating to the inner store.
//
// It used to delete the on-disk record of every job it dropped, so the
// directory would not grow forever. PruneOld below already does that job
// properly — age-based, 48 hours, and it never removes a queued or running
// record — so the two together only destroyed history.
//
// Idle shutdown calls engine.Clean(), which calls Clear(). So half an hour
// after the queue emptied, every completed job's record was gone and "enqueue
// and walk away" came back to "job not found". A real session lost two gate
// results to exactly that.
//
// Memory eviction and disk retention are separate concerns, and this is where
// they separate.
func (s *DurableStore) Clear(completedOnly bool) int {
	return s.inner.Clear(completedOnly)
}

// PruneOld deletes on-disk records for jobs that finished more than maxAge
// ago. Unlike Clear, this never touches the in-memory store — a job's
// presence in pushguard_list/status is unaffected — and it only ever
// considers jobs that are themselves recorded as finished, so a queued or
// running job is never at risk regardless of how old its CreatedAt is.
//
// This exists because RecoverOrphans scans this whole directory on every
// daemon startup, and the daemon's idle-shutdown lifecycle means startups
// are routine now, not rare. Without an age-based prune independent of a
// full Clean(), a daemon that's busy all day (and so never idles long
// enough to trigger Clean) accumulates one file per job here forever; this
// keeps that directory — and therefore startup's scan of it — bounded on
// its own schedule.
func (s *DurableStore) PruneOld(maxAge time.Duration) (int, error) {
	if s.dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	pruned := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		job, err := s.readFromDisk(id)
		if err != nil || !job.IsFinished() || job.FinishedAt == nil {
			continue
		}
		if job.FinishedAt.Before(cutoff) {
			if path := s.path(id); path != "" {
				s.mu.Lock()
				removeErr := os.Remove(path)
				s.mu.Unlock()
				if removeErr == nil {
					pruned++
				}
			}
		}
	}
	return pruned, nil
}

// RecoverOrphans marks jobs that disk says were queued or running as cancelled,
// and returns them.
//
// It is called once at daemon start. Any job still recorded as in-flight cannot
// be: this process has just started and is running nothing. Turning those into
// an explicit cancellation with a reason is the whole point -- the caller who
// enqueued and walked away gets "the daemon restarted, re-run it" instead of
// "job not found", which is the answer that would send them looking for a bug
// in their own command.
func (s *DurableStore) RecoverOrphans() []*core.Job {
	if s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var orphans []*core.Job
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		job, err := s.readFromDisk(id)
		if err != nil || job.IsFinished() {
			continue
		}
		wasStatus := string(job.Status)
		reason := "daemon restarted while this job was " + wasStatus + "; it did not run to completion — re-enqueue it"
		if !job.MarkOrphaned(reason, time.Now()) {
			// Raced with something else finishing it between the IsFinished
			// check above and the lock inside MarkOrphaned — vanishingly
			// unlikely at startup (nothing else is running yet), but correct
			// to just skip rather than persist a stale snapshot either way.
			continue
		}
		s.persist(job)
		orphans = append(orphans, job)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].CreatedAt.Before(orphans[j].CreatedAt) })
	return orphans
}

// ListFromDisk reads every mirrored record and returns them newest-first,
// capped at limit.
//
// List above delegates to the in-memory store, which is the right answer
// inside the daemon and the wrong one everywhere else: a CLI process is not
// the daemon, so its memory store is empty and `pushguard list` had nothing to
// fall back on when the daemon was down. Combined with the idle shutdown —
// which is routine, not exceptional — that meant "enqueue and walk away" could
// come back half an hour later to "daemon is not running" with a jobs
// directory full of the results being asked for.
//
// Get already falls through to disk; this is the same fallback for the
// listing path.
func (s *DurableStore) ListFromDisk(limit int) []*core.Job {
	if s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var jobs []*core.Job
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		job, err := s.readFromDisk(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs
}

// DefaultJobDir is where job records are mirrored: ~/.pushguard/jobs, alongside
// the socket, so the whole layout stays one place.
//
// It lives here rather than in the daemon's own package because the readers are
// no longer only the daemon. The CLI and the MCP server both have to answer
// "what happened to my job" with no daemon up, and each having its own idea of
// where the records are is exactly how one of them ends up confidently
// reporting an empty queue.
func DefaultJobDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pushguard", "jobs")
}

// OpenReadOnly opens the mirrored records for a process that only reads them.
// The memory store it wraps is a throwaway: nothing on this path writes, and
// in particular it must never share a store with an embedded engine that does
// — a second writer against the daemon's directory is how a running job gets
// recovered out from under itself.
func OpenReadOnly() *DurableStore {
	return &DurableStore{inner: NewMemoryStore(1), dir: DefaultJobDir()}
}
