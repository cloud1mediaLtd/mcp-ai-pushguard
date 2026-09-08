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
	"github.com/pushguard/pushguard/internal/verification/application"
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

	server := daemon.NewServer(application.NewEngineService(engine), daemon.Config{SocketPath: socketPath})
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

	server := daemon.NewServer(application.NewEngineService(engine), daemon.Config{SocketPath: socketPath})
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

// Concurrency was unconfigurable in practice: both auto-spawn call sites passed
// a literal 1, and the daemon's ~30-minute idle shutdown meant a hand-started
// --concurrency 3 reverted to 1 at the next quiet window. Three agents on one
// queue then serialised every ~10-minute gate behind every other.
func TestDefaultConcurrency(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want int
	}{
		{"unset serialises, as before", "", 1},
		{"an explicit value is honoured", "3", 3},
		{"one is still one", "1", 1},
		// A malformed value must never become "lots": serialising is always
		// correct, merely slower, whereas over-parallelising a gate collides on
		// the shared test database.
		{"garbage serialises", "lots", 1},
		{"empty-ish serialises", "   ", 1},
		{"zero serialises", "0", 1},
		{"negative serialises", "-4", 1},
		{"absurd values are capped", "500", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // no config file in play for these cases
			t.Setenv("PUSHGUARD_CONCURRENCY", tc.env)
			if got := client.DefaultConcurrency(); got != tc.want {
				t.Fatalf("PUSHGUARD_CONCURRENCY=%q gave %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// The environment variable alone could not hold a raise in place.
//
// Auto-spawn inherits the environment of whichever caller wakes the daemon, and
// the daemon idles out after ~30 minutes. So one agent exporting
// PUSHGUARD_CONCURRENCY=3 gets three workers until the queue goes quiet, after
// which the next enqueue from a session without that export brings it back at
// 1 — a setting that appears to work and then silently stops.
func TestDefaultConcurrencyPersistsInAFile(t *testing.T) {
	writeConfig := func(t *testing.T, body string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PUSHGUARD_CONCURRENCY", "")
		dir := filepath.Join(home, ".pushguard")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "concurrency"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a stored value survives a caller with no environment set", func(t *testing.T) {
		writeConfig(t, "3\n")
		if got := client.DefaultConcurrency(); got != 3 {
			t.Fatalf("stored concurrency = %d, want 3", got)
		}
	})

	t.Run("the environment overrides the file", func(t *testing.T) {
		writeConfig(t, "3")
		t.Setenv("PUSHGUARD_CONCURRENCY", "2")
		if got := client.DefaultConcurrency(); got != 2 {
			t.Fatalf("env should win, got %d", got)
		}
	})

	// Same rule as the environment: serialising is always correct, so a
	// corrupt file must not become "lots".
	for _, body := range []string{"lots", "0", "-4", "   "} {
		t.Run("a malformed file serialises: "+body, func(t *testing.T) {
			writeConfig(t, body)
			if got := client.DefaultConcurrency(); got != 1 {
				t.Fatalf("body %q gave %d, want 1", body, got)
			}
		})
	}

	t.Run("an absurd stored value is capped", func(t *testing.T) {
		writeConfig(t, "500")
		if got := client.DefaultConcurrency(); got != 8 {
			t.Fatalf("capped concurrency = %d, want 8", got)
		}
	})
}
