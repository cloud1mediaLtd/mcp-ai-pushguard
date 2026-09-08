package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/git"
	"github.com/pushguard/pushguard/internal/runner"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/internal/verification/application"
	"github.com/pushguard/pushguard/pkg/api"
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
func NewServer(build string) *Server {
	return newServerWithClient(client.New("", client.WithBuild(build)))
}

// newServerWithClient builds a Server around an already-constructed client,
// letting tests point it at a throwaway daemon instead of the real
// ~/.pushguard socket.
func newServerWithClient(c *client.Client) *Server {
	s := &Server{client: c}

	if err := c.EnsureDaemon(client.DefaultConcurrency()); err != nil {
		// Auto-start failed (binary not locatable, spawn error, etc.) — fall
		// back to an embedded engine scoped to this process only. stdout is
		// the JSON-RPC channel, so this diagnostic goes to stderr only.
		if errors.Is(err, client.ErrIncompatible) {
			fmt.Fprintf(os.Stderr, "pushguard: %s\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "pushguard: shared daemon unavailable; submissions may use an embedded engine: %s\n", err)
		}
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
func (s *Server) ensureEmbeddedEngine() (*core.Engine, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	base, err := os.MkdirTemp("", "pushguard-embedded-")
	if err != nil {
		return nil, err
	}
	wm, err := git.NewWorktreeManager(filepath.Join(base, "worktrees"))
	if err != nil {
		return nil, err
	}
	r, err := runner.NewLocalRunner(filepath.Join(base, "logs"))
	if err != nil {
		return nil, err
	}
	s.engine = core.NewEngine(core.EngineConfig{DefaultTimeout: 15 * time.Minute}, store.NewMemoryStore(100), wm, r, nil,
		func(dir string) []string { return runner.DetectCommand(dir).Command })
	s.engine.Start()
	return s.engine, nil
}

// localJob routes session-owned jobs to their original engine, even after daemon recovery.
func (s *Server) localJob(id string) (*core.Job, bool) {
	if s.engine != nil {
		if job, err := s.engine.GetJob(id); err == nil {
			return job.Snapshot(), true
		}
	}
	return nil, false
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
				"version": "0.2.0",
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
						"description": "Git ref/commit to test (e.g., 'HEAD' or specific commit). Defaults to committed HEAD. Cannot combine with snapshot.",
					},
					"snapshot":        map[string]any{"type": "boolean", "description": "Explicitly test a working-tree snapshot; cannot combine with ref."},
					"dedupe_key":      map[string]any{"type": "string", "description": "Stable verification identity; reuses an active matching job."},
					"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "description": "Execution timeout in seconds (default: 900)."},
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
						"description": "Disabled; true is rejected. Push separately with explicit authorization.",
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
			"description": "List queue state, or find jobs already submitted for an exact commit/ref.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max number of jobs to return (default: 10).",
					},
					"ref": map[string]any{
						"type":        "string",
						"description": "Exact tested SHA or requested ref to find; use before submitting duplicate verification.",
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
						"description": "Timeout in seconds to wait (default: 1500); expiration leaves the job running.",
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
	snapshot, _ := args["snapshot"].(bool)
	dedupeKey, _ := args["dedupe_key"].(string)
	timeout, err := secondsArgument(args, "timeout_seconds", 900)
	if err != nil || autoPush || (snapshot && ref != "") {
		s.sendToolText(out, id, "Invalid submission: auto-push is disabled; snapshot/ref are exclusive; timeout must be positive integer seconds.", true)
		return
	}

	var cmd []string
	if cmdStr != "" {
		cmd = []string{cmdStr}
	}

	req := api.EnqueueRequest{
		RepoPath:  repoPath,
		Ref:       ref,
		Command:   cmd,
		Name:      name,
		AutoPush:  autoPush,
		Snapshot:  snapshot,
		Timeout:   timeout,
		DedupeKey: dedupeKey,
	}

	var job api.Job
	var pos int

	if s.client.IsDaemonRunning() {
		var remote *api.Job
		remote, pos, err = s.client.Enqueue(req)
		if remote != nil {
			job = *remote
		}
	} else {
		var engine *core.Engine
		engine, err = s.ensureEmbeddedEngine()
		if err == nil {
			var local *core.Job
			local, pos, err = engine.Enqueue(core.EnqueueRequest{RepoPath: req.RepoPath, Ref: req.Ref, Command: req.Command, Name: req.Name, AutoPush: req.AutoPush, Snapshot: req.Snapshot, Timeout: req.Timeout, DedupeKey: req.DedupeKey})
			if local != nil {
				job = application.Snapshot(local)
			}
		}
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Error enqueuing job: %s", err.Error()), true)
		return
	}

	verb := "enqueued"
	if job.Reused {
		verb = "reused from an active verification"
	}
	msg := fmt.Sprintf("Job `%s` %s.\n\n- Repository: `%s`\n- Source: %s\n- Tested SHA: `%s`\n- Requested ref: `%s`\n- Base SHA: `%s`\n- Command: `%s`\n- Queue position: %d\n\nContinue working, then collect the result. For a tracked background wait, run `pushguard await %s --timeout 1500`. Desktop notifications reach the human, not the agent.", job.ID, verb, job.RepoPath, job.SourceKind, job.Ref, job.RequestedRef, job.BaseSHA, strings.Join(job.Command, " "), pos, job.ID)

	if _, local := s.localJob(job.ID); local {
		msg = fmt.Sprintf("Job `%s` enqueued in this MCP session's private engine.\nSource: %s; tested SHA: `%s`; requested ref: `%s`; base SHA: `%s`; command: `%s`.\nCollect it using this session's pushguard_await or pushguard_status. CLI await cannot see this job, and it cannot outlive this MCP process.", job.ID, job.SourceKind, job.Ref, job.RequestedRef, job.BaseSHA, strings.Join(job.Command, " "))
	}

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

	var job api.Job
	var logs string
	var err error
	var offlineNote string

	if local, ok := s.localJob(jobID); ok {
		job = application.Snapshot(local)
		logs = local.TailLogs(scanLines)
	} else {
		var remote *api.Job
		remote, err = s.client.GetJob(jobID)
		if remote != nil {
			job = *remote
		}
		if err == nil {
			logs, _ = s.client.GetLogs(jobID, scanLines)
		} else if stored, storeErr := store.OpenReadOnly().Get(jobID); storeErr == nil {
			job, err = application.Snapshot(stored), nil
			logs = readJobLog(jobID)
			offlineNote = daemonDownNote
		}
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Error: %s", err.Error()), true)
		return
	}

	statusEmoji := "⏳"
	switch job.Status {
	case api.StatusPassed:
		statusEmoji = "✅"
	case api.StatusFailed:
		statusEmoji = "❌"
	case api.StatusRunning:
		statusEmoji = "🔄"
	case api.StatusCancelled:
		statusEmoji = "⚠️"
	}

	msg := fmt.Sprintf(
		"**Job ID**: `%s` (%s %s)\n"+
			"- **Repository**: `%s`\n"+
			"- **Command**: `%s`\n"+
			"- **Duration**: %s\n",
		job.ID, statusEmoji, strings.ToUpper(string(job.Status)), job.RepoPath, strings.Join(job.Command, " "), job.DurationString(),
	) + fmt.Sprintf("- Source: %s\n- Tested SHA: `%s`\n- Requested ref: `%s`\n- Base SHA: `%s`\n", job.SourceKind, job.Ref, job.RequestedRef, job.BaseSHA) + offlineNote

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
	ref, _ := args["ref"].(string)

	var jobs []api.Job
	offlineNote := ""
	if s.client.IsDaemonRunning() {
		var err error
		jobs, err = s.client.ListJobsByRef(limit, ref)
		if err != nil {
			s.sendToolText(out, id, fmt.Sprintf("Error listing jobs: %s", err), true)
			return
		}
	} else {
		for _, job := range store.OpenReadOnly().ListFromDisk(limit) {
			public := application.Snapshot(job)
			if ref == "" || public.Ref == ref || public.CommitSHA == ref || public.RequestedRef == ref || public.BaseSHA == ref {
				jobs = append(jobs, public)
			}
		}
		if len(jobs) > 0 {
			offlineNote = daemonDownNote
		}
	}
	byID := make(map[string]api.Job)
	for _, job := range jobs {
		byID[job.ID] = job
	}
	if s.engine != nil {
		for _, job := range s.engine.ListJobs(0) {
			public := application.Snapshot(job)
			if ref == "" || public.Ref == ref || public.CommitSHA == ref || public.RequestedRef == ref || public.BaseSHA == ref {
				byID[job.ID] = public
			}
		}
	}
	jobs = jobs[:0]
	for _, job := range byID {
		jobs = append(jobs, job)
	}
	application.SortForQueueView(jobs)
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}

	if len(jobs) == 0 {
		s.sendToolText(out, id, "No recent PushGuard jobs found.", false)
		return
	}

	// Same ordering and columns as the CLI. The queue is shared between agents,
	// so the questions are "what is running", "what is ahead of mine", and
	// "which commit is each testing" — and every gate's command string looks
	// identical, so the ref is what actually distinguishes them.
	application.SortForQueueView(jobs)

	var sb strings.Builder
	sb.WriteString("| ID | Status | Queue | Ref | Repo | Duration | Command |\n")
	sb.WriteString("|:---|:---|:---|:---|:---|:---|:---|\n")

	position := 0
	for _, j := range jobs {
		pos := "-"
		if j.Status == api.StatusQueued {
			position++
			pos = fmt.Sprintf("%d", position)
		}
		sb.WriteString(fmt.Sprintf("| `%s` | %s | %s | `%s` | %s | %s | `%s` |\n",
			j.ID, j.Status, pos, shortJobRef(j), j.Name, j.DurationString(), strings.Join(j.Command, " "),
		))
	}

	s.sendToolText(out, id, sb.String()+offlineNote, false)
}

func (s *Server) toolCancel(out io.Writer, id any, args map[string]any) {
	jobID, _ := args["job_id"].(string)
	if jobID == "" {
		s.sendToolText(out, id, "Error: job_id is required", true)
		return
	}

	var err error
	if _, ok := s.localJob(jobID); ok {
		err = s.engine.Cancel(jobID)
	} else {
		err = s.client.CancelJob(jobID)
	}

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Failed to cancel job: %s", err.Error()), true)
		return
	}

	s.sendToolText(out, id, fmt.Sprintf("Cancellation requested for job `%s`; await its terminal result.", jobID), false)
}

func (s *Server) toolAwait(out io.Writer, id any, args map[string]any) {
	jobID, _ := args["job_id"].(string)
	if jobID == "" {
		s.sendToolText(out, id, "Error: job_id is required", true)
		return
	}
	timeout, err := secondsArgument(args, "timeout_seconds", 1500)
	if err != nil {
		s.sendToolText(out, id, err.Error(), true)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	job, err := s.await(ctx, jobID)

	if err != nil {
		s.sendToolText(out, id, fmt.Sprintf("Await failed: %s", err.Error()), true)
		return
	}

	// Cancellation is not a completed verdict, regardless of the process exit code.
	resultText := fmt.Sprintf("Job `%s` completed with status: **%s** (exit code %d) in %s.",
		job.ID, job.Status, job.ExitCode, job.DurationString())
	if job.Status == api.StatusCancelled {
		resultText = fmt.Sprintf("Job `%s` was **cancelled** after %s. Checks did not finish; "+
			"there is no verdict. Re-enqueue it if you still need the gate.", job.ID, job.DurationString())
	}
	s.sendToolText(out, id, resultText, job.Status != api.StatusPassed)
}

func (s *Server) toolClean(out io.Writer, id any) {
	var report *api.CleanReport
	var err error

	if s.client.IsDaemonRunning() {
		report, err = s.client.Clean()
	} else {
		if s.engine == nil {
			s.sendToolText(out, id, "No local engine is running; no cleanup performed.", false)
			return
		}
		r := application.CleanSnapshot(s.engine.Clean())
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

// shortJobRef prefers the tested ref, preserving compatibility with older records.
func shortJobRef(j api.Job) string {
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

// daemonDownNote is appended to any answer assembled from mirrored records
// rather than a live daemon.
//
// It has to say both halves. "The daemon is down" alone invites the reader to
// treat the listing as complete history, when it is capped by the 48-hour
// prune; and a record mirrored mid-flight keeps its QUEUED or RUNNING status
// forever, because only a daemon start rewrites those. An agent that reads
// RUNNING here and waits is waiting for nothing.
const daemonDownNote = "\n\n> ⚠️ The PushGuard daemon is not running. This came from stored job records on disk, so nothing here is queued or running — a job shown as QUEUED or RUNNING did not finish and must be re-enqueued. Records are pruned 48 hours after a job finishes. Start the daemon with `make pushguard-start` to queue new work."

// readJobLog reads a finished job's log file directly, for the paths that have
// no daemon to ask and no in-process copy of the job. It duplicates the
// runner's default location rather than constructing a runner, because
// NewLocalRunner creates its directory and a read must not.
func readJobLog(jobID string) string {
	base := os.Getenv("PUSHGUARD_LOG_DIR")
	if base == "" && os.Getenv("PUSHGUARD_STATE_DIR") != "" {
		base = filepath.Join(os.Getenv("PUSHGUARD_STATE_DIR"), "logs")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache", "pushguard", "logs")
	}
	data, err := os.ReadFile(filepath.Join(base, jobID+".log"))
	if err != nil {
		return ""
	}
	return string(data)
}

func secondsArgument(args map[string]any, key string, fallback int) (time.Duration, error) {
	value, exists := args[key]
	if !exists {
		return time.Duration(fallback) * time.Second, nil
	}
	seconds, ok := value.(float64)
	if !ok || seconds <= 0 || math.Trunc(seconds) != seconds || seconds > float64(math.MaxInt64/int64(time.Second)) {
		return 0, fmt.Errorf("%s must be positive integer seconds", key)
	}
	return time.Duration(seconds) * time.Second, nil
}

func (s *Server) await(ctx context.Context, id string) (*api.Job, error) {
	if _, ok := s.localJob(id); ok {
		job, err := s.engine.AwaitJob(ctx, id, 100*time.Millisecond)
		if err != nil {
			return nil, err
		}
		result := application.Snapshot(job)
		return &result, nil
	}
	return s.client.Await(ctx, id)
}
