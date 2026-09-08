package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorktreeManager handles safe lifecycle management of temporary git worktrees.
type WorktreeManager struct {
	BaseDir string
}

// NewWorktreeManager creates a manager storing worktrees in baseDir.
// If baseDir is empty, defaults to ~/.cache/pushguard/worktrees.
func NewWorktreeManager(baseDir string) (*WorktreeManager, error) {
	if baseDir == "" && os.Getenv("PUSHGUARD_STATE_DIR") != "" {
		baseDir = filepath.Join(os.Getenv("PUSHGUARD_STATE_DIR"), "worktrees")
	}
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			baseDir = filepath.Join(os.TempDir(), "pushguard-worktrees")
		} else {
			baseDir = filepath.Join(home, ".cache", "pushguard", "worktrees")
		}
	}
	absoluteBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree base directory: %w", err)
	}
	baseDir = absoluteBase
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktree base directory %q: %w", baseDir, err)
	}
	return &WorktreeManager{BaseDir: baseDir}, nil
}

// CreateWorktree creates a detached worktree for the specified ref in a unique directory.
func (m *WorktreeManager) CreateWorktree(repoPath, jobID, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return m.CreateWorktreeContext(ctx, repoPath, jobID, ref)
}

// CreateWorktreeContext creates a new checkout within the manager's owned directory.
func (m *WorktreeManager) CreateWorktreeContext(ctx context.Context, repoPath, jobID, ref string) (string, error) {
	if jobID == "" || filepath.Base(jobID) != jobID || jobID == "." || jobID == ".." {
		return "", fmt.Errorf("invalid job id")
	}
	targetDir := filepath.Join(m.BaseDir, jobID)
	if _, err := os.Lstat(targetDir); err == nil {
		return "", fmt.Errorf("worktree path already exists: %s", targetDir)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := runGitContext(ctx, repoPath, nil, "worktree", "add", "--detach", targetDir, ref); err != nil {
		// Git normally rolls back interrupted checkout itself. If registration
		// survived, remove only that registered worktree, never an arbitrary path.
		cleanupErr := m.RemoveWorktree(repoPath, targetDir)
		return "", errors.Join(fmt.Errorf("failed to add git worktree: %w", err), cleanupErr)
	}
	return targetDir, nil
}

// RemoveWorktree removes only a registered direct child of the manager's base.
func (m *WorktreeManager) RemoveWorktree(repoPath, targetDir string) error {
	base, err := filepath.Abs(m.BaseDir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(targetDir)
	if err != nil {
		return err
	}
	if filepath.Dir(target) != base {
		return fmt.Errorf("refusing worktree outside owned directory: %s", target)
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink worktree: %s", target)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := runGit(repoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return fmt.Errorf("inspect registered worktrees: %w", err)
	}
	canonicalTarget := target
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		canonicalTarget = resolved
	}
	registered := false
	for _, line := range strings.Split(out, "\x00") {
		if strings.TrimPrefix(line, "worktree ") == canonicalTarget && strings.HasPrefix(line, "worktree ") {
			registered = true
			break
		}
	}
	if !registered {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("refusing unregistered worktree: %s", target)
	}
	if _, err := runGit(repoPath, "worktree", "remove", "--force", target); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return nil
}

// PruneOrphaned removes stale registered worktrees, preserving active and unknown directories.
func (m *WorktreeManager) PruneOrphaned(activeJobIDs map[string]bool, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(m.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	pruned := 0
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobID := entry.Name()
		if activeJobIDs != nil && activeJobIDs[jobID] {
			continue // Currently active
		}

		info, err := entry.Info()
		if err == nil && maxAge > 0 && now.Sub(info.ModTime()) < maxAge {
			continue // Newer than maxAge
		}

		dirPath := filepath.Join(m.BaseDir, jobID)
		common, err := runGit(dirPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		} // Unknown directories are not ours to remove.
		if err := m.RemoveWorktree(strings.TrimSpace(common), dirPath); err != nil {
			return pruned, err
		}
		pruned++
	}

	return pruned, nil
}
