package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/git"
	"github.com/pushguard/pushguard/internal/runner"
	"github.com/pushguard/pushguard/internal/verification/application"
	"github.com/pushguard/pushguard/pkg/api"
)

// fakeStore is a minimal StoreInterface implementation, avoiding a real
// dependency on internal/store (which would be fine here, but keeping the
// daemon package's tests self-contained makes the fixture explicit about
// exactly what the server needs from a store).
//
// The mutex here is not incidental: TestServer_EnqueueAndFetchEndToEnd
// actually enqueues a real job, so a worker goroutine's Update() and an HTTP
// handler's List() run concurrently against this store, same as they would
// against a real one. Without this lock, go test -race catches exactly that
// (found via CI running these tests for the first time).
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]*core.Job
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: make(map[string]*core.Job)} }

func (s *fakeStore) Save(j *core.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	return nil
}

func (s *fakeStore) Get(id string) (*core.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s not found", id)
	}
	return j, nil
}

func (s *fakeStore) List(limit int) []*core.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*core.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out
}

func (s *fakeStore) Update(j *core.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	return nil
}

func (s *fakeStore) Clear(completedOnly bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
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

	wm, err := git.NewWorktreeManager(filepath.Join(dir, "worktrees"))
	if err != nil {
		t.Fatalf("failed to init worktree manager: %v", err)
	}
	r, err := runner.NewLocalRunner(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatalf("failed to init runner: %v", err)
	}
	engine := core.NewEngine(
		core.EngineConfig{MaxConcurrent: 1}, newFakeStore(), wm, r, nil,
		func(d string) []string { return runner.DetectCommand(d).Command },
	)
	engine.Start()

	server := NewServer(application.NewEngineService(engine), Config{SocketPath: socketPath})
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
	server, socketPath, cleanup := newTestServer(t)
	defer cleanup()
	server.build = "0.2.0+abc123"
	server.concurrency = 2

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
	if body["build"] != "0.2.0+abc123" || body["pid"].(float64) <= 0 || body["concurrency"].(float64) != 2 {
		t.Fatalf("incomplete daemon identity: %#v", body)
	}
}

func TestServer_EnqueueRefusesClientBuildMismatch(t *testing.T) {
	server, socketPath, cleanup := newTestServer(t)
	defer cleanup()
	server.build = "daemon-build"
	req, _ := http.NewRequest(http.MethodPost, "http://daemon/api/v1/jobs", bytes.NewBufferString(`{}`))
	req.Header.Set(api.ProtocolHeader, "2")
	req.Header.Set(api.BuildHeader, "client-build")
	resp, err := httpClientFor(socketPath).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestServer_EnqueueRejectsInvalidJSON(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, "http://daemon/api/v1/jobs", bytes.NewReader([]byte("{not json")))
	req.Header.Set(api.ProtocolHeader, "2")
	resp, err := httpClientFor(socketPath).Do(req)
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

func TestServer_RefFilterPrecedesHistoryLimit(t *testing.T) {
	mem := newFakeStore()
	base := time.Now()
	for _, job := range []*core.Job{
		{ID: "pg_match", Status: core.StatusPassed, Ref: "wanted", CreatedAt: base},
		{ID: "pg_newer", Status: core.StatusPassed, Ref: "other", CreatedAt: base.Add(time.Second)},
	} {
		if err := mem.Save(job); err != nil {
			t.Fatal(err)
		}
	}
	engine := core.NewEngine(core.EngineConfig{}, mem, nil, nil, nil, nil)
	server := NewServer(application.NewEngineService(engine), Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?ref=wanted&limit=1", nil)
	resp := httptest.NewRecorder()
	server.handleJobs(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	var body struct {
		Jobs []api.Job `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Jobs) != 1 || body.Jobs[0].ID != "pg_match" {
		t.Fatalf("filtered jobs = %+v", body.Jobs)
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

func TestServer_JobRouteRequiresExactMethodAndPath(t *testing.T) {
	_, socketPath, cleanup := newTestServer(t)
	defer cleanup()

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "post job read", method: http.MethodPost, path: "/api/v1/jobs/pg_x", want: http.StatusMethodNotAllowed},
		{name: "extra route segment", method: http.MethodPost, path: "/api/v1/jobs/pg_x/cancel/extra", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, "http://daemon"+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := httpClientFor(socketPath).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
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
	req, _ := http.NewRequest(http.MethodPost, "http://daemon/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set(api.ProtocolHeader, "2")
	resp, err := httpClientFor(socketPath).Do(req)
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
	refResp, err := httpClientFor(socketPath).Get("http://daemon/api/v1/jobs?limit=5&ref=" + enqueueResp.Job.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer refResp.Body.Close()
	var refBody struct {
		Jobs []*core.Job `json:"jobs"`
	}
	if err := json.NewDecoder(refResp.Body).Decode(&refBody); err != nil {
		t.Fatal(err)
	}
	if len(refBody.Jobs) != 1 || refBody.Jobs[0].ID != enqueueResp.Job.ID {
		t.Fatalf("ref lookup = %#v", refBody.Jobs)
	}
}

func TestOwnershipExclusiveAndReusable(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireOwnership(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireOwnership(dir); err == nil {
		second.Close()
		t.Fatal("second owner acquired live state")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireOwnership(dir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestServerPreservesLiveSocket(t *testing.T) {
	_, path, cleanup := newTestServer(t)
	defer cleanup()
	contender := NewServer(nil, Config{SocketPath: path})
	if err := contender.Start(); err == nil {
		contender.Stop(context.Background())
		t.Fatal("replaced live listener")
	}
	resp, err := httpClientFor(path).Get("http://daemon/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestServerTCPBindFailureUnwindsUnix(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dir, err := os.MkdirTemp("", "pg-bind-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	server := NewServer(nil, Config{SocketPath: path, HTTPAddr: listener.Addr().String()})
	if err := server.Start(); err == nil {
		server.Stop(context.Background())
		t.Fatal("ignored TCP bind error")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unix socket leaked: %v", err)
	}
}

func TestServerStopClosesTCP(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg-tcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	server := NewServer(nil, Config{SocketPath: filepath.Join(dir, "s"), HTTPAddr: "127.0.0.1:0"})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	address := server.tcpListener.Addr().String()
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("TCP listener survived shutdown")
	}
}

func TestServerReclaimsStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg-stale-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	server := NewServer(nil, Config{SocketPath: path})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
}

func TestConcurrentOwnershipOnlyOneWinner(t *testing.T) {
	dir := t.TempDir()
	start := make(chan struct{})
	winners := make(chan *Ownership, 12)
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			<-start
			owner, err := AcquireOwnership(dir)
			if err == nil {
				winners <- owner
			}
		})
	}
	close(start)
	group.Wait()
	close(winners)
	count := 0
	for owner := range winners {
		count++
		owner.Close()
	}
	if count != 1 {
		t.Fatalf("got %d owners, want one", count)
	}
}

func TestServerRejectsLegacySubmissionBeforeDecoding(t *testing.T) {
	_, path, cleanup := newTestServer(t)
	defer cleanup()
	resp, err := httpClientFor(path).Post("http://daemon/api/v1/jobs", "application/json", strings.NewReader(`{"timeout_seconds":900000000000}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("legacy submission returned %d", resp.StatusCode)
	}
}
