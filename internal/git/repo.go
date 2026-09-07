package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
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

// CreateDirtySnapshotCommit creates a git tree object of any uncommitted/staged changes
// using `git stash create`. It returns a commit SHA representing the exact working state
// without modifying the working tree or affecting stash list. If tree is clean, returns "".
func CreateDirtySnapshotCommit(repoPath string) (string, error) {
	out, err := runGit(repoPath, "stash", "create")
	if err != nil {
		return "", fmt.Errorf("failed to create stash snapshot: %w", err)
	}
	sha := strings.TrimSpace(out)
	return sha, nil
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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s (git %s): %s", err, strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
