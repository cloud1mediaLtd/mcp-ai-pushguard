package store

import (
	"fmt"
	"testing"

	"github.com/pushguard/pushguard/internal/core"
)

func TestMemoryStore_CRUD(t *testing.T) {
	s := NewMemoryStore(3)

	job1 := &core.Job{ID: "job1", Status: core.StatusQueued}
	job2 := &core.Job{ID: "job2", Status: core.StatusQueued}
	job3 := &core.Job{ID: "job3", Status: core.StatusQueued}

	_ = s.Save(job1)
	_ = s.Save(job2)
	_ = s.Save(job3)

	retrieved, err := s.Get("job2")
	if err != nil || retrieved.ID != "job2" {
		t.Fatalf("failed to retrieve job2: %v", err)
	}

	job2.Status = core.StatusPassed
	_ = s.Update(job2)

	updated, _ := s.Get("job2")
	if updated.Status != core.StatusPassed {
		t.Fatalf("expected updated status passed, got %s", updated.Status)
	}

	list := s.List(10)
	if len(list) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(list))
	}

	// Test eviction capacity (max 3)
	job4 := &core.Job{ID: "job4", Status: core.StatusQueued}
	_ = s.Save(job4)

	if _, err := s.Get("job1"); err != ErrJobNotFound {
		t.Fatalf("expected oldest job1 to be evicted")
	}
}

func TestMemoryStore_NotFound(t *testing.T) {
	s := NewMemoryStore(10)
	_, err := s.Get("nonexistent")
	if err != ErrJobNotFound {
		t.Fatalf("expected ErrJobNotFound, got %v", err)
	}

	err = s.Update(&core.Job{ID: "nonexistent"})
	if err != ErrJobNotFound {
		t.Fatalf("expected ErrJobNotFound on update, got %v", err)
	}
	fmt.Println("NotFound tests passed")
}
