package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pushguard/pushguard/internal/core"
)

// Runner executes test commands for a job inside an isolated worktree.
type Runner interface {
	Run(ctx context.Context, job *core.Job, workDir string) (int, error)
	PruneOldLogs(maxAge time.Duration) (int, error)
}

// LocalRunner is the standard process executor on the local host.
type LocalRunner struct {
	LogBaseDir string
}

// NewLocalRunner creates a new process runner, placing logs in logBaseDir.
func NewLocalRunner(logBaseDir string) (*LocalRunner, error) {
	if logBaseDir == "" && os.Getenv("PUSHGUARD_STATE_DIR") != "" {
		logBaseDir = filepath.Join(os.Getenv("PUSHGUARD_STATE_DIR"), "logs")
	}
	if logBaseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logBaseDir = filepath.Join(os.TempDir(), "pushguard-logs")
		} else {
			logBaseDir = filepath.Join(home, ".cache", "pushguard", "logs")
		}
	}
	if err := os.MkdirAll(logBaseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory %q: %w", logBaseDir, err)
	}
	return &LocalRunner{LogBaseDir: logBaseDir}, nil
}

// Run executes the job's command in the workDir.
func (r *LocalRunner) Run(ctx context.Context, job *core.Job, workDir string) (exitCode int, runErr error) {
	if len(job.Command) == 0 {
		return 1, fmt.Errorf("empty test command")
	}

	logFilePath := filepath.Join(r.LogBaseDir, fmt.Sprintf("%s.log", job.ID))
	job.SetLogFilePath(logFilePath)

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return 1, fmt.Errorf("failed to open log file %q: %w", logFilePath, err)
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			exitCode = 1
			runErr = errors.Join(runErr, fmt.Errorf("close job log: %w", err))
		}
	}()

	var cmd *exec.Cmd
	if len(job.Command) == 1 {
		// Single string shell invocation
		cmd = exec.CommandContext(ctx, "sh", "-c", job.Command[0])
	} else {
		cmd = exec.CommandContext(ctx, job.Command[0], job.Command[1:]...)
	}

	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"CI=true",
		"PUSHGUARD=1",
		fmt.Sprintf("PUSHGUARD_JOB_ID=%s", job.ID),
		// Where the repository actually lives. The worktree is an isolated copy
		// under the cache directory, so anything resolved RELATIVE to the
		// checkout — a sibling repository, most obviously — silently finds
		// nothing here.
		//
		// c1m-server's consumers-check is the case that exposed it: it verifies
		// three sibling repositories are re-vendored against the API contract by
		// looking for ../platform_fe and friends, and inside a worktree it found
		// none of them and reported "PASS — 0 consumer(s) current, 3 skipped".
		// A release whose contract had changed would have been told the
		// consumers were in sync by a check that looked at nothing.
		//
		// Exporting the origin lets a check resolve siblings correctly rather
		// than be given a flag that tells it to stop caring.
		fmt.Sprintf("PUSHGUARD_ORIGIN_REPO=%s", job.RepoPath),
	)

	// A command may start make, shells, compilers, and smoke servers. Give
	// them a group so cancelling the job stops its descendants as well.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
	// Bound inherited output pipes even if a child escapes the process group.
	cmd.WaitDelay = 2 * time.Second

	capture := &logCapture{disk: logFile, job: job}
	cmd.Stdout = capture
	cmd.Stderr = capture
	defer capture.flush()

	startMsg := fmt.Sprintf("[pushguard] daemon_build=%s job=%s starting '%s' in %s\n", job.DaemonBuild, job.ID, strings.Join(job.Command, " "), workDir)
	if _, err := capture.Write([]byte(startMsg)); err != nil {
		return 1, fmt.Errorf("write job log: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("failed to start command: %w", err)
	}

	waitErr := cmd.Wait()
	// Also stop background children left behind by a command that has exited.
	cleanupErr := killProcessGroup(cmd.Process.Pid)
	if errors.Is(cleanupErr, os.ErrProcessDone) {
		cleanupErr = nil
	}
	if ctx.Err() != nil {
		return 130, errors.Join(ctx.Err(), capture.err, cleanupErr)
	}
	if capture.err != nil || cleanupErr != nil {
		return 1, errors.Join(capture.err, cleanupErr)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("wait for command: %w", waitErr)
	}
	return 0, nil
}

func killProcessGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("stop command process group: %w", err)
	}
	return nil
}

const maxLogChunkBytes = 64 * 1024

// logCapture preserves raw output on disk while retaining bounded lines or
// chunks in memory. os/exec serializes writes when stdout and stderr share
// the same writer; flush is called only after Wait has joined its copier.
type logCapture struct {
	disk    io.Writer
	job     *core.Job
	pending []byte
	err     error
}

func (c *logCapture) Write(p []byte) (int, error) {
	n, err := c.disk.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		c.err = fmt.Errorf("capture job output: %w", err)
	}
	remaining := p[:n]
	for len(remaining) > 0 {
		size := min(len(remaining), maxLogChunkBytes-len(c.pending))
		newline := bytes.IndexByte(remaining[:size], '\n')
		if newline >= 0 {
			c.pending = append(c.pending, remaining[:newline]...)
			c.job.AppendLog(string(c.pending))
			c.pending = c.pending[:0]
			remaining = remaining[newline+1:]
			continue
		}
		c.pending = append(c.pending, remaining[:size]...)
		remaining = remaining[size:]
		if len(c.pending) == maxLogChunkBytes {
			c.flush()
		}
	}
	return n, err
}

func (c *logCapture) flush() {
	if len(c.pending) > 0 {
		c.job.AppendLog(string(c.pending))
		c.pending = c.pending[:0]
	}
}

// PruneOldLogs deletes log files older than maxAge. If maxAge <= 0, defaults to 48 hours.
func (r *LocalRunner) PruneOldLogs(maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		maxAge = 48 * time.Hour
	}

	entries, err := os.ReadDir(r.LogBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	now := time.Now()
	pruned := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(r.LogBaseDir, entry.Name()))
			pruned++
		}
	}

	return pruned, nil
}
