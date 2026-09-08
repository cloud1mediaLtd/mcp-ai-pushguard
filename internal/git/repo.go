package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// GetRepoRoot finds the top-level directory of a git repository from dir.
func GetRepoRoot(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository or git error: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// GetHeadSHA gets the commit SHA of HEAD.
func GetHeadSHA(repoPath string) (string, error) {
	out, err := runGit(repoPath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD sha: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// ResolveCommit resolves a revision to one immutable commit, rejecting trees and blobs.
func ResolveCommit(repoPath, ref string) (string, error) {
	out, err := runGit(repoPath, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve commit %q: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// GetCurrentBranch gets the current branch name (or HEAD if detached).
func GetCurrentBranch(repoPath string) (string, error) {
	out, err := runGit(repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// HasUncommittedChanges returns true if git status reports unstaged or staged changes.
func HasUncommittedChanges(repoPath string) (bool, error) {
	out, err := runGit(repoPath, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status error: %w", err)
	}
	return len(strings.TrimSpace(out)) > 0, nil
}

// CreateDirtySnapshotCommit builds a commit representing the working tree —
// tracked modifications AND untracked files — without touching the working tree
// or the caller's index. Returns "" when there is nothing to snapshot.
//
// It used to be `git stash create`, which silently excludes untracked files.
// That disagreed with HasUncommittedChanges, which uses `git status
// --porcelain` and DOES count them, so a tree whose only new code sat in a new
// file was reported dirty and then gated without that file. The failure looks
// like a genuine compile error in someone else's code:
//
//	internal/routing/gateway/router.go:47:2: undefined: injectAuthRuntime
//
// while `injectAuthRuntime` is defined perfectly well in an untracked
// auth_runtime.go. A gate that invents failures is worse than no gate, because
// people stop trusting red — and the same gap can invent a PASS, by omitting an
// untracked caller of something the change deleted.
//
// The scratch index is what makes this safe: GIT_INDEX_FILE points `git add` at
// a temporary file, so the caller's staged content is untouched. That matters
// in a shared checkout where another session may have work staged.
func CreateDirtySnapshotCommit(repoPath string) (string, error) {
	base, err := ResolveCommit(repoPath, "HEAD")
	if err != nil {
		return "", err
	}
	return CreateSnapshotCommit(repoPath, base)
}

// CreateSnapshotCommit snapshots working files against an already resolved parent.
func CreateSnapshotCommit(repoPath, baseSHA string) (string, error) {
	indexFile, err := os.CreateTemp("", "pushguard-index-*")
	if err != nil {
		return "", fmt.Errorf("failed to create scratch index: %w", err)
	}
	indexPath := indexFile.Name()
	_ = indexFile.Close()
	// git wants to create the index itself; an existing empty file is not a
	// valid index.
	_ = os.Remove(indexPath)
	defer func() { _ = os.Remove(indexPath) }()

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)

	// Seed from HEAD so unchanged files carry their existing blobs.
	if _, err := runGitEnv(repoPath, env, "read-tree", baseSHA); err != nil {
		return "", fmt.Errorf("failed to seed scratch index: %w", err)
	}
	// -A stages modifications, deletions and untracked files, and still honours
	// .gitignore — so build output and secrets stay out exactly as they do for
	// a normal commit.
	if _, err := runGitEnv(repoPath, env, "add", "-A", "."); err != nil {
		return "", fmt.Errorf("failed to stage working tree into scratch index: %w", err)
	}
	tree, err := runGitEnv(repoPath, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("failed to write snapshot tree: %w", err)
	}
	baseTree, err := runGit(repoPath, "rev-parse", "--verify", "--end-of-options", baseSHA+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve snapshot base tree: %w", err)
	}
	if strings.TrimSpace(tree) == strings.TrimSpace(baseTree) {
		return "", nil
	}
	commit, err := runGitEnv(repoPath, env, "commit-tree", strings.TrimSpace(tree), "-p", baseSHA,
		"-m", "pushguard: working-tree snapshot")
	if err != nil {
		return "", fmt.Errorf("failed to commit snapshot tree: %w", err)
	}
	return strings.TrimSpace(commit), nil
}

// Push pushes the current or specified branch to remote.
func Push(repoPath, branch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "push", "origin", branch)
	cmd.Dir = repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func runGit(dir string, args ...string) (string, error) {
	return runGitEnv(dir, nil, args...)
}

// runGitEnv runs git with an explicit environment. A nil env inherits this
// process's, which is what every caller but the snapshot wants.
func runGitEnv(dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runGitContext(ctx, dir, env, args...)
}

func runGitContext(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s (git %s): %s", err, strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
