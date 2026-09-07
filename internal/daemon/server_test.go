package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/git"
	"github.com/pushguard/pushguard/internal/runner"
)

// fakeStore is a minimal StoreInterface implementation, avoiding a real
// dependency on internal/store (which would be fine here, but keeping the
// daemon package's tests self-contained makes the fixture explicit about
// exactly what the server needs from a store).
type fakeStore struct {
	jobs map[string]*core.Job
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: make(map[string]*core.Job)} }

func (s *fakeStore) Save(j *core.Job) error {
	s.jobs[j.ID] = j
	return nil
}

func (s *fakeStore) Get(id string) (*core.Job, error) {
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s not found", id)
	}
	return j, nil
}

func (s *fakeStore) List(limit int) []*core.Job {
	out := make([]*core.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out
}

func (s *fakeStore) Update(j *core.Job) error {
	s.jobs[j.ID] = j
	return nil
}

func (s *fakeStore) Clear(completedOnly bool) int {
	n := len(s.jobs)
	s.jobs = make(map[string]*core.Job)
	return n
}

// newTestServer starts a real daemon on a throwaway short-lived unix socket
// (short path: unix sockets are capped ~104 bytes on macOS, and t.TempDir()
// embeds the test name, which can push a long test name over that limit).
func newTestServer(t *testing.T) (*Server, string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pg-daemon-*")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(dir, "test.sock")

	wm, err := git.NewWorktreeManager("")
	if err != nil {
		t.Fatalf("failed to init worktree manager: %v", err)
	}
	r, err := runner.NewLocalRunner("")
	if err != nil {
		t.Fatalf("failed to init runner: %v", err)
	}
	engine := core.NewEngine(
		core.EngineConfig{MaxConcurrent: 1}, newFakeStore(), wm, r, nil,
		func(d string) []string { return runner.DetectCommand(d).Command },
	)
	engine.Start()

	server := NewServer(engine, Config{SocketPath: socketPath})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	cleanup := func() {
		_ = server.Stop(context.Background())
		engine.Stop()
		_ = os.RemoveAll(dir)
	}
	return server, socketPath, cleanup
}

func httpClientFor(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

func TestServer_Health(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", body["status"])
	}
}

func TestServer_EnqueueRejectsInvalidJSON(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := httpClientFor(socketPath).Post(
		"http://daemon/api/v1/jobs", "application/json", bytes.NewReader([]byte("{not json")),
	)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestServer_JobsListMethodNotAllowed(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodDelete, "http://daemon/api/v1/jobs", nil)
	resp, err := httpClientFor(socketPath).Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestServer_GetUnknownJob404(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/jobs/pg_does_not_exist")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestServer_JobRouteInvalidURL(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	// Fewer than 4 path segments (/api/v1/jobs/{id}) is malformed.
	resp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/jobs/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed job URL, got %d", resp.StatusCode)
	}
}

func TestServer_JobRouteUnknownSubaction(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	// A job id that doesn't exist under an unrecognized action still hits
	// the unknown-subaction branch (checked before any job lookup for
	// "cancel"/"logs"-shaped paths) — 404 either way is the documented
	// contract for this route, whether the 404 comes from "no such job" or
	// "no such action".
	resp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/jobs/pg_x/not-a-real-action")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestServer_CancelRequiresPost(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/jobs/pg_x/cancel")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET on cancel, got %d", resp.StatusCode)
	}
}

func TestServer_CleanRequiresPost(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/clean")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET on clean, got %d", resp.StatusCode)
	}

	resp2, err := httpClientFor(socketPath).Post("http://daemon/api/v1/clean", "application/json", nil)
	if err != nil {
		t.Fatalf("POST clean failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for POST clean, got %d", resp2.StatusCode)
	}
}

func TestServer_EnqueueAndFetchEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg-repo-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// A throwaway repo so Enqueue's git.GetRepoRoot lookup succeeds.
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	body, _ := json.Marshal(core.EnqueueRequest{RepoPath: dir, Command: []string{"true"}})
	resp, err := httpClientFor(socketPath).Post("http://daemon/api/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var enqueueResp struct {
		Job *core.Job `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&enqueueResp); err != nil {
		t.Fatalf("failed to decode enqueue response: %v", err)
	}
	if enqueueResp.Job == nil || enqueueResp.Job.ID == "" {
		t.Fatalf("expected a job with an ID in the response")
	}

	// GET /api/v1/jobs (list) should now include it.
	listResp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/jobs?limit=5")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	defer listResp.Body.Close()
	var listBody struct {
		Jobs []*core.Job `json:"jobs"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listBody); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	found := false
	for _, j := range listBody.Jobs {
		if j.ID == enqueueResp.Job.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected enqueued job %s in list response", enqueueResp.Job.ID)
	}
}
