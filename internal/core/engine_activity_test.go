package core

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// fakeStore is a minimal StoreInterface implementation. internal/store can't
// be imported here: it imports internal/core, which would be a cycle from a
// same-package test file.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: make(map[string]*Job)} }

func (s *fakeStore) Save(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	return nil
}

func (s *fakeStore) Get(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s not found", id)
	}
	return j, nil
}

func (s *fakeStore) List(limit int) []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out
}

func (s *fakeStore) Update(j *Job) error { return s.Save(j) }

func (s *fakeStore) Clear(completedOnly bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.jobs)
	s.jobs = make(map[string]*Job)
	return n
}

// initTempGitRepo creates a throwaway git repo so Engine.Enqueue's
// git.GetRepoRoot lookup succeeds without touching a real project.
func initTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(dir+"/README.md", []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}
	return dir
}

func TestEngine_IdleDurationAndActiveJobCount(t *testing.T) {
	repo := initTempGitRepo(t)
	// No worker pool started (Start() not called): jobs stay queued rather
	// than executing, so ActiveJobCount reflects the queue alone.
	engine := NewEngine(EngineConfig{MaxConcurrent: 1}, newFakeStore(), nil, nil, nil, nil)

	if d := engine.IdleDuration(); d > time.Second {
		t.Fatalf("expected a freshly created engine to be idle for well under a second, got %s", d)
	}
	if got := engine.ActiveJobCount(); got != 0 {
		t.Fatalf("expected 0 active jobs on a fresh engine, got %d", got)
	}

	before := time.Now()
	time.Sleep(5 * time.Millisecond)

	if _, _, err := engine.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	if got := engine.ActiveJobCount(); got != 1 {
		t.Fatalf("expected 1 active (queued) job after enqueue, got %d", got)
	}
	if !engine.lastActivityAfter(before) {
		t.Fatalf("expected enqueue to bump lastActivity forward")
	}
}

// lastActivityAfter is a small test-only accessor so the test doesn't reach
// into the atomic field directly.
func (e *Engine) lastActivityAfter(t time.Time) bool {
	return time.Unix(0, e.lastActivity.Load()).After(t)
}
