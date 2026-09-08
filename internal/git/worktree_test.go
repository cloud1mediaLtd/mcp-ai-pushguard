package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	repoDir, err := os.MkdirTemp("", "pushguard-test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	runCmd(t, repoDir, "git", "init")
	runCmd(t, repoDir, "git", "config", "user.name", "PushGuard Test")
	runCmd(t, repoDir, "git", "config", "user.email", "test@pushguard.local")

	// Create initial commit
	dummyFile := filepath.Join(repoDir, "file.txt")
	if err := os.WriteFile(dummyFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	runCmd(t, repoDir, "git", "add", "file.txt")
	runCmd(t, repoDir, "git", "commit", "-m", "initial commit")

	return repoDir
}

func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %s %v: %v\nOutput: %s", name, args, err, string(out))
	}
}

func TestWorktreeLifecycle(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	defer os.RemoveAll(repoDir)

	worktreeBase, err := os.MkdirTemp("", "pushguard-test-worktrees-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(worktreeBase)

	wm, err := NewWorktreeManager(worktreeBase)
	if err != nil {
		t.Fatalf("failed to create worktree manager: %v", err)
	}

	headSHA, err := GetHeadSHA(repoDir)
	if err != nil {
		t.Fatalf("failed to get head sha: %v", err)
	}

	jobID := "pg_test_job_1"
	worktreeDir, err := wm.CreateWorktree(repoDir, jobID, headSHA)
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Verify worktree file exists
	checkedOutFile := filepath.Join(worktreeDir, "file.txt")
	data, err := os.ReadFile(checkedOutFile)
	if err != nil || string(data) != "test content" {
		t.Fatalf("expected checked out file with 'test content', got %s, err: %v", string(data), err)
	}

	// Remove worktree
	err = wm.RemoveWorktree(repoDir, worktreeDir)
	if err != nil {
		t.Fatalf("RemoveWorktree failed: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("expected worktreeDir to be removed")
	}
}

func TestWorktreePruneOrphaned(t *testing.T) {
	worktreeBase, err := os.MkdirTemp("", "pushguard-test-orphan-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(worktreeBase)

	wm, _ := NewWorktreeManager(worktreeBase)

	repo := setupTestGitRepo(t)
	defer os.RemoveAll(repo)
	active, err := wm.CreateWorktree(repo, "pg_active_1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	registered, err := wm.CreateWorktree(repo, "pg_registered", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Unknown directories must survive cleanup.
	// Create fake orphaned folder
	orphanDir := filepath.Join(worktreeBase, "pg_orphan_123")
	_ = os.Mkdir(orphanDir, 0755)

	pruned, err := wm.PruneOrphaned(map[string]bool{"pg_active_1": true}, 0)
	if err != nil {
		t.Fatalf("PruneOrphaned error: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned folder, got %d", pruned)
	}

	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active worktree was removed: %v", err)
	}
	if _, err := os.Stat(registered); !os.IsNotExist(err) {
		t.Error("registered stale worktree was not deleted")
	}
	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("unknown orphanDir must be preserved: %v", err)
	}
}

func TestDirtySnapshot(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	defer os.RemoveAll(repoDir)

	// Clean state
	dirty, err := HasUncommittedChanges(repoDir)
	if err != nil || dirty {
		t.Fatalf("expected clean repo, got dirty: %v", dirty)
	}

	// Make dirty
	dummyFile := filepath.Join(repoDir, "file.txt")
	_ = os.WriteFile(dummyFile, []byte("dirty content"), 0644)

	dirty, _ = HasUncommittedChanges(repoDir)
	if !dirty {
		t.Fatalf("expected dirty repo")
	}

	stashSHA, err := CreateDirtySnapshotCommit(repoDir)
	if err != nil || len(stashSHA) == 0 {
		t.Fatalf("failed to create dirty snapshot commit: %v (sha=%s)", err, stashSHA)
	}

	// Verify working tree is still dirty (untouched by stash create)
	dirtyAfter, _ := HasUncommittedChanges(repoDir)
	if !dirtyAfter {
		t.Fatalf("working tree should remain dirty after stash create")
	}
	time.Sleep(1 * time.Millisecond)
}
