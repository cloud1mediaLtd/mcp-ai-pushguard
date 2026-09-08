package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/pkg/api"
)

// Client interacts with the PushGuard daemon over Unix domain socket.
type Client struct {
	socketPath string
	httpClient *http.Client
	build      string
}

// Option configures a daemon client.
type Option func(*Client)

// WithBuild identifies the binary using the daemon. Supplying it makes build
// skew a hard submission error rather than an invisible source of surprises.
func WithBuild(build string) Option { return func(c *Client) { c.build = build } }

// New creates a client connecting to socketPath.
func New(socketPath string, options ...Option) *Client {
	if socketPath == "" {
		socketPath = daemon.DefaultSocketPath()
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}

	c := &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
	}
	for _, option := range options {
		option(c)
	}
	return c
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
func (c *Client) Enqueue(req api.EnqueueRequest) (*api.Job, int, error) {
	if err := c.CheckCompatibility(context.Background()); err != nil {
		return nil, 0, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, "http://daemon/api/v1/jobs", bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(api.ProtocolHeader, strconv.Itoa(api.ProtocolVersion))
	httpReq.Header.Set(api.BuildHeader, c.build)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("daemon connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("daemon error (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Job      *api.Job `json:"job"`
		Position int      `json:"position"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("failed to parse daemon response: %w", err)
	}

	if result.Job == nil || result.Job.ID == "" {
		return nil, 0, fmt.Errorf("daemon response omitted accepted job")
	}
	return result.Job, result.Position, nil
}

// GetJob retrieves a job status from daemon.
func (c *Client) GetJob(id string) (*api.Job, error) {
	return c.GetJobContext(context.Background(), id)
}

// GetJobContext retrieves a job with cancellation and deadlines.
func (c *Client) GetJobContext(ctx context.Context, id string) (*api.Job, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://daemon/api/v1/jobs/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query job: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon query failed (%d)", resp.StatusCode)
	}
	var result struct {
		Job *api.Job `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Job == nil {
		return nil, fmt.Errorf("daemon response omitted job")
	}
	return result.Job, nil
}

// ListJobs retrieves the recent jobs list.
func (c *Client) ListJobs(limit int) ([]api.Job, error) {
	return c.ListJobsByRef(limit, "")
}

// ListJobsByRef returns jobs associated with an exact tested SHA or requested
// ref. It lets a second agent discover whether its commit is already queued or
// has a recorded verdict before submitting duplicate work.
func (c *Client) ListJobsByRef(limit int, ref string) ([]api.Job, error) {
	endpoint := fmt.Sprintf("http://daemon/api/v1/jobs?limit=%d", limit)
	if ref != "" {
		endpoint += "&ref=" + url.QueryEscape(ref)
	}
	resp, err := c.httpClient.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon list failed (%d)", resp.StatusCode)
	}

	var result struct {
		Jobs []api.Job `json:"jobs"`
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
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon logs failed (%d)", resp.StatusCode)
	}

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
	probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := c.CheckCompatibility(probeCtx)
	cancel()
	if err == nil || errors.Is(err, ErrIncompatible) {
		return err
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
	if dir := os.Getenv("PUSHGUARD_STATE_DIR"); dir != "" {
		logDir = dir
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to prepare daemon log dir: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exePath, "daemon", "--concurrency", strconv.Itoa(concurrency), "--socket", c.socketPath)
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

	ctx, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReady()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := c.CheckCompatibility(ctx)
		if err == nil || errors.Is(err, ErrIncompatible) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon did not become ready within 5s (check %s): %w", filepath.Join(logDir, "daemon.log"), ctx.Err())
		case <-ticker.C:
		}
	}

}

// Clean requests the daemon to purge completed jobs, stale worktrees, old logs, and run GC.
func (c *Client) Clean() (*api.CleanReport, error) {
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
		Report *api.CleanReport `json:"report"`
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
//
// The environment variable alone was not enough, and the reason is the same
// idle-shutdown that made the flag useless. Auto-spawn inherits the
// environment of whichever caller happens to wake the daemon — so one agent
// exporting PUSHGUARD_CONCURRENCY=3 gets three workers until the queue empties
// for thirty minutes, after which the next enqueue from a session without that
// export silently brings the daemon back at 1. The setting would appear to
// work and then quietly stop, which is worse than not having it.
//
// So the file is the durable answer and the variable is the override. Neither
// requires editing a shell profile, which is the wrong place for a setting
// that belongs to the machine's queue rather than to one person's terminal.
func DefaultConcurrency() int {
	if n, ok := parseConcurrency(os.Getenv("PUSHGUARD_CONCURRENCY")); ok {
		return n
	}
	if home, err := os.UserHomeDir(); err == nil {
		if body, err := os.ReadFile(filepath.Join(home, ".pushguard", "concurrency")); err == nil {
			if n, ok := parseConcurrency(string(body)); ok {
				return n
			}
		}
	}
	return 1
}

// parseConcurrency reports a usable worker count, or false to fall through.
//
// A malformed value falls through rather than becoming "lots": serialising is
// always correct and merely slower, whereas over-parallelising collides on the
// shared test database. Empty is not an error — it is simply "not set here".
func parseConcurrency(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 1 {
		return 0, false
	}
	if n > maxAutoConcurrency {
		return maxAutoConcurrency, true
	}
	return n, true
}

// ErrIncompatible indicates a live daemon that cannot safely accept this client's jobs.
var ErrIncompatible = errors.New("incompatible daemon; wait for active jobs to finish, then restart the daemon with the updated binary")
var ErrNoVerdict = errors.New("daemon unavailable; no completed verdict is available")
var ErrJobNotFound = errors.New("job not found")

// CheckCompatibility validates the submission contract without changing daemon state.
func (c *Client) CheckCompatibility(ctx context.Context) error {
	health, err := c.Health(ctx)
	if err != nil {
		return err
	}
	if health.Protocol != api.ProtocolVersion {
		return fmt.Errorf("%w (server protocol %d, client protocol %d, build %q)", ErrIncompatible, health.Protocol, api.ProtocolVersion, health.Build)
	}
	if c.build != "" && health.Build != c.build {
		return fmt.Errorf("%w: daemon build %q does not match client build %q; wait for active jobs, then restart the daemon", ErrIncompatible, health.Build, c.build)
	}
	for _, required := range []string{"immutable-refs", "explicit-snapshot", "timeout-seconds", "no-auto-push"} {
		if !health.HasCapability(required) {
			return fmt.Errorf("%w: missing capability %s", ErrIncompatible, required)
		}
	}
	return nil
}

// Health returns daemon build and live queue state without changing daemon
// state. Unlike CheckCompatibility, it is useful for diagnosing a mismatch.
func (c *Client) Health(ctx context.Context) (api.Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://daemon/api/v1/health", nil)
	if err != nil {
		return api.Health{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return api.Health{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.Health{}, fmt.Errorf("%w: health status %d", ErrIncompatible, resp.StatusCode)
	}
	var health api.Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return api.Health{}, fmt.Errorf("%w: invalid health response", ErrIncompatible)
	}
	return health, nil
}

// Build returns the configured local client build for status output.
func (c *Client) Build() string { return c.build }

// Await waits for a verdict, bounded by ctx. A wait deadline never cancels the job.
// If the daemon disappears, persisted terminal results remain usable.
func (c *Client) Await(ctx context.Context, id string) (*api.Job, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := c.GetJobContext(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			stored, diskErr := store.OpenReadOnly().Get(id)
			if diskErr == nil && stored.IsFinished() {
				result := apiJobFromCore(stored)
				return &result, nil
			}
			if diskErr == nil {
				return nil, fmt.Errorf("%w: %s", ErrNoVerdict, id)
			}
			if errors.Is(err, ErrJobNotFound) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrNoVerdict, err)
		}
		if job.IsFinished() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func apiJobFromCore(job *core.Job) api.Job {
	j := job.Snapshot()
	return api.Job{ID: j.ID, Name: j.Name, RepoPath: j.RepoPath, Ref: j.Ref, RequestedRef: j.RequestedRef, SourceKind: j.SourceKind, BaseSHA: j.BaseSHA, Timeout: j.Timeout, CommitSHA: j.CommitSHA, Branch: j.Branch, Command: append([]string(nil), j.Command...), AutoPush: j.AutoPush, Status: api.JobStatus(j.Status), ExitCode: j.ExitCode, Error: j.Error, CreatedAt: j.CreatedAt, StartedAt: copyTime(j.StartedAt), FinishedAt: copyTime(j.FinishedAt), Duration: j.Duration, WorktreeDir: j.WorktreeDir, LogFilePath: j.LogFilePath, DaemonBuild: j.DaemonBuild, DedupeKey: j.DedupeKey, Reused: j.Reused}
}

func copyTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
