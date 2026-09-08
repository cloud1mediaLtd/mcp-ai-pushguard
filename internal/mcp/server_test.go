package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/internal/verification/application"
	"github.com/pushguard/pushguard/pkg/api"
	"github.com/pushguard/pushguard/pkg/client"
)

func TestMCPServer_Lifecycle(t *testing.T) {
	server := newServerWithClient(client.New(filepath.Join(t.TempDir(), "absent.sock")))

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
	daemonServer := daemon.NewServer(application.NewEngineService(engine), daemon.Config{SocketPath: socketPath})
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

// seedMirroredJob writes one job record into $HOME/.pushguard/jobs, the way
// DurableStore.persist does inside the daemon.
func seedMirroredJob(t *testing.T, home, id, status string) {
	t.Helper()
	dir := filepath.Join(home, ".pushguard", "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"id":%q,"name":"c1m-server","status":%q,"repo_path":"/repo/c1m-server",`+
		`"branch":"main","ref":"d7ab0d01abcdef","command":["make","pre-push"],"exit_code":0,`+
		`"created_at":"2026-09-07T10:00:00Z","started_at":"2026-09-07T10:00:01Z","finished_at":"2026-09-07T10:11:00Z"}`,
		id, status)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
}

func callTool(t *testing.T, server *Server, call string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(call+"\n"), &out); err != nil {
		t.Fatalf("Serve errored: %v", err)
	}
	var resp JSONRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, out.String())
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected a tool result, got: %v", resp)
	}
	return result
}

func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	var sb strings.Builder
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

// The queue is shared between agents. With the daemon idled out, the embedded
// fallback engine knows only jobs THIS process ran — so pushguard_list reported
// "No recent PushGuard jobs found", which is not "I cannot see the queue" but a
// positive claim that it is empty. An agent believing it starts work another
// agent is already gating.
func TestMCPServer_ListReadsMirroredRecordsWhenDaemonIsDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedMirroredJob(t, home, "pg_mirrored1", "passed")

	server := newServerWithClient(client.New(filepath.Join(home, "nonexistent.sock")))
	text := toolText(t, callTool(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pushguard_list","arguments":{}}}`))

	if !strings.Contains(text, "pg_mirrored1") {
		t.Fatalf("expected the mirrored job in the listing, got %q", text)
	}
	if strings.Contains(text, "No recent PushGuard jobs found") {
		t.Fatalf("claimed the queue was empty while records existed on disk: %q", text)
	}
	if !strings.Contains(text, "daemon is not running") {
		t.Fatalf("expected the answer to say it came from disk, got %q", text)
	}
}

// Same defect on the single-job path: "job not found" is indistinguishable
// from a typo'd id, and sends the caller looking for a bug in their own command
// rather than collecting a result that is sitting on disk.
func TestMCPServer_StatusReadsMirroredRecordWhenDaemonIsDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedMirroredJob(t, home, "pg_mirrored2", "failed")

	server := newServerWithClient(client.New(filepath.Join(home, "nonexistent.sock")))
	result := callTool(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pushguard_status","arguments":{"job_id":"pg_mirrored2"}}}`)

	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected the stored record to answer, got an error result: %v", result)
	}
	text := toolText(t, result)
	if !strings.Contains(text, "pg_mirrored2") || !strings.Contains(text, "FAILED") {
		t.Fatalf("expected the stored record's id and status, got %q", text)
	}
	if !strings.Contains(text, "daemon is not running") {
		t.Fatalf("expected the answer to say it came from disk, got %q", text)
	}
}

// An unknown id must still be an error. Falling back to disk must not turn
// "no such job" into a confident empty answer.
func TestMCPServer_StatusStillErrorsForUnknownJobWithDaemonDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	server := newServerWithClient(client.New(filepath.Join(home, "nonexistent.sock")))
	result := callTool(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pushguard_status","arguments":{"job_id":"pg_nosuchjob"}}}`)

	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("expected an error result for an unknown job, got %v", result)
	}
}

func TestAwaitHonorsContextDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg-await-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	mem := store.NewMemoryStore(10)
	if err := mem.Save(&core.Job{ID: "pg_waiting", Status: core.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	engine := core.NewEngine(core.EngineConfig{}, mem, nil, nil, nil, nil)
	srv := daemon.NewServer(application.NewEngineService(engine), daemon.Config{SocketPath: filepath.Join(dir, "s")})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(context.Background())
	server := &Server{client: client.New(filepath.Join(dir, "s"))}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = server.await(ctx, "pg_waiting")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline did not bound await")
	}
}

func TestEmbeddedJobRemainsOwnedAfterDaemonRecovery(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	localStore := store.NewMemoryStore(10)
	local := core.NewEngine(core.EngineConfig{}, localStore, nil, nil, nil, nil)
	job, _, err := local.Enqueue(core.EnqueueRequest{RepoPath: initTempGitRepoForMCPTest(t), Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Stop()

	remote := core.NewEngine(core.EngineConfig{}, store.NewMemoryStore(10), nil, nil, nil, nil)
	srv := daemon.NewServer(application.NewEngineService(remote), daemon.Config{SocketPath: filepath.Join(dir, "s")})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(context.Background())
	server := &Server{client: client.New(filepath.Join(dir, "s")), engine: local}
	for _, tool := range []string{"status", "list", "cancel"} {
		var out bytes.Buffer
		args := map[string]any{"job_id": job.ID}
		switch tool {
		case "status":
			server.toolStatus(&out, 1, args)
		case "list":
			server.toolList(&out, 1, args)
		case "cancel":
			server.toolCancel(&out, 1, args)
		}
		var resp JSONRPCResponse
		if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
			t.Fatal(err)
		}
		result := resp.Result.(map[string]any)
		if result["isError"] == true {
			t.Fatalf("%s: %s", tool, out.String())
		}
		if !strings.Contains(out.String(), job.ID) {
			t.Fatalf("%s omitted local job", tool)
		}
	}
	result, err := server.await(context.Background(), job.ID)
	if err != nil || result.Status != api.StatusCancelled {
		t.Fatalf("local await: %v %v", result, err)
	}
}

func TestSecondsArgumentRejectsInvalidValues(t *testing.T) {
	for _, value := range []any{float64(0), float64(-1), 1.5, "10", math.Inf(1)} {
		if _, err := secondsArgument(map[string]any{"timeout_seconds": value}, "timeout_seconds", 900); err == nil {
			t.Fatalf("accepted %v", value)
		}
	}
}
