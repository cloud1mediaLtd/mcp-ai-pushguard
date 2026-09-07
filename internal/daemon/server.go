package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pushguard/pushguard/internal/core"
)

// Server is the HTTP/Unix-socket server for PushGuard daemon.
type Server struct {
	engine     *core.Engine
	socketPath string
	httpAddr   string
	listener   net.Listener
	httpServer *http.Server
}

// Config specifies daemon network configuration.
type Config struct {
	SocketPath string
	HTTPAddr   string
}

// DefaultSocketPath returns ~/.pushguard/pushguard.sock
func DefaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pushguard.sock")
	}
	return filepath.Join(home, ".pushguard", "pushguard.sock")
}

// NewServer creates a new daemon HTTP server.
func NewServer(engine *core.Engine, cfg Config) *Server {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	return &Server{
		engine:     engine,
		socketPath: cfg.SocketPath,
		httpAddr:   cfg.HTTPAddr,
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

	// Prepare socket directory
	_ = os.MkdirAll(filepath.Dir(s.socketPath), 0755)
	_ = os.Remove(s.socketPath) // Remove stale socket

	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket %s: %w", s.socketPath, err)
	}
	s.listener = l

	// Set permissions on unix socket
	_ = os.Chmod(s.socketPath, 0600)

	go func() {
		_ = s.httpServer.Serve(s.listener)
	}()

	// Also listen on TCP if HTTPAddr is set
	if s.httpAddr != "" {
		go func() {
			_ = http.ListenAndServe(s.httpAddr, mux)
		}()
	}

	return nil
}

// Stop gracefully shuts down the server and cleans up socket file.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	if s.httpServer != nil {
		err = s.httpServer.Shutdown(ctx)
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req core.EnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request json: "+err.Error())
			return
		}
		job, pos, err := s.engine.Enqueue(req)
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
		jobs := s.engine.ListJobs(limit)
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
		// GET /api/v1/jobs/{id}
		job, err := s.engine.GetJob(jobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"job": job,
		})
		return
	}

	subAction := parts[4]
	switch subAction {
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "use POST to cancel")
			return
		}
		if err := s.engine.Cancel(jobID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"message": fmt.Sprintf("job %s cancelled", jobID),
		})

	case "logs":
		job, err := s.engine.GetJob(jobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		lines := 100
		if q := r.URL.Query().Get("lines"); q != "" {
			if l, err := strconv.Atoi(q); err == nil && l > 0 {
				lines = l
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":   job.ID,
			"logs": job.TailLogs(lines),
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
	report := s.engine.Clean()
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
