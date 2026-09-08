package hook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pushguard/pushguard/internal/git"
)

const managedSyncHeader = "#!/usr/bin/env bash\n# PushGuard Pre-Push Hook (Synchronous Wait Mode)\n"

// InstallPrePushHook installs a synchronous hook that gates each pushed commit.
// It preserves unrelated hooks and rejects the retired automatic-push mode.
func InstallPrePushHook(repoPath string, async bool) (string, error) {
	if async {
		return "", fmt.Errorf("asynchronous auto-push hooks are no longer supported; install a synchronous hook")
	}
	root, err := git.GetRepoRoot(repoPath)
	if err != nil {
		return "", fmt.Errorf("failed to locate git repository: %w", err)
	}
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git hooks directory: %w", err)
	}
	hooksDir := strings.TrimSpace(string(output))
	hookFile := filepath.Join(hooksDir, "pre-push")
	if info, err := os.Lstat(hookFile); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("refusing to replace non-regular hook %q", hookFile)
		}
		content, err := os.ReadFile(hookFile)
		if err != nil {
			return "", fmt.Errorf("read existing hook: %w", err)
		}
		if !strings.HasPrefix(string(content), managedSyncHeader) {
			return "", fmt.Errorf("existing hook %q is not a managed synchronous PushGuard hook; preserve or integrate it before installing", hookFile)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect existing hook: %w", err)
	}
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create hooks directory: %w", err)
	}
	if err := os.WriteFile(hookFile, []byte(syncScript), 0755); err != nil {
		return "", fmt.Errorf("failed to write hook file: %w", err)
	}
	if err := os.Chmod(hookFile, 0755); err != nil {
		return "", fmt.Errorf("make hook executable: %w", err)
	}
	return hookFile, nil
}

const syncScript = managedSyncHeader + `# Git supplies the actual updates on stdin; HEAD may be unrelated to them.

repo=$(git rev-parse --show-toplevel) || exit 1
verified=" "
while read -r local_ref local_oid remote_ref remote_oid extra; do
    if [ -z "$local_ref" ] || [ -z "$local_oid" ] || [ -z "$remote_ref" ] || [ -z "$remote_oid" ] || [ -n "$extra" ]; then
        echo "[pushguard] Invalid pre-push update record." >&2
        exit 1
    fi
    # An all-zero local object ID means a deletion, which has no commit to gate.
    if [ -z "${local_oid//0/}" ]; then
        continue
    fi
    commit=$(git rev-parse --verify --end-of-options "${local_oid}^{commit}") || exit 1
    case "$verified" in
        *" $commit "*) continue ;;
    esac
    echo "[pushguard] Checking pushed commit $commit..." >&2
    # The child must not consume the remaining Git update records.
    pushguard enqueue --repo "$repo" --ref "$commit" --wait </dev/null
    verdict=$?
    if [ "$verdict" -ne 0 ]; then
        exit "$verdict"
    fi
    verified="$verified$commit "
done
exit 0
`
