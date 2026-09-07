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

