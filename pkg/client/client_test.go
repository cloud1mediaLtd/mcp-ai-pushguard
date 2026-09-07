package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/pkg/client"
)

func TestDaemonAndClientLifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pushguard-client-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	socketPath := filepath.Join(tempDir, "test.sock")

	memStore := store.NewMemoryStore(10)
	engine := core.NewEngine(
		core.EngineConfig{MaxConcurrent: 1},
		memStore,
		nil,
		nil,
		nil,
		nil,
	)

	server := daemon.NewServer(engine, daemon.Config{SocketPath: socketPath})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start daemon server: %v", err)
	}
	defer func() {
		_ = server.Stop(context.Background())
	}()

	time.Sleep(50 * time.Millisecond)

	c := client.New(socketPath)
	if !c.IsDaemonRunning() {
		t.Fatalf("expected daemon to report running")
	}

	// Insert dummy job directly to store to test query
	job := &core.Job{
		ID:        "pg_sock_1",
		RepoPath:  "/tmp/repo",
		Status:    core.StatusPassed,
		CreatedAt: time.Now(),
	}
	_ = memStore.Save(job)

	// Test GetJob via client
	retrieved, err := c.GetJob("pg_sock_1")
	if err != nil || retrieved.ID != "pg_sock_1" {
		t.Fatalf("failed to get job via client: %v", err)
	}

	// Test ListJobs via client
	jobs, err := c.ListJobs(5)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected 1 job in list, got %d, err: %v", len(jobs), err)
	}

	// Test Clean via client
	report, err := c.Clean()
	if err != nil {
		t.Fatalf("clean via client failed: %v", err)
	}
	if report.JobsFlushed != 1 {
		t.Errorf("expected 1 job flushed, got %d", report.JobsFlushed)
	}

	// Verify job is now flushed from memory
	_, err = c.GetJob("pg_sock_1")
	if err == nil {
		t.Errorf("expected job to be removed after clean")
	}
}

func TestEnsureDaemon_NoOpWhenAlreadyRunning(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pushguard-ensure-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	socketPath := filepath.Join(tempDir, "test.sock")
	memStore := store.NewMemoryStore(10)
	engine := core.NewEngine(core.EngineConfig{MaxConcurrent: 1}, memStore, nil, nil, nil, nil)

	server := daemon.NewServer(engine, daemon.Config{SocketPath: socketPath})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start daemon server: %v", err)
	}
	defer func() { _ = server.Stop(context.Background()) }()
	time.Sleep(50 * time.Millisecond)

	c := client.New(socketPath)
	if err := c.EnsureDaemon(1); err != nil {
		t.Fatalf("EnsureDaemon should no-op when daemon already running, got: %v", err)
	}
}

func TestEnsureDaemon_RefusesToSpawnFromTestBinary(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pushguard-ensure-nospawn-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// No daemon listening on this socket, and os.Args[0] is a `go test`
	// binary (ends in ".test") — EnsureDaemon must refuse to spawn a
	// background process from it rather than leaving a stray daemon behind
	// on whatever machine runs this test suite.
	c := client.New(filepath.Join(tempDir, "nonexistent.sock"))
	start := time.Now()
	err = c.EnsureDaemon(1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected EnsureDaemon to refuse spawning from a test binary")
	}
	if elapsed > time.Second {
		t.Fatalf("expected an immediate refusal, not a poll/timeout loop; took %s", elapsed)
	}
}
