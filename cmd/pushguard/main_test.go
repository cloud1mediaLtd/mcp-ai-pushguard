package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// main() calls os.Exit() throughout, which makes it untestable in-process —
// these are black-box tests that exec the real compiled binary and assert on
// its stdout/stderr/exit code, the standard pattern for testing a Go CLI
// entrypoint. The binary is built once in TestMain into a directory that
// outlives any single test (unlike t.TempDir(), which is cleaned up the
// moment the test that created it finishes — the first test to need the
// binary would otherwise delete it out from under every test after it).
var builtBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pushguard-cli-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to create build dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	builtBinary = filepath.Join(dir, "pushguard-test-bin")
	cmd := exec.Command("go", "build", "-o", builtBinary, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build pushguard binary: %v\n%s\n", err, out)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func binaryPath(t *testing.T) string {
	t.Helper()
	return builtBinary
}

// run execs the built binary with an isolated $HOME, so DefaultSocketPath()
// resolves under a directory with no daemon running — these tests must never
// depend on (or interfere with) whatever daemon happens to be running on the
// machine that runs `go test`.
func run(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := binaryPath(t)
	fakeHome := t.TempDir()

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+fakeHome, "PUSHGUARD_NO_AUTOSPAWN=1")

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()

	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("failed to run binary: %v", err)
	}
	return outBuf.String(), errBuf.String(), code
}

func TestCLI_Version(t *testing.T) {
	stdout, _, code := run(t, "version")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "pushguard version") {
		t.Fatalf("expected version output, got %q", stdout)
	}
}

func TestCLI_HelpAndNoArgs(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {}} {
		stdout, _, code := run(t, args...)
		if code != 0 {
			t.Fatalf("args %v: expected exit 0, got %d", args, code)
		}
		if !strings.Contains(stdout, "PushGuard") || !strings.Contains(stdout, "Usage:") {
			t.Fatalf("args %v: expected usage text, got %q", args, stdout)
		}
	}
}

func TestCLI_UnknownCommand(t *testing.T) {
	_, stderr, code := run(t, "definitely-not-a-real-command")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "Unknown command") {
		t.Fatalf("expected an unknown-command message, got %q", stderr)
	}
}

func TestCLI_StatusRequiresJobID(t *testing.T) {
	_, stderr, code := run(t, "status")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "Usage: pushguard status") {
		t.Fatalf("expected a usage message, got %q", stderr)
	}
}

func TestCLI_StatusWithoutDaemon(t *testing.T) {
	_, stderr, code := run(t, "status", "pg_doesnotexist")
	if code != 1 {
		t.Fatalf("expected exit 1 when no daemon is running, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "daemon is not running") {
		t.Fatalf("expected a daemon-not-running message, got %q", stderr)
	}
}

func TestCLI_ListWithoutDaemon(t *testing.T) {
	_, stderr, code := run(t, "list")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "daemon is not running") {
		t.Fatalf("expected a daemon-not-running message, got %q", stderr)
	}
}

func TestCLI_CancelWithoutDaemon(t *testing.T) {
	_, stderr, code := run(t, "cancel", "pg_doesnotexist")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "daemon is not running") {
		t.Fatalf("expected a daemon-not-running message, got %q", stderr)
	}
}

func TestCLI_CleanStandalone(t *testing.T) {
	// No daemon (isolated $HOME) -> falls back to the standalone in-process
	// clean path rather than erroring.
	stdout, _, code := run(t, "clean")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "cleaned successfully") {
		t.Fatalf("expected a cleanup summary, got %q", stdout)
	}
}

func TestCLI_HookInstallRequiresSubcommand(t *testing.T) {
	_, stderr, code := run(t, "hook")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "Usage: pushguard hook install") {
		t.Fatalf("expected a usage message, got %q", stderr)
	}
}
