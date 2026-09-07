package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "-qm", "initial"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func filesInCommit(t *testing.T, dir, sha string) string {
	t.Helper()
	out, err := runGit(dir, "ls-tree", "-r", "--name-only", sha)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	return out
}

// The bug this replaced: `git stash create` omits untracked files, so a change
// whose new code lives in a new file was gated without it — and reported a
// compile error naming a symbol that is defined perfectly well on disk.
func TestSnapshotIncludesUntrackedFiles(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "brand_new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha, err := CreateDirtySnapshotCommit(dir)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if sha == "" {
		t.Fatal("a tree with an untracked file is dirty and must produce a snapshot")
	}
	if files := filesInCommit(t, dir, sha); !strings.Contains(files, "brand_new.go") {
		t.Fatalf("the snapshot omitted the untracked file; the gate would compile without it:\n%s", files)
	}
}

func TestSnapshotIncludesModificationsAndDeletions(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := CreateDirtySnapshotCommit(dir)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	content, err := runGit(dir, "show", sha+":tracked.txt")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(content, "changed") {
		t.Fatalf("the snapshot carried the committed content, not the working tree: %q", content)
	}

	if err := os.Remove(filepath.Join(dir, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	sha, err = CreateDirtySnapshotCommit(dir)
	if err != nil {
		t.Fatalf("snapshot after delete: %v", err)
	}
	if files := filesInCommit(t, dir, sha); strings.Contains(files, "tracked.txt") {
		t.Fatalf("a deleted file survived into the snapshot:\n%s", files)
	}
}

// .gitignore must still govern, or build output and untracked secrets would be
// swept into a snapshot that a normal commit would never contain.
func TestSnapshotRespectsGitignore(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.env"), []byte("TOKEN=hunter2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := CreateDirtySnapshotCommit(dir)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if files := filesInCommit(t, dir, sha); strings.Contains(files, "secret.env") {
		t.Fatalf("an ignored file was swept into the snapshot:\n%s", files)
	}
}

// The reason for the scratch index. Another session may have work staged in the
// same checkout, and snapshotting must not touch it.
func TestSnapshotLeavesTheCallersIndexAlone(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "staged_by_someone_else.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "staged_by_someone_else.txt"); err != nil {
		t.Fatal(err)
	}
	before, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := CreateDirtySnapshotCommit(dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	after, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("snapshotting changed the caller's git state.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestSnapshotOfACleanTreeReturnsNothing(t *testing.T) {
	dir := initRepo(t)
	sha, err := CreateDirtySnapshotCommit(dir)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if sha != "" {
		t.Fatalf("a clean tree needs no snapshot, got %q", sha)
	}
}
