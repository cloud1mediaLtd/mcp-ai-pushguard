package store

import (
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/pushguard/pushguard/internal/core"
)

var (
	ErrJobNotFound = errors.New("job not found")
)

// MemoryStats contains runtime memory allocations and job counts.
type MemoryStats struct {
	AllocBytes      uint64 `json:"alloc_bytes"`
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
	SysBytes        uint64 `json:"sys_bytes"`
	NumGC           uint32 `json:"num_gc"`
	TotalJobs       int    `json:"total_jobs"`
	ActiveJobs      int    `json:"active_jobs"`
	CompletedJobs   int    `json:"completed_jobs"`
}

// Store defines persistence and querying for PushGuard jobs.
type Store interface {
	Save(job *core.Job) error
	Get(id string) (*core.Job, error)
	List(limit int) []*core.Job
	Update(job *core.Job) error
	Clear(completedOnly bool) int
	Stats() MemoryStats
}

// MemoryStore is a thread-safe in-memory store.
type MemoryStore struct {
	mu       sync.RWMutex
	jobs     map[string]*core.Job
	order    []string
	capacity int
}

// NewMemoryStore creates an in-memory job store retaining up to capacity jobs.
func NewMemoryStore(capacity int) *MemoryStore {
	if capacity <= 0 {
		capacity = 100
	}
	return &MemoryStore{
		jobs:     make(map[string]*core.Job),
		order:    make([]string, 0, capacity),
		capacity: capacity,
	}
}

// Save inserts a new job into the store.
func (s *MemoryStore) Save(job *core.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.jobs[job.ID]; exists {
		return fmt.Errorf("job %s already exists", job.ID)
	}
	s.jobs[job.ID] = job
	s.order = append([]string{job.ID}, s.order...)

	s.trimLocked()
	return nil
}

// trimLocked limits completed history without ever evicting active jobs.
func (s *MemoryStore) trimLocked() {
	completed := 0
	order := make([]string, 0, len(s.order))
	for _, id := range s.order {
		job, ok := s.jobs[id]
		if !ok {
			continue
		}
		if job.IsFinished() {
			completed++
			if completed > s.capacity {
				delete(s.jobs, id)
				continue
			}
		}
		order = append(order, id)
	}
	s.order = order
}

// Get retrieves a job by ID.
func (s *MemoryStore) Get(id string) (*core.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	return job, nil
}

// List returns the latest jobs up to limit.
func (s *MemoryStore) List(limit int) []*core.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.order) {
		limit = len(s.order)
	}

	result := make([]*core.Job, 0, limit)
	for i := 0; i < limit; i++ {
		if j, ok := s.jobs[s.order[i]]; ok {
			result = append(result, j)
		}
	}
	return result
}

// Update saves changes to an existing job.
func (s *MemoryStore) Update(job *core.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[job.ID]; !ok {
		return ErrJobNotFound
	}
	s.jobs[job.ID] = job
	s.trimLocked()
	return nil
}

// Clear flushes jobs from memory. If completedOnly is true, only completed/failed/cancelled
// jobs are removed, preserving queued/running jobs. Returns the number of removed jobs.
func (s *MemoryStore) Clear(completedOnly bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	if !completedOnly {
		removed = len(s.jobs)
		s.jobs = make(map[string]*core.Job)
		s.order = make([]string, 0, s.capacity)
		return removed
	}

	newOrder := make([]string, 0, len(s.order))
	for _, id := range s.order {
		job, ok := s.jobs[id]
		if ok && job.IsFinished() {
			delete(s.jobs, id)
			removed++
		} else if ok {
			newOrder = append(newOrder, id)
		}
	}
	s.order = newOrder
	return removed
}

// Stats gathers memory allocations and job counts.
func (s *MemoryStore) Stats() MemoryStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	active := 0
	completed := 0
	for _, j := range s.jobs {
		if j.IsFinished() {
			completed++
		} else {
			active++
		}
	}

	return MemoryStats{
		AllocBytes:      m.Alloc,
		TotalAllocBytes: m.TotalAlloc,
		SysBytes:        m.Sys,
		NumGC:           m.NumGC,
		TotalJobs:       len(s.jobs),
		ActiveJobs:      active,
		CompletedJobs:   completed,
	}
}
