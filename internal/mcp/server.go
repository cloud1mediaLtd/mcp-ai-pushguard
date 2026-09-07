package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// NewServer initializes an MCP server. It first tries to auto-start the
// shared daemon (so every caller ends up on one coordinated queue instead of
// silently forking off isolated engines), and only falls back to an embedded
// in-process engine if that spawn fails.
func NewServer() *Server {
	return newServerWithClient(client.New(""))
}

// newServerWithClient builds a Server around an already-constructed client,
// letting tests point it at a throwaway daemon instead of the real
// ~/.pushguard socket.
func newServerWithClient(c *client.Client) *Server {
	s := &Server{client: c}

	if err := c.EnsureDaemon(1); err != nil {
		// Auto-start failed (binary not locatable, spawn error, etc.) — fall
		// back to an embedded engine scoped to this process only. stdout is
		// the JSON-RPC channel, so this diagnostic goes to stderr only.
		fmt.Fprintf(os.Stderr, "pushguard: daemon auto-start failed, using embedded engine: %s\n", err)
		s.ensureEmbeddedEngine()
	}

	return s
}

// redisAddrFromEnv mirrors the CLI's --redis-addr default so the embedded
// fallback engine still publishes job events when Redis is reachable,
// without needing its own flag parsing.
func redisAddrFromEnv() string {
	if v, ok := os.LookupEnv("PUSHGUARD_REDIS_ADDR"); ok {
		return v
	}
	return "127.0.0.1:6379"
}

// ensureEmbeddedEngine lazily creates (once) and returns a per-process
// fallback engine. Every tool handler routes to it whenever the daemon is
// unreachable at call time — not just at server startup — because the
// daemon can legitimately disappear mid-session (its idle-shutdown timer,
// or someone killing it) after this server started with the daemon up and
// s.engine still nil. Serve() processes one request at a time off a single
// goroutine, so no locking is needed around this lazy init.
func (s *Server) ensureEmbeddedEngine() *core.Engine {
	if s.engine != nil {
		return s.engine
	}
	memStore := store.NewMemoryStore(100)
	wm, _ := git.NewWorktreeManager("")
	r, _ := runner.NewLocalRunner("")
	notifiers := []notifier.Notifier{notifier.NewMacOSNotifier(), &notifier.TerminalBellNotifier{}}
	if addr := redisAddrFromEnv(); addr != "" {
		if rn, err := notifier.NewRedisNotifier(addr, ""); err == nil {
			notifiers = append(notifiers, rn)
		}
		// Unreachable Redis is not worth logging here: this is already the
		// degraded fallback path (daemon unreachable), and stdout is the
		// JSON-RPC channel.
	}
	n := notifier.NewMultiNotifier(notifiers...)

	cfg := core.EngineConfig{
		MaxConcurrent:  1,
		DefaultTimeout: 10 * time.Minute,
	}
	s.engine = core.NewEngine(cfg, memStore, wm, r, n, func(dir string) []string {
		return runner.DetectCommand(dir).Command
	})
	s.engine.Start()
	return s.engine
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
			"description": "Enqueue a pre-push check or test suite in an isolated git worktree. Returns immediately with a job ID so your working tree is never locked and you can continue coding. NOTHING will notify you when it finishes: this call has already returned, and the daemon runs the job out of band. So keep working, then either call pushguard_status when you reach a natural stopping point, or — if the outcome blocks your next step — call pushguard_await. Do not poll pushguard_status in a loop; that gives back everything the queue bought you.",
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
				"type":       "object",
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
	// Resolve against THIS process's working directory, not the daemon's. The
	// daemon is separate and long-lived, so a relative path handed over as-is
	// resolves wherever it was started -- reported to the caller as an invalid
	// repository, which points at the wrong thing entirely.
	if abs, err := filepath.Abs(repoPath); err == nil {
		repoPath = abs
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
		job, pos, err = s.ensureEmbeddedEngine().Enqueue(req)
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
	// Read a wider window than we render. The failure is usually not in the
	// tail -- a failed pre-push run ends with whichever package happened to
	// finish last -- so the extractor needs more log than the caller wants back.
	scanLines := logLines
	if scanLines < 400 {
		scanLines = 400
	}

	var job *core.Job
	var logs string
	var err error

	if s.client.IsDaemonRunning() {
		job, err = s.client.GetJob(jobID)
		if err == nil {
			logs, _ = s.client.GetLogs(jobID, scanLines)
		}
	} else {
		job, err = s.ensureEmbeddedEngine().GetJob(jobID)
		if err == nil {
			logs = job.TailLogs(scanLines)
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
		// Answer "why did it fail" before "what did it print". Everything
		// below this line is context; this line is the result.
		if failure := core.ExtractFailure(logs, 30); failure != "" {
			msg += fmt.Sprintf("\n**Why it failed**:\n```text\n%s\n```\n", failure)
		}
		msg += fmt.Sprintf("\n**Recent Logs (last %d lines)**:\n```text\n%s\n```", logLines, tailLines(logs, logLines))
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
		jobs = s.ensureEmbeddedEngine().ListJobs(limit)
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Error listing jobs: %s", err.Error()), true)
		return
	}

	if len(jobs) == 0 {
		s.sendToolText(out, id, "No recent PushGuard jobs found.", false)
		return
	}

	// Same ordering and columns as the CLI. The queue is shared between agents,
	// so the questions are "what is running", "what is ahead of mine", and
	// "which commit is each testing" — and every gate's command string looks
	// identical, so the ref is what actually distinguishes them.
	core.SortForQueueView(jobs)

	var sb strings.Builder
	sb.WriteString("| ID | Status | Queue | Ref | Repo | Duration | Command |\n")
	sb.WriteString("|:---|:---|:---|:---|:---|:---|:---|\n")

	position := 0
	for _, j := range jobs {
		pos := "-"
		if j.Status == core.StatusQueued {
			position++
			pos = fmt.Sprintf("%d", position)
		}
		sb.WriteString(fmt.Sprintf("| `%s` | %s | %s | `%s` | %s | %s | `%s` |\n",
			j.ID, j.Status, pos, shortJobRef(j), j.Name, j.DurationString(), strings.Join(j.Command, " "),
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
		err = s.ensureEmbeddedEngine().Cancel(jobID)
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
		job, err = s.ensureEmbeddedEngine().AwaitJob(ctx, jobID, 1*time.Second)
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
	} else {
		r := s.ensureEmbeddedEngine().Clean()
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

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// shortRef is the commit a job is actually testing.
//
// Job.Ref is that commit: the ref the caller asked for, or the resolved HEAD or
// working-tree snapshot when they asked for neither. Job.CommitSHA is something
// else entirely — the branch tip at the moment of enqueue, recorded regardless
// of --ref.
//
// Preferring CommitSHA, as this first did, reports the tip for a job that is
// deliberately gating an older SHA. That is the dangerous direction: it tells
// you a release gate covered the tip when it covered a commit from before the
// fix you are relying on. Caught when a peer enqueued --ref d7ab0d01a, saw the
// enqueue echo it back correctly, and then saw this column claim 635ee5063.
func shortJobRef(j *core.Job) string {
	ref := strings.TrimSpace(j.Ref)
	if ref == "" {
		ref = strings.TrimSpace(j.CommitSHA)
	}
	if ref == "" {
		return "-"
	}
	if len(ref) > 9 {
		return ref[:9]
	}
	return ref
}
