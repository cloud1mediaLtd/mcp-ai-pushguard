package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pushguard/pushguard/pkg/api"
	"github.com/pushguard/pushguard/pkg/client"
)

// This exercises the separately built executable with private state and no
// Redis endpoint. It never connects to the installed daemon.
func TestDaemonDuplicateStartupPreservesOwnedState(t *testing.T) {
	state, err := os.MkdirTemp("", "pg-life-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	env := append(os.Environ(), "PUSHGUARD_STATE_DIR="+state, "PUSHGUARD_REDIS_ADDR=", "PUSHGUARD_NO_AUTOSPAWN=1")
	cmd := exec.Command(binaryPath(t), "daemon", "--idle-shutdown-minutes", "0", "--redis-addr", "")
	cmd.Env = env
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var processErr error
	go func() { processErr = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	})
	socket := filepath.Join(state, "pushguard.sock")
	c := client.New(socket, client.WithBuild(buildID()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := time.NewTicker(20 * time.Millisecond)
	defer ready.Stop()
	for c.CheckCompatibility(ctx) != nil {
		select {
		case <-exited:
			t.Fatalf("daemon exited: %v", processErr)
		case <-ctx.Done():
			t.Fatal("private daemon did not become ready")
		case <-ready.C:
		}
	}
	// A record written after ownership was established models a live job. The
	// losing startup must not recover it or sweep its corresponding worktree.
	record := []byte(`{"id":"pg_owned","status":"running","repo_path":"/unused"}`)
	jobs := filepath.Join(state, "jobs")
	if err := os.MkdirAll(jobs, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(jobs, "pg_owned.json")
	if err := os.WriteFile(path, record, 0600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(state, "worktrees", "pg_owned")
	if err := os.MkdirAll(artifact, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(artifact, "keep")
	if err := os.WriteFile(sentinel, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	duplicate := exec.CommandContext(ctx, binaryPath(t), "daemon", "--redis-addr", "")
	duplicate.Env = env
	duplicateOutput, err := duplicate.CombinedOutput()
	if err == nil {
		t.Fatalf("duplicate startup succeeded: %s", duplicateOutput)
	}
	if !strings.Contains(string(duplicateOutput), "Cannot own daemon state") {
		t.Fatalf("unexpected startup failure: %s", duplicateOutput)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, record) {
		t.Fatalf("live record changed: %s, %v", got, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("owned worktree changed: %v", err)
	}
	if err := c.CheckCompatibility(ctx); err != nil {
		t.Fatalf("original daemon lost listener: %v", err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	resp, err := httpClient.Get("http://daemon/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health struct {
		Protocol int    `json:"protocol_version"`
		Build    string `json:"build"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Protocol != api.ProtocolVersion || health.Build != buildID() {
		t.Fatalf("private binary health mismatch: %+v", health)
	}
}

func TestLegacyListenerBlocksRecoveryAndOfflineCleanup(t *testing.T) {
	for _, command := range []string{"daemon", "clean"} {
		t.Run(command, func(t *testing.T) {
			state, err := os.MkdirTemp("", "pg-legacy-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(state)
			socket := filepath.Join(state, "pushguard.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			// An unhealthy legacy daemon is still an owner; failed HTTP health is not
			// evidence that its job records or worktrees are safe to recover/prune.
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })}
			go server.Serve(listener)
			defer server.Close()
			jobs := filepath.Join(state, "jobs")
			if err := os.MkdirAll(jobs, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(jobs, "pg_owned.json")
			record := []byte(`{"id":"pg_owned","status":"running","repo_path":"/unused"}`)
			if err := os.WriteFile(path, record, 0600); err != nil {
				t.Fatal(err)
			}
			artifact := filepath.Join(state, "worktrees", "pg_owned")
			if err := os.MkdirAll(artifact, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(artifact, "keep")
			if err := os.WriteFile(sentinel, []byte("owned"), 0600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(artifact, old, old); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			process := exec.CommandContext(ctx, binaryPath(t), command)
			process.Env = append(os.Environ(), "PUSHGUARD_STATE_DIR="+state, "PUSHGUARD_REDIS_ADDR=", "PUSHGUARD_NO_AUTOSPAWN=1")
			output, err := process.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "daemon socket already active") {
				t.Fatalf("legacy owner was not refused: %v, %s", err, output)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, record) {
				t.Fatalf("legacy live record changed: %s, %v", got, err)
			}
			if _, err := os.Stat(sentinel); err != nil {
				t.Fatalf("legacy owned artifact changed: %v", err)
			}
		})
	}
}
