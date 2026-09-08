package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/pkg/api"
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

func TestCLI_StatusWithoutJobIDReportsDaemonState(t *testing.T) {
	_, stderr, code := run(t, "status")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "daemon is not running") {
		t.Fatalf("expected daemon diagnostic, got %q", stderr)
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

// runInHome is run() with a caller-supplied $HOME, so a test can seed
// ~/.pushguard/jobs before invoking the binary.
func runInHome(t *testing.T, home string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binaryPath(t), args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PUSHGUARD_NO_AUTOSPAWN=1")

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

// seedJobRecord writes one mirrored job record the way DurableStore.persist
// does. Hand-rolled JSON rather than marshalling core.Job, so the test pins the
// on-disk shape the CLI has to read rather than agreeing with the writer by
// construction.
func seedJobRecord(t *testing.T, home, id, status string, exitCode int) {
	seedJobRecordWithRef(t, home, id, status, exitCode, "d7ab0d01abcdef")
}

func seedJobRecordWithRef(t *testing.T, home, id, status string, exitCode int, ref string) {
	t.Helper()
	dir := filepath.Join(home, ".pushguard", "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir jobs dir: %v", err)
	}
	record := fmt.Sprintf(`{
		"id": %q,
		"name": "c1m-server",
		"status": %q,
		"repo_path": "/repo/c1m-server",
		"branch": "main",
		"ref": %q,
		"commit_sha": "635ee5063abcdef",
		"command": ["make", "pre-push"],
		"exit_code": %d,
		"created_at": "2026-09-07T10:00:00Z",
		"started_at": "2026-09-07T10:00:01Z",
		"finished_at": "2026-09-07T10:11:00Z"
	}`, id, status, ref, exitCode)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(record), 0o644); err != nil {
		t.Fatalf("write job record: %v", err)
	}
}

// The regression this whole path exists for: the daemon idles out after thirty
// minutes, which is normal, and a caller who enqueued and walked away came back
// to "daemon is not running" with their result sitting on disk unread.
func TestCLI_StatusReadsStoredRecordWithoutDaemon(t *testing.T) {
	home := t.TempDir()
	seedJobRecord(t, home, "pg_stored01", "passed", 0)

	stdout, stderr, code := runInHome(t, home, "status", "pg_stored01")
	if code != 0 {
		t.Fatalf("expected exit 0 for a stored passing job, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "pg_stored01") || !strings.Contains(stdout, "PASSED") {
		t.Fatalf("expected the stored record in stdout, got %q", stdout)
	}
	// The banner belongs on stderr so it never corrupts parsed stdout.
	if !strings.Contains(stderr, "reading stored records from disk") {
		t.Fatalf("expected an offline banner on stderr, got %q", stderr)
	}
	if strings.Contains(stdout, "reading stored records from disk") {
		t.Fatalf("banner leaked into stdout: %q", stdout)
	}
}

// `status` reports; `await` gates. The daemon-backed status exits 0 whatever
// the job did, and the offline path must match it exactly: the same command
// against the same job must not change its exit code depending on whether a
// daemon happened to still be up. A caller that needs the verdict in $? uses
// `await`, which does propagate it.
func TestCLI_OfflineStatusExitCodeMatchesOnlineContract(t *testing.T) {
	for _, tc := range []struct {
		id       string
		status   string
		exitCode int
	}{
		{"pg_stored02", "failed", 2},
		{"pg_stored03", "cancelled", 0},
	} {
		home := t.TempDir()
		seedJobRecord(t, home, tc.id, tc.status, tc.exitCode)

		stdout, stderr, code := runInHome(t, home, "status", tc.id)
		if code != 0 {
			t.Fatalf("%s: status must exit 0 like the daemon-backed path, got %d", tc.status, code)
		}
		// It must still be unmistakable that the job did not pass.
		if !strings.Contains(stdout, strings.ToUpper(tc.status)) {
			t.Fatalf("%s: expected the status in the output, got %q (stderr=%q)", tc.status, stdout, stderr)
		}
	}
}

func TestCLI_ListReadsStoredRecordsWithoutDaemon(t *testing.T) {
	home := t.TempDir()
	seedJobRecord(t, home, "pg_stored04", "passed", 0)

	stdout, stderr, code := runInHome(t, home, "list")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "pg_stored04") {
		t.Fatalf("expected the stored job in the table, got %q", stdout)
	}
	// The REF column must show the gated commit, not the branch tip.
	if !strings.Contains(stdout, "d7ab0d01a") {
		t.Fatalf("expected the job's Ref in the REF column, got %q", stdout)
	}
	if strings.Contains(stdout, "635ee5063") {
		t.Fatalf("REF column showed CommitSHA (the branch tip) instead of Ref: %q", stdout)
	}
}

func TestCLI_OfflineListHonorsRefAndLimit(t *testing.T) {
	home := t.TempDir()
	seedJobRecordWithRef(t, home, "pg_listwanted", "passed", 0, "wanted-ref")
	seedJobRecordWithRef(t, home, "pg_listother", "passed", 0, "other-ref")

	stdout, stderr, code := runInHome(t, home, "list", "--ref", "wanted-ref", "--limit", "1")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "pg_listwanted") || strings.Contains(stdout, "pg_listother") {
		t.Fatalf("offline ref filter was ignored: %q", stdout)
	}
}

// Warn about a record that outlived its daemon: it says RUNNING but nothing is.
func TestCLI_OfflineListWarnsThatInFlightRecordsAreNotRunning(t *testing.T) {
	home := t.TempDir()
	seedJobRecord(t, home, "pg_stored05", "running", 0)

	stdout, stderr, code := runInHome(t, home, "list")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "pg_stored05") {
		t.Fatalf("expected the stored job, got %q", stdout)
	}
	if !strings.Contains(stderr, "is not running now") {
		t.Fatalf("expected a warning that an in-flight record is stale, got %q", stderr)
	}
}

// The exit-code contract, pinned because the whole value of a gate runner is
// that $? tells the truth about whether the checks passed.
//
// The dangerous case is a cancelled job: it records ExitCode 0 because no
// process ever returned one, and passing that through reported an unrun gate
// as a clean one. A peer nearly reported a gate as green having cancelled it
// themselves seconds earlier.
func TestExitCodeFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   core.JobStatus
		exitCode int
		want     int
	}{
		{"a passing gate is the only zero", core.StatusPassed, 0, 0},
		{"a failed gate is 1", core.StatusFailed, 1, exitJobFailed},
		{"a failed gate does not forward the runner's code", core.StatusFailed, 2, exitJobFailed},
		{"a failed gate recording 0 is still a failure", core.StatusFailed, 0, exitJobFailed},
		{"a cancelled gate never ran, so it is not a pass", core.StatusCancelled, 0, exitJobNoVerdict},
		{"nor a failure", core.StatusCancelled, 1, exitJobNoVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := exitCodeFor(api.Job{Status: api.JobStatus(tc.status), ExitCode: tc.exitCode})
			if got != tc.want {
				t.Fatalf("exitCodeFor(%s, exit %d) = %d, want %d", tc.status, tc.exitCode, got, tc.want)
			}
		})
	}
}

// The three non-zero codes must stay distinct, or "never ran" collapses back
// into "failed" or "timed out" and the distinction above buys nothing.
func TestExitCodesAreDistinct(t *testing.T) {
	seen := map[int]string{0: "passed"}
	for code, name := range map[int]string{
		exitJobFailed:    "failed",
		exitAwaitTimeout: "await timeout",
		exitJobNoVerdict: "no verdict",
	} {
		if other, clash := seen[code]; clash {
			t.Fatalf("%s and %s both exit %d", name, other, code)
		}
		seen[code] = name
	}
}

// The verdict must be the LAST thing await prints.
//
// An agent's harness reports the exit status of the whole shell command, so
// `await ... ; echo done` or `await ... | tail -20` reports 0 for a red gate.
// The reader then falls back on the output — and the output ended in screens of
// test-failure noise with the single FAILED line scrolled off the top. Two
// sessions each misread a red gate as green in one evening on exactly that.
func TestAwaitPrintsTheVerdictLast(t *testing.T) {
	home := t.TempDir()
	seedJobRecord(t, home, "pg_verdict01", "failed", 2)

	// No daemon: await answers from the mirrored record, which is the path a
	// caller hits after the daemon idles out.
	stdout, _, _ := runInHome(t, home, "await", "pg_verdict01", "--timeout", "1")

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "GATE FAILED") {
		t.Fatalf("the final line must carry the verdict, got %q\nfull output:\n%s", last, stdout)
	}
	if !strings.Contains(last, "pg_verdict01") {
		t.Fatalf("the verdict line must name the job, got %q", last)
	}
}

func TestAwaitVerdictLineNamesAPass(t *testing.T) {
	home := t.TempDir()
	seedJobRecord(t, home, "pg_verdict02", "passed", 0)

	stdout, _, code := runInHome(t, home, "await", "pg_verdict02", "--timeout", "1")
	if code != 0 {
		t.Fatalf("a passing gate must exit 0, got %d", code)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "GATE PASSED") {
		t.Fatalf("the final line must carry the verdict, got %q", last)
	}
}
