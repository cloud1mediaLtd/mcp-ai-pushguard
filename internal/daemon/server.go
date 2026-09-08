package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pushguard/pushguard/internal/verification/application"
	"github.com/pushguard/pushguard/pkg/api"
)

// Server is the HTTP/Unix-socket server for PushGuard daemon.
type Server struct {
	service     application.Service
	socketPath  string
	httpAddr    string
	listener    net.Listener
	tcpListener net.Listener
	build       string
	concurrency int
	httpServer  *http.Server
}

// Config specifies daemon network configuration.
type Config struct {
	SocketPath  string
	HTTPAddr    string
	Build       string
	Concurrency int
}

// DefaultSocketPath returns ~/.pushguard/pushguard.sock
func DefaultSocketPath() string {
	if dir := os.Getenv("PUSHGUARD_STATE_DIR"); dir != "" {
		return filepath.Join(dir, "pushguard.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pushguard.sock")
	}
	return filepath.Join(home, ".pushguard", "pushguard.sock")
}

// NewServer creates a new daemon HTTP server.
func NewServer(service application.Service, cfg Config) *Server {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	return &Server{
		service:     service,
		socketPath:  cfg.SocketPath,
		httpAddr:    cfg.HTTPAddr,
		build:       cfg.Build,
		concurrency: cfg.Concurrency,
	}
}

// Start boots up the listeners and starts serving.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.handleHealth)
	mux.HandleFunc("/api/v1/clean", s.handleClean)
	mux.HandleFunc("/api/v1/jobs", s.handleJobs)
	mux.HandleFunc("/api/v1/jobs/", s.handleJobRoute)

	s.httpServer = &http.Server{
		Handler: mux,
	}

	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0700); err != nil {
		return err
	}
	// Only remove sockets positively identified as stale. Never unlink a live
	// listener or a regular file, including when another process races startup.
	if info, err := os.Lstat(s.socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("socket path is not a socket: %s", s.socketPath)
		}
		conn, dialErr := net.DialTimeout("unix", s.socketPath, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return fmt.Errorf("daemon socket already active: %s", s.socketPath)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return fmt.Errorf("cannot establish stale socket: %w", dialErr)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on unix socket: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		l.Close()
		return err
	}
	s.listener = l
	if s.httpAddr != "" {
		s.tcpListener, err = net.Listen("tcp", s.httpAddr)
		if err != nil {
			l.Close()
			s.listener = nil
			return fmt.Errorf("listen on TCP: %w", err)
		}
	}
	go s.httpServer.Serve(l)
	if s.tcpListener != nil {
		go s.httpServer.Serve(s.tcpListener)
	}

	return nil
}

// Stop gracefully shuts down the server and cleans up socket file.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	if s.httpServer != nil {
		err = s.httpServer.Shutdown(ctx)
	}
	// Close also covers listeners that Shutdown did not reach before its deadline.
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.tcpListener != nil {
		_ = s.tcpListener.Close()
	}

	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var queueDepth, activeJobs int
	var uptime time.Duration
	if s.service != nil {
		queueDepth = s.service.QueueDepth()
		activeJobs = s.service.ActiveJobCount()
		uptime = s.service.Uptime()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"protocol_version": api.ProtocolVersion,
		"build":            s.build,
		"capabilities":     []string{"immutable-refs", "explicit-snapshot", "timeout-seconds", "no-auto-push"},
		"time":             time.Now().Format(time.RFC3339),
		"uptime_seconds":   int64(uptime.Round(time.Second) / time.Second),
		"queue_depth":      queueDepth,
		"active_jobs":      activeJobs,
		"pid":              os.Getpid(),
		"concurrency":      s.concurrency,
	})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if r.Header.Get(api.ProtocolHeader) != strconv.Itoa(api.ProtocolVersion) {
			writeError(w, http.StatusUpgradeRequired, "incompatible submission protocol; update the client before submitting jobs")
			return
		}
		if clientBuild := r.Header.Get(api.BuildHeader); s.build != "" && clientBuild != s.build {
			writeError(w, http.StatusUpgradeRequired, fmt.Sprintf("daemon build %q does not match client build %q; wait for active jobs, then restart the daemon", s.build, clientBuild))
			return
		}
		var req api.EnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request json: "+err.Error())
			return
		}
		job, pos, err := s.service.Enqueue(req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"job":      job,
			"position": pos,
			"message":  fmt.Sprintf("Job %s enqueued at position %d", job.ID, pos),
		})

	case http.MethodGet:
		limit := 20
		if q := r.URL.Query().Get("limit"); q != "" {
			if l, err := strconv.Atoi(q); err == nil && l > 0 {
				limit = l
			}
		}
		// Filter before limiting so `list --ref <sha>` can find a completed
		// gate even when newer unrelated jobs fill the default history page.
		jobs := s.service.ListJobs(0)
		if ref := strings.TrimSpace(r.URL.Query().Get("ref")); ref != "" {
			filtered := jobs[:0]
			for _, job := range jobs {
				if job.Ref == ref || job.CommitSHA == ref || job.RequestedRef == ref || job.BaseSHA == ref {
					filtered = append(filtered, job)
				}
			}
			jobs = filtered
		}
		if len(jobs) > limit {
			jobs = jobs[:limit]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"jobs": jobs,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleJobRoute(w http.ResponseWriter, r *http.Request) {
	// Path format: /api/v1/jobs/{id} or /api/v1/jobs/{id}/cancel or /api/v1/jobs/{id}/logs
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		writeError(w, http.StatusBadRequest, "invalid job url")
		return
	}
	jobID := parts[3]

	if len(parts) == 4 {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "use GET to retrieve a job")
			return
		}
		job, err := s.service.GetJob(jobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"job": job,
		})
		return
	}
	if len(parts) != 5 {
		writeError(w, http.StatusNotFound, "unknown job route")
		return
	}

	subAction := parts[4]
	switch subAction {
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "use POST to cancel")
			return
		}
		if err := s.service.Cancel(jobID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"message": fmt.Sprintf("job %s cancelled", jobID),
		})

	case "logs":
		lines := 100
		if q := r.URL.Query().Get("lines"); q != "" {
			if l, err := strconv.Atoi(q); err == nil && l > 0 {
				lines = l
			}
		}
		logs, err := s.service.Logs(jobID, lines)
		if err != nil {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":   jobID,
			"logs": logs,
		})

	default:
		writeError(w, http.StatusNotFound, "unknown action")
	}
}

func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}
	report := s.service.Clean()
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "PushGuard queue and resources cleaned successfully",
		"report":  report,
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
	})
}
