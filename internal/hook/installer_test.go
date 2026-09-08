package hook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPrePushHook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		hooksPath string
		worktree  bool
	}{
		{name: "default"},
		{name: "configured relative", hooksPath: "custom hooks"},
		{name: "configured absolute", hooksPath: "absolute"},
		{name: "linked worktree", worktree: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			expectedDir := filepath.Join(repo, ".git", "hooks")
			if tc.hooksPath != "" {
				configured := tc.hooksPath
				if configured == "absolute" {
					configured = filepath.Join(t.TempDir(), "hooks")
				}
				runGit(t, repo, "config", "core.hooksPath", configured)
				expectedDir = configured
				if !filepath.IsAbs(expectedDir) {
					expectedDir = filepath.Join(repo, expectedDir)
				}
			}
			if tc.worktree {
				worktree := filepath.Join(t.TempDir(), "checkout")
				runGit(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
				repo = worktree
			}
			hookPath, err := InstallPrePushHook(repo, false)
			if err != nil {
				t.Fatal(err)
			}
			expectedDir, err = filepath.EvalSymlinks(expectedDir)
			if err != nil {
				t.Fatal(err)
			}
			if hookPath != filepath.Join(expectedDir, "pre-push") {
				t.Fatalf("hook path %q, want directory %q", hookPath, expectedDir)
			}
			info, err := os.Stat(hookPath)
			if err != nil || info.Mode()&0111 == 0 {
				t.Fatalf("hook not executable: %v", err)
			}
			if _, err := InstallPrePushHook(repo, false); err != nil {
				t.Fatalf("reinstall managed sync hook: %v", err)
			}
		})
	}
}

func TestInstallPrePushHook_PreservesExistingArtifacts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, existing string
		async          bool
	}{
		{"unrelated", "#!/bin/sh\necho user hook\n", false},
		{"legacy async", "#!/usr/bin/env bash\n# PushGuard Pre-Push Hook (Async Mode)\n", false},
		{"async rejected", "#!/bin/sh\necho user hook\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			path := filepath.Join(repo, ".git", "hooks", "pre-push")
			writeTestFile(t, path, tc.existing, 0755)
			writeTestFile(t, path+".backup", "original backup", 0600)
			if _, err := InstallPrePushHook(repo, tc.async); err == nil {
				t.Fatal("expected refusal")
			}
			assertFile(t, path, tc.existing)
			assertFile(t, path+".backup", "original backup")
		})
	}
	t.Run("async before repository lookup", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist")
		if _, err := InstallPrePushHook(path, true); err == nil || !strings.Contains(err.Error(), "no longer supported") {
			t.Fatalf("expected early async rejection, got %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("async install mutated path")
		}
	})
}

func TestSyncHook_VerifiesPushedCommits(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	first := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "commit", "--allow-empty", "-m", "second")
	second := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "tag", "-a", "release", first, "-m", "release")
	tag := runGit(t, repo, "rev-parse", "release")
	hook, err := InstallPrePushHook(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	input := fmt.Sprintf("refs/heads/old %s refs/heads/old %s\nrefs/tags/release %s refs/tags/release %s\nrefs/heads/new %s refs/heads/new %s\n(delete) %s refs/heads/deleted %s\n", first, zero, tag, zero, second, zero, zero, first)
	calls, code := runHook(t, repo, hook, input, 0)
	if code != 0 {
		t.Fatalf("hook exit %d", code)
	}
	if calls != first+"\n"+second+"\n" {
		t.Fatalf("wrong commit gates: %q", calls)
	}
	calls, code = runHook(t, repo, hook, input, 3)
	if code != 3 || calls != first+"\n" {
		t.Fatalf("must propagate verdict and stop: code=%d calls=%q", code, calls)
	}
}

func TestSyncHook_DeletionAndInvalidObject(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	hook, err := InstallPrePushHook(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	calls, code := runHook(t, repo, hook, "(delete) "+zero+" refs/heads/old "+zero+"\n", 0)
	if calls != "" || code != 0 {
		t.Fatalf("deletion ran checks: code=%d calls=%q", code, calls)
	}
	calls, code = runHook(t, repo, hook, "refs/heads/bad invalid refs/heads/bad "+zero+"\n", 0)
	if calls != "" || code == 0 {
		t.Fatalf("invalid object accepted: code=%d calls=%q", code, calls)
	}
}

func testRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "pushguard-test@example.invalid")
	runGit(t, repo, "config", "user.name", "PushGuard Test")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	return repo
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func runHook(t *testing.T, repo, hook, input string, verdict int) (string, int) {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	writeTestFile(t, filepath.Join(bin, "pushguard"), `#!/bin/sh
[ "$1" = enqueue ] && [ "$2" = --repo ] && [ "$4" = --ref ] && [ "$6" = --wait ] || exit 19
printf '%s\n' "$5" >> "$HOOK_TEST_CALLS"
# A consuming CLI must not steal the next Git update record.
cat >/dev/null
exit "$HOOK_TEST_VERDICT"
`, 0755)
	cmd := exec.Command("bash", hook)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOOK_TEST_CALLS="+calls, fmt.Sprintf("HOOK_TEST_VERDICT=%d", verdict))
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run hook: %v\n%s", err, output)
		}
	}
	data, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data), code
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("file %s changed: %q err=%v", path, data, err)
	}
}
