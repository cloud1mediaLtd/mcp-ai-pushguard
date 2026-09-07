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

// Clear removes the records it drops from memory, so `pushguard clean` does not
// leave the directory growing forever.
func (s *DurableStore) Clear(completedOnly bool) int {
	before := map[string]bool{}
	for _, job := range s.inner.List(0) {
		if job != nil {
			before[job.ID] = true
		}
	}
	n := s.inner.Clear(completedOnly)
	for _, job := range s.inner.List(0) {
		if job != nil {
			delete(before, job.ID)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range before {
		if path := s.path(id); path != "" {
			_ = os.Remove(path)
		}
	}
	return n
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
		job.Status = core.StatusCancelled
		job.Error = "daemon restarted while this job was " + wasStatus + "; it did not run to completion — re-enqueue it"
		finished := time.Now()
		job.FinishedAt = &finished
		s.persist(job)
		orphans = append(orphans, job)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].CreatedAt.Before(orphans[j].CreatedAt) })
	return orphans
}
