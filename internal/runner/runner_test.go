package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
)

func TestLocalRunner_SuccessAndLogs(t *testing.T) {
	logDir, err := os.MkdirTemp("", "pushguard-runner-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(logDir)

	r, err := NewLocalRunner(logDir)
	if err != nil {
		t.Fatal(err)
	}

	job := &core.Job{
		ID:      "pg_run_test_1",
		Command: []string{"echo", "hello pushguard test"},
	}

	workDir, _ := os.MkdirTemp("", "pushguard-workdir-*")
	defer os.RemoveAll(workDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exitCode, err := r.Run(ctx, job, workDir)
	if err != nil || exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, err: %v", exitCode, err)
	}

	// Verify log in memory
	logs := job.TailLogs(10)
	if !strings.Contains(logs, "hello pushguard test") {
		t.Errorf("expected logs to contain 'hello pushguard test', got: %s", logs)
	}

	// Verify log file on disk
	logPath := filepath.Join(logDir, "pg_run_test_1.log")
	content, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(content), "hello pushguard test") {
		t.Errorf("expected log file to contain output, got: %s, err: %v", string(content), err)
	}
}

func TestLocalRunner_FailureExitCode(t *testing.T) {
	logDir, _ := os.MkdirTemp("", "pushguard-runner-logs-*")
	defer os.RemoveAll(logDir)

	r, _ := NewLocalRunner(logDir)
	job := &core.Job{
		ID:      "pg_run_fail_1",
		Command: []string{"sh", "-c", "exit 42"},
	}

	workDir, _ := os.MkdirTemp("", "pushguard-workdir-*")
	defer os.RemoveAll(workDir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exitCode, err := r.Run(ctx, job, workDir)
	if exitCode != 42 {
		t.Errorf("expected exit code 42, got %d (err: %v)", exitCode, err)
	}
}

func TestLocalRunner_PruneOldLogs(t *testing.T) {
	logDir, _ := os.MkdirTemp("", "pushguard-runner-logs-*")
	defer os.RemoveAll(logDir)

	r, _ := NewLocalRunner(logDir)

	oldLog := filepath.Join(logDir, "old.log")
	_ = os.WriteFile(oldLog, []byte("old logs"), 0644)

	// Set mod time to 3 days ago
	oldTime := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(oldLog, oldTime, oldTime)

	recentLog := filepath.Join(logDir, "recent.log")
	_ = os.WriteFile(recentLog, []byte("recent logs"), 0644)

	pruned, err := r.PruneOldLogs(48 * time.Hour)
	if err != nil {
		t.Fatalf("PruneOldLogs error: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned log, got %d", pruned)
	}

	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Errorf("expected oldLog to be deleted")
	}
	if _, err := os.Stat(recentLog); os.IsNotExist(err) {
		t.Errorf("expected recentLog to still exist")
	}
}
