package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/pkg/client"
)

func TestMCPServer_Lifecycle(t *testing.T) {
	server := NewServer()

	// 1. Initialize
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	input += `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	input += `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"

	in := strings.NewReader(input)
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 JSON-RPC responses, got %d:\n%s", len(lines), out.String())
	}

	// Verify initialize response
	var resp1 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[0]), &resp1); err != nil {
		t.Fatalf("failed to decode response 1: %v", err)
	}
	resMap, ok := resp1.Result.(map[string]any)
	if !ok || resMap["protocolVersion"] == nil {
		t.Errorf("expected protocolVersion in response 1")
	}

	// Verify tools/list response
	var resp2 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[1]), &resp2); err != nil {
		t.Fatalf("failed to decode response 2: %v", err)
	}
	toolsResult, ok := resp2.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected tools result in response 2")
	}
	tools, ok := toolsResult["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Errorf("expected tools list to have items, got %v", toolsResult)
	}

	// Verify ping response
	var resp3 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[2]), &resp3); err != nil {
		t.Fatalf("failed to decode response 3: %v", err)
	}
	if resp3.Error != nil {
		t.Errorf("expected no error for ping, got %v", resp3.Error)
	}
}

// TestMCPServer_SurvivesDaemonDisappearingMidSession pins down the bug fixed
// alongside the daemon's idle-shutdown timer: a Server built while the daemon
// was up has a nil embedded engine, so every tool handler's "daemon is
// unreachable" branch used to nil-pointer-panic the moment the daemon went
// away mid-session (exactly what idle-shutdown now makes routine, not rare).
// It must fall back to a lazily-created embedded engine instead of crashing.
func TestMCPServer_SurvivesDaemonDisappearingMidSession(t *testing.T) {
	// A short, non-test-name-derived temp dir: unix socket paths are capped
	// around 104 bytes on macOS, and t.TempDir() embeds this (long) test
	// function name in the path.
	tempDir, err := os.MkdirTemp("", "pg-mcp-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)
	socketPath := filepath.Join(tempDir, "test.sock")

	memStore := store.NewMemoryStore(10)
	engine := core.NewEngine(core.EngineConfig{MaxConcurrent: 1}, memStore, nil, nil, nil, nil)
	daemonServer := daemon.NewServer(engine, daemon.Config{SocketPath: socketPath})
	if err := daemonServer.Start(); err != nil {
		t.Fatalf("failed to start test daemon: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	c := client.New(socketPath)
	server := newServerWithClient(c)
	if server.engine != nil {
		t.Fatalf("expected no embedded engine while the daemon is up")
	}

	// Kill the daemon out from under this already-running server, exactly
	// like the idle-shutdown timer (or a manual kill) would mid-session.
	if err := daemonServer.Stop(context.Background()); err != nil {
		t.Fatalf("failed to stop test daemon: %v", err)
	}

	repo := initTempGitRepoForMCPTest(t)
	call := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pushguard_enqueue","arguments":{"repo_path":%q,"command":"true"}}}`+"\n",
		repo,
	)

	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(call), &out); err != nil {
		t.Fatalf("Serve panicked or errored instead of falling back: %v", err)
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to decode response: %v\nraw: %s", err, out.String())
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected a tool result, got: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected the fallback enqueue to succeed, got error result: %v", result)
	}
	if server.engine == nil {
		t.Fatalf("expected the fallback embedded engine to have been created lazily")
	}
}

func initTempGitRepoForMCPTest(t *testing.T) string {
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
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test"), 0644); err != nil {
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
