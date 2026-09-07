package hook

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pushguard/pushguard/internal/git"
)

// InstallPrePushHook installs a PushGuard-powered pre-push hook into repoPath.
func InstallPrePushHook(repoPath string, async bool) (string, error) {
	root, err := git.GetRepoRoot(repoPath)
	if err != nil {
		return "", fmt.Errorf("failed to locate git repository: %w", err)
	}

	hooksDir := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create hooks directory: %w", err)
	}

	hookFile := filepath.Join(hooksDir, "pre-push")

	// Backup existing hook if not already backed up
	if _, err := os.Stat(hookFile); err == nil {
		backup := hookFile + ".backup"
		_ = os.Rename(hookFile, backup)
	}

	var script string
	if async {
		script = `#!/usr/bin/env bash
# PushGuard Pre-Push Hook (Async Mode)
# Offloads checks to PushGuard queue and cancels direct push.
# When tests pass, PushGuard will push on your behalf!

echo "[pushguard] Enqueueing pre-push checks asynchronously..."
pushguard enqueue --repo "$(git rev-parse --show-toplevel)" --auto-push
echo "[pushguard] Direct push aborted; PushGuard will push automatically upon green tests."
exit 1
`
	} else {
		script = `#!/usr/bin/env bash
# PushGuard Pre-Push Hook (Synchronous Wait Mode)
# Runs tests in an isolated worktree so your working tree is preserved.

echo "[pushguard] Running pre-push checks via PushGuard worktree queue..."
pushguard enqueue --repo "$(git rev-parse --show-toplevel)" --wait
exit $?
`
	}

	if err := os.WriteFile(hookFile, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("failed to write hook file: %w", err)
	}

	return hookFile, nil
}
