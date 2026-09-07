package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/git"
	"github.com/pushguard/pushguard/internal/notifier"
	"github.com/pushguard/pushguard/internal/runner"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/pkg/client"
)

// JSONRPCRequest represents an incoming MCP request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents an outgoing MCP response.
type JSONRPCResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id,omitempty"`
	Result  any          `json:"result,omitempty"`
	Error   *RPCErrorObj `json:"error,omitempty"`
}

// RPCErrorObj represents a JSON-RPC error.
type RPCErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server handles MCP communication over stdio.
type Server struct {
	client *client.Client
	engine *core.Engine
}

// NewServer initializes an MCP server. It connects to the daemon if available,
// otherwise spins up an embedded engine for standalone stdio usage.
func NewServer() *Server {
	c := client.New("")
	var eng *core.Engine

	if !c.IsDaemonRunning() {
		// Boot up embedded engine
		memStore := store.NewMemoryStore(100)
		wm, _ := git.NewWorktreeManager("")
		r, _ := runner.NewLocalRunner("")
		n := notifier.NewMultiNotifier(notifier.NewMacOSNotifier(), &notifier.TerminalBellNotifier{})

		cfg := core.EngineConfig{
			MaxConcurrent:  1,
			DefaultTimeout: 10 * time.Minute,
		}
		eng = core.NewEngine(cfg, memStore, wm, r, n, func(dir string) []string {
			return runner.DetectCommand(dir).Command
		})
		eng.Start()
	}

	return &Server{
		client: c,
		engine: eng,
	}
}

// Serve reads JSON-RPC requests from in and writes to out.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// Support larger payloads up to 1MB
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(out, nil, -32700, "Parse error")
			continue
		}

		s.handleRequest(out, req)
	}

	if s.engine != nil {
		s.engine.Stop()
	}

	return scanner.Err()
}

func (s *Server) handleRequest(out io.Writer, req JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		s.sendResult(out, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "pushguard",
				"version": "0.1.0",
			},
		})

	case "notifications/initialized":
		// No response required for notification

	case "ping":
		s.sendResult(out, req.ID, map[string]any{})

	case "tools/list":
		s.handleToolsList(out, req.ID)

	case "tools/call":
		s.handleToolCall(out, req.ID, req.Params)

	default:
		if req.ID != nil {
			s.sendError(out, req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
		}
	}
}

func (s *Server) handleToolsList(out io.Writer, id any) {
	tools := []map[string]any{
		{
			"name":        "pushguard_enqueue",
			"description": "Enqueue a pre-push check or test suite in an isolated git worktree. Returns immediately with a job ID so your working tree is never locked and you can continue coding.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo_path": map[string]any{
						"type":        "string",
						"description": "Path to the git repository (default: current directory).",
					},
					"ref": map[string]any{
						"type":        "string",
						"description": "Git ref/commit to test (e.g., 'HEAD' or specific commit). If dirty files exist, PushGuard automatically creates an isolated snapshot without touching your working tree.",
					},
					"command": map[string]any{
						"type":        "string",
						"description": "Optional test command to run (e.g. 'go test ./...', 'pnpm test'). Auto-detected if omitted.",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Optional human-readable label for the job.",
					},
					"auto_push": map[string]any{
						"type":        "boolean",
						"description": "If true, automatically pushes to origin branch when all tests pass.",
					},
				},
				"required": []string{},
			},
		},
		{
			"name":        "pushguard_status",
			"description": "Check the status, duration, and tail logs of a queued or running PushGuard job.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The PushGuard job ID (e.g., pg_a1b2c3d4).",
					},
					"log_lines": map[string]any{
						"type":        "integer",
						"description": "Number of recent log lines to return (default: 50).",
					},
				},
				"required": []string{"job_id"},
			},
		},
		{
			"name":        "pushguard_list",
			"description": "List all queued, running, and recently finished PushGuard verification jobs.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max number of jobs to return (default: 10).",
					},
				},
			},
		},
		{
			"name":        "pushguard_cancel",
			"description": "Cancel a queued or currently executing PushGuard test job.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID to abort.",
					},
				},
				"required": []string{"job_id"},
			},
		},
		{
			"name":        "pushguard_await",
			"description": "Wait synchronously for a job to complete (or fail) up to a timeout. Use this only when the agent explicitly needs the test outcome before proceeding.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID to wait for.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Timeout in seconds to wait (default: 300).",
					},
				},
				"required": []string{"job_id"},
			},
		},
		{
			"name":        "pushguard_clean",
			"description": "Performs queue maintenance: clears completed jobs from memory, prunes stale git worktrees, cleans up old test logs, and forces runtime garbage collection to free memory.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
		},
	}

	s.sendResult(out, id, map[string]any{
		"tools": tools,
	})
}

func (s *Server) handleToolCall(out io.Writer, id any, rawParams json.RawMessage) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &call); err != nil {
		s.sendError(out, id, -32602, "Invalid params")
		return
	}

	switch call.Name {
	case "pushguard_enqueue":
		s.toolEnqueue(out, id, call.Arguments)
	case "pushguard_status":
		s.toolStatus(out, id, call.Arguments)
	case "pushguard_list":
		s.toolList(out, id, call.Arguments)
	case "pushguard_cancel":
		s.toolCancel(out, id, call.Arguments)
	case "pushguard_await":
		s.toolAwait(out, id, call.Arguments)
	case "pushguard_clean":
		s.toolClean(out, id)
	default:
		s.sendError(out, id, -32601, fmt.Sprintf("Tool not found: %s", call.Name))
	}
}

func (s *Server) toolEnqueue(out io.Writer, id any, args map[string]any) {
	repoPath, _ := args["repo_path"].(string)
	if repoPath == "" {
		repoPath = "."
	}
	ref, _ := args["ref"].(string)
	cmdStr, _ := args["command"].(string)
	name, _ := args["name"].(string)
	autoPush, _ := args["auto_push"].(bool)

	var cmd []string
	if cmdStr != "" {
		cmd = strings.Fields(cmdStr)
	}

	req := core.EnqueueRequest{
		RepoPath: repoPath,
		Ref:      ref,
		Command:  cmd,
		Name:     name,
		AutoPush: autoPush,
	}

	var job *core.Job
	var pos int
	var err error

	if s.client.IsDaemonRunning() {
		job, pos, err = s.client.Enqueue(req)
	} else {
		job, pos, err = s.engine.Enqueue(req)
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Error enqueuing job: %s", err.Error()), true)
		return
	}

	msg := fmt.Sprintf(
		"✅ PushGuard Job Enqueued!\n\n"+
			"- **Job ID**: `%s`\n"+
			"- **Repository**: `%s`\n"+
			"- **Branch/Ref**: `%s`\n"+
			"- **Command**: `%s`\n"+
			"- **Queue Position**: %d\n"+
			"- **Auto-Push on Pass**: %v\n\n"+
			"💡 *Your working tree remains 100%% unlocked.* Tests are running in an isolated git worktree.\n"+
			"PushGuard will ping you via macOS notification when done. You can also call `pushguard_status(job_id=%q)`.",
		job.ID, job.RepoPath, job.Ref, strings.Join(job.Command, " "), pos, job.AutoPush, job.ID,
	)

	s.sendToolText(out, id, msg, false)
}

func (s *Server) toolStatus(out io.Writer, id any, args map[string]any) {
	jobID, _ := args["job_id"].(string)
	if jobID == "" {
		s.sendToolText(out, id, "Error: job_id is required", true)
		return
	}
	logLines := 50
	if n, ok := args["log_lines"].(float64); ok && n > 0 {
		logLines = int(n)
	}

	var job *core.Job
	var logs string
	var err error

	if s.client.IsDaemonRunning() {
		job, err = s.client.GetJob(jobID)
		if err == nil {
			logs, _ = s.client.GetLogs(jobID, logLines)
		}
	} else {
		job, err = s.engine.GetJob(jobID)
		if err == nil {
			logs = job.TailLogs(logLines)
		}
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Error: %s", err.Error()), true)
		return
	}

	statusEmoji := "⏳"
	switch job.Status {
	case core.StatusPassed:
		statusEmoji = "✅"
	case core.StatusFailed:
		statusEmoji = "❌"
	case core.StatusRunning:
		statusEmoji = "🔄"
	case core.StatusCancelled:
		statusEmoji = "⚠️"
	}

	msg := fmt.Sprintf(
		"**Job ID**: `%s` (%s %s)\n"+
			"- **Repository**: `%s`\n"+
			"- **Command**: `%s`\n"+
			"- **Duration**: %s\n",
		job.ID, statusEmoji, strings.ToUpper(string(job.Status)), job.RepoPath, strings.Join(job.Command, " "), job.DurationString(),
	)

	if job.Error != "" {
		msg += fmt.Sprintf("- **Error**: %s\n", job.Error)
	}
	if job.IsFinished() {
		msg += fmt.Sprintf("- **Exit Code**: %d\n", job.ExitCode)
	}
	if logs != "" {
		msg += fmt.Sprintf("\n**Recent Logs (last %d lines)**:\n```text\n%s\n```", logLines, logs)
	}

	s.sendToolText(out, id, msg, false)
}

func (s *Server) toolList(out io.Writer, id any, args map[string]any) {
	limit := 10
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	var jobs []*core.Job
	var err error

	if s.client.IsDaemonRunning() {
		jobs, err = s.client.ListJobs(limit)
	} else {
		jobs = s.engine.ListJobs(limit)
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Error listing jobs: %s", err.Error()), true)
		return
	}

	if len(jobs) == 0 {
		s.sendToolText(out, id, "No recent PushGuard jobs found.", false)
		return
	}

	var sb strings.Builder
	sb.WriteString("| ID | Status | Repo | Duration | Command |\n")
	sb.WriteString("|:---|:---|:---|:---|:---|\n")

	for _, j := range jobs {
		sb.WriteString(fmt.Sprintf("| `%s` | %s | %s | %s | `%s` |\n",
			j.ID, j.Status, j.Name, j.DurationString(), strings.Join(j.Command, " "),
		))
	}

	s.sendToolText(out, id, sb.String(), false)
}

func (s *Server) toolCancel(out io.Writer, id any, args map[string]any) {
	jobID, _ := args["job_id"].(string)
	if jobID == "" {
		s.sendToolText(out, id, "Error: job_id is required", true)
		return
	}

	var err error
	if s.client.IsDaemonRunning() {
		err = s.client.CancelJob(jobID)
	} else {
		err = s.engine.Cancel(jobID)
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Failed to cancel job: %s", err.Error()), true)
		return
	}

	s.sendToolText(out, id, fmt.Sprintf("Job `%s` has been cancelled.", jobID), false)
}

func (s *Server) toolAwait(out io.Writer, id any, args map[string]any) {
	jobID, _ := args["job_id"].(string)
	if jobID == "" {
		s.sendToolText(out, id, "Error: job_id is required", true)
		return
	}
	timeoutSec := 300
	if t, ok := args["timeout_seconds"].(float64); ok && t > 0 {
		timeoutSec = int(t)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	var job *core.Job
	var err error

	if s.client.IsDaemonRunning() {
		// Poll daemon
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			job, err = s.client.GetJob(jobID)
			if err != nil || job.IsFinished() {
				break
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
				break
			case <-ticker.C:
			}
		}
	} else {
		job, err = s.engine.AwaitJob(ctx, jobID, 1*time.Second)
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Await failed: %s", err.Error()), true)
		return
	}

	resultText := fmt.Sprintf("Job `%s` completed with status: **%s** (exit code %d) in %s.",
		job.ID, job.Status, job.ExitCode, job.DurationString())
	s.sendToolText(out, id, resultText, job.Status != core.StatusPassed)
}

func (s *Server) toolClean(out io.Writer, id any) {
	var report *core.CleanReport
	var err error

	if s.client.IsDaemonRunning() {
		report, err = s.client.Clean()
	} else if s.engine != nil {
		r := s.engine.Clean()
		report = &r
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Clean error: %s", err.Error()), true)
		return
	}

	msg := fmt.Sprintf("🧹 **PushGuard Cleanup Complete**\n\n"+
		"- **Completed Jobs Flushed**: %d\n"+
		"- **Orphaned Worktrees Pruned**: %d\n"+
		"- **Stale Logs Pruned**: %d\n"+
		"- **Active Memory**: %.2f MB\n",
		report.JobsFlushed,
		report.WorktreesPruned,
		report.LogsPruned,
		float64(report.AllocBytesAfter)/(1024*1024),
	)
	s.sendToolText(out, id, msg, false)
}

func (s *Server) sendToolText(out io.Writer, id any, text string, isError bool) {
	s.sendResult(out, id, map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": text,
			},
		},
		"isError": isError,
	})
}

func (s *Server) sendResult(out io.Writer, id any, result any) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	_, _ = fmt.Fprintln(out, string(data))
}

func (s *Server) sendError(out io.Writer, id any, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCErrorObj{
			Code:    code,
			Message: message,
		},
	}
	data, _ := json.Marshal(resp)
	_, _ = fmt.Fprintln(out, string(data))
}
