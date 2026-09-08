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
	"strconv"
	"strings"
	"syscall"
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

func TestLocalRunner_LargeOutput(t *testing.T) {
	t.Parallel()
	r, err := NewLocalRunner(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := &core.Job{ID: "pg_large", DaemonBuild: "0.2.0+test", Command: helperCommand("large")}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	code, err := r.Run(ctx, job, t.TempDir())
	if err != nil || code != 0 {
		t.Fatalf("run: code %d, err %v", code, err)
	}
	data, err := os.ReadFile(job.LogFilePath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("x", 2*1024*1024) + "\nmarker\r\npartial"
	if !bytes.HasSuffix(data, []byte(want)) {
		t.Fatalf("disk output lost bytes: size %d", len(data))
	}
	if !bytes.HasPrefix(data, []byte("[pushguard] daemon_build=0.2.0+test")) {
		t.Fatalf("job log omitted daemon build: %q", data[:min(len(data), 80)])
	}
	for _, chunk := range strings.Split(job.TailLogs(2000), "\n") {
		if len(chunk) > maxLogChunkBytes {
			t.Fatalf("memory chunk has %d bytes", len(chunk))
		}
	}
	if !strings.HasSuffix(job.TailLogs(1), "partial") {
		t.Fatal("partial final line missing")
	}
}

func TestLocalRunner_ProcessGroupCancellation(t *testing.T) {
	t.Parallel()
	r, err := NewLocalRunner(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	job := &core.Job{ID: "pg_cancel_tree", Command: helperCommand("parent")}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.Run(ctx, job, workDir); done <- err }()
	childPID := waitForChildPID(t, filepath.Join(workDir, "child.pid"))
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not return after cancellation")
	}
	assertProcessExited(t, childPID)
}

func TestLocalRunner_BoundsInheritedPipes(t *testing.T) {
	t.Parallel()
	r, err := NewLocalRunner(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	job := &core.Job{ID: "pg_background", Command: helperCommand("background")}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	code, err := r.Run(ctx, job, workDir)
	if !errors.Is(err, exec.ErrWaitDelay) || code == 0 {
		t.Fatalf("expected bounded pipe failure, code %d, err %v", code, err)
	}
	if time.Since(started) >= 5*time.Second {
		t.Fatal("inherited pipe exceeded wait bound")
	}
	assertProcessExited(t, waitForChildPID(t, filepath.Join(workDir, "child.pid")))
}

func TestLogCapture_WriteErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		writer io.Writer
		want   error
	}{
		{"disk error", failingLogWriter{}, io.ErrClosedPipe},
		{"short write", shortLogWriter{}, io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &logCapture{disk: tc.writer, job: &core.Job{}}
			_, err := capture.Write([]byte("hello\n"))
			if !errors.Is(err, tc.want) || !errors.Is(capture.err, tc.want) {
				t.Fatalf("write err=%v, retained err=%v", err, capture.err)
			}
		})
	}
}

type failingLogWriter struct{}

func (failingLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type shortLogWriter struct{}

func (shortLogWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func helperCommand(mode string) []string {
	return []string{os.Args[0], "-test.run=^TestRunnerHelperProcess$", "--", mode}
}

func TestRunnerHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	switch os.Args[separator+1] {
	case "large":
		fmt.Print(strings.Repeat("x", 2*1024*1024) + "\nmarker\r\npartial")
	case "sleep":
		time.Sleep(30 * time.Second)
	case "parent", "background":
		args := helperCommand("sleep")
		child := exec.Command(args[0], args[1:]...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(10)
		}
		if err := os.WriteFile("child.pid", []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(11)
		}
		if os.Args[separator+1] == "parent" {
			_ = child.Wait()
		}
	default:
		os.Exit(12)
	}
	os.Exit(0)
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not report child PID")
	return 0
}

func assertProcessExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d survived job completion", pid)
}
