package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
)

// Client interacts with the PushGuard daemon over Unix domain socket.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// New creates a client connecting to socketPath.
func New(socketPath string) *Client {
	if socketPath == "" {
		socketPath = daemon.DefaultSocketPath()
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}

	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
	}
}

// IsDaemonRunning checks if the daemon socket is alive and responsive.
func (c *Client) IsDaemonRunning() bool {
	if _, err := os.Stat(c.socketPath); os.IsNotExist(err) {
		return false
	}
	resp, err := c.httpClient.Get("http://daemon/api/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Enqueue sends a new job to the daemon.
func (c *Client) Enqueue(req core.EnqueueRequest) (*core.Job, int, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}

	resp, err := c.httpClient.Post("http://daemon/api/v1/jobs", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, 0, fmt.Errorf("daemon connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("daemon error (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Job      *core.Job `json:"job"`
		Position int       `json:"position"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("failed to parse daemon response: %w", err)
	}

	return result.Job, result.Position, nil
}

// GetJob retrieves a job status from daemon.
func (c *Client) GetJob(id string) (*core.Job, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("http://daemon/api/v1/jobs/%s", id))
	if err != nil {
		return nil, fmt.Errorf("failed to query job: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("job %s not found", id)
	}

	var result struct {
		Job *core.Job `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Job, nil
}

// ListJobs retrieves the recent jobs list.
func (c *Client) ListJobs(limit int) ([]*core.Job, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("http://daemon/api/v1/jobs?limit=%d", limit))
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Jobs []*core.Job `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Jobs, nil
}

// CancelJob requests daemon to cancel a job.
func (c *Client) CancelJob(id string) error {
	resp, err := c.httpClient.Post(fmt.Sprintf("http://daemon/api/v1/jobs/%s/cancel", id), "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel failed: %s", string(body))
	}
	return nil
}

// GetLogs fetches the latest logs for a job.
func (c *Client) GetLogs(id string, lines int) (string, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("http://daemon/api/v1/jobs/%s/logs?lines=%d", id, lines))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Logs string `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Logs, nil
}

// EnsureDaemon returns immediately if the daemon is already reachable.
// Otherwise it spawns `<this binary> daemon --concurrency N` as a detached
// background process (its own session, so it survives the caller exiting),
// logs to ~/.pushguard/daemon.log, and polls briefly for the socket to come
// up. Every caller (MCP server, CLI, hook) can call this instead of requiring
// a human to have run `pushguard daemon` or `make pushguard-start` by hand.
func (c *Client) EnsureDaemon(concurrency int) error {
	if c.IsDaemonRunning() {
		return nil
	}

	// Never auto-spawn from a `go test` binary or when explicitly disabled —
	// otherwise every machine that runs this package's tests without a
	// daemon already up would spawn a stray background process from a
	// throwaway test binary. Callers still fall back to an embedded engine.
	if os.Getenv("PUSHGUARD_NO_AUTOSPAWN") != "" || strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		return fmt.Errorf("daemon auto-start disabled in this context")
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate pushguard binary to auto-start daemon: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve home directory: %w", err)
	}
	logDir := filepath.Join(home, ".pushguard")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to prepare daemon log dir: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exePath, "daemon", "--concurrency", strconv.Itoa(concurrency))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Detach into its own session so it isn't killed when the spawning
	// process (an MCP server tied to a Claude Code session, or a one-shot
	// CLI invocation) exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	// Release our handle; the daemon manages its own lifetime from here.
	_ = cmd.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.IsDaemonRunning() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready within 5s (check ~/.pushguard/daemon.log)")
}

// Clean requests the daemon to purge completed jobs, stale worktrees, old logs, and run GC.
func (c *Client) Clean() (*core.CleanReport, error) {
	resp, err := c.httpClient.Post("http://daemon/api/v1/clean", "application/json", nil)
	if err != nil {
		return nil, fmt.Errorf("clean failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("clean error (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Report *core.CleanReport `json:"report"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Report, nil
}

// maxAutoConcurrency caps what the environment may ask for. A gate run is a
// full build and test suite; past a handful in parallel they contend for CPU
// and the shared Postgres server's connection limit, and every job gets slower
// rather than the set finishing sooner.
const maxAutoConcurrency = 8

// DefaultConcurrency resolves how many gate workers a daemon should run, from
// PUSHGUARD_CONCURRENCY, defaulting to 1.
//
// It exists because concurrency was, in practice, unconfigurable. Both
// auto-spawn call sites passed a literal 1, and the daemon self-shuts-down
// after ~30 idle minutes — so a daemon started by hand with --concurrency 3
// reverted to 1 at the next idle window, permanently and silently. Three
// agents sharing one queue therefore serialised every ~10-minute gate behind
// every other, and it looked like a setting someone had forgotten rather than
// one that could not be set.
//
// Raising it is only safe when concurrent jobs do not collide on resources
// outside the git worktree PushGuard isolates for them — the shared test
// database above all. PUSHGUARD_JOB_ID is exported into every job's
// environment precisely so a caller's build can derive a unique name from it;
// a repository whose gate does not do that should leave this at 1.
func DefaultConcurrency() int {
	raw := strings.TrimSpace(os.Getenv("PUSHGUARD_CONCURRENCY"))
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		// A malformed value must not silently become "lots". Serialising is
		// always correct, just slower.
		return 1
	}
	if n > maxAutoConcurrency {
		return maxAutoConcurrency
	}
	return n
}
