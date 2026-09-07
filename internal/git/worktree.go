package git

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WorktreeManager handles safe lifecycle management of temporary git worktrees.
type WorktreeManager struct {
	BaseDir string
}

// NewWorktreeManager creates a manager storing worktrees in baseDir.
// If baseDir is empty, defaults to ~/.cache/pushguard/worktrees.
func NewWorktreeManager(baseDir string) (*WorktreeManager, error) {
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			baseDir = filepath.Join(os.TempDir(), "pushguard-worktrees")
		} else {
			baseDir = filepath.Join(home, ".cache", "pushguard", "worktrees")
		}
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktree base directory %q: %w", baseDir, err)
	}
	return &WorktreeManager{BaseDir: baseDir}, nil
}

// CreateWorktree creates a detached worktree for the specified ref in a unique directory.
func (m *WorktreeManager) CreateWorktree(repoPath, jobID, ref string) (string, error) {
	targetDir := filepath.Join(m.BaseDir, jobID)

	// Clean up any stale directory at target
	if _, err := os.Stat(targetDir); err == nil {
		_ = m.RemoveWorktree(repoPath, targetDir)
	}

	// git worktree add --detach <targetDir> <ref>
	_, err := runGit(repoPath, "worktree", "add", "--detach", targetDir, ref)
	if err != nil {
		return "", fmt.Errorf("failed to add git worktree: %w", err)
	}

	return targetDir, nil
}

// RemoveWorktree safely unlinks and cleans up a worktree.
func (m *WorktreeManager) RemoveWorktree(repoPath, targetDir string) error {
	// Try git worktree remove --force
	_, _ = runGit(repoPath, "worktree", "remove", "--force", targetDir)
	_, _ = runGit(repoPath, "worktree", "prune")

	// Ensure filesystem cleanup
	_ = os.RemoveAll(targetDir)
	return nil
}

// PruneOrphaned removes any worktrees in BaseDir not present in activeJobIDs or older than maxAge.
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
		_ = os.RemoveAll(dirPath)
		pruned++
	}

	return pruned, nil
}
