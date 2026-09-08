package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DetectionDir materializes only the root metadata used by suite detection from
// an immutable commit. The caller owns the temporary directory and must remove
// it. Symlinks are excluded rather than following files outside the commit.
func DetectionDir(repoPath, ref string) (string, error) {
	manifests := []string{"Makefile", "go.mod", "package.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb", "bun.lock", "Cargo.toml", "pytest.ini", "pyproject.toml"}
	args := append([]string{"ls-tree", "-z", ref, "--"}, manifests...)
	listing, err := runGit(repoPath, args...)
	if err != nil {
		return "", fmt.Errorf("inspect verification metadata: %w", err)
	}
	dir, err := os.MkdirTemp("", "pushguard-detect-*")
	if err != nil {
		return "", fmt.Errorf("create verification metadata directory: %w", err)
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	for _, entry := range strings.Split(listing, "\x00") {
		header, name, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			continue
		}
		// These are fixed known paths, never arbitrary tree entries.
		if filepath.Base(name) != name {
			return "", fmt.Errorf("unexpected verification metadata path: %q", name)
		}
		content := ""
		if name == "Makefile" {
			content, err = runGit(repoPath, "cat-file", "blob", fields[2])
			if err != nil {
				return "", fmt.Errorf("read verification Makefile: %w", err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			return "", fmt.Errorf("materialize verification metadata: %w", err)
		}
	}
	success = true
	return dir, nil
}
