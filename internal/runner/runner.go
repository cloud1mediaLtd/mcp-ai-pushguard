package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
func (r *LocalRunner) Run(ctx context.Context, job *core.Job, workDir string) (int, error) {
	if len(job.Command) == 0 {
		return 1, fmt.Errorf("empty test command")
	}

	logFilePath := filepath.Join(r.LogBaseDir, fmt.Sprintf("%s.log", job.ID))
	job.LogFilePath = logFilePath

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return 1, fmt.Errorf("failed to open log file %q: %w", logFilePath, err)
	}
	defer logFile.Close()

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
	)

	// Combine stdout & stderr
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	// Stream reader
	doneReader := make(chan struct{})
	go func() {
		defer close(doneReader)
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			text := scanner.Text()
			job.AppendLog(text)
			_, _ = fmt.Fprintln(logFile, text)
		}
	}()

	startMsg := fmt.Sprintf("[pushguard] Starting '%s' in %s", strings.Join(job.Command, " "), workDir)
	job.AppendLog(startMsg)
	_, _ = fmt.Fprintln(logFile, startMsg)

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		<-doneReader
		return 1, fmt.Errorf("failed to start command: %w", err)
	}

	waitErr := cmd.Wait()
	_ = pw.Close()
	<-doneReader

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		if ctx.Err() != nil {
			return 130, ctx.Err()
		}
		return 1, waitErr
	}

	return 0, nil
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
