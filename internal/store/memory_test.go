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

	if _, err := s.Get("job1"); err != nil {
		t.Fatalf("active oldest job1 must survive capacity: %v", err)
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

func TestMemoryStoreCapacityRetainsEveryActiveJob(t *testing.T) {
	s := NewMemoryStore(1)
	active := queuedJob("pg_active")
	if err := s.Save(active); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pg_done1", "pg_done2"} {
		done := queuedJob(id)
		done.Status = core.StatusPassed
		if err := s.Save(done); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.Get(active.ID); err != nil || got != active {
		t.Fatalf("lost live job identity: %p %v", got, err)
	}
	if _, err := s.Get("pg_done1"); err != ErrJobNotFound {
		t.Fatal("old completed history was not evicted")
	}
	if got := len(s.List(0)); got != 2 {
		t.Fatalf("expected active plus one history record, got %d", got)
	}
}
