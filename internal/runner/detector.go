package runner

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// DetectedSuite describes a detected test/check command and the reason for choosing it.
type DetectedSuite struct {
	Command []string
	Source  string
}

// DetectCommand analyzes the files in the directory to pick an appropriate test command.
func DetectCommand(dir string) DetectedSuite {
	// 1. Check Makefile for pre-push / check / test targets
	makefilePath := filepath.Join(dir, "Makefile")
	if _, err := os.Stat(makefilePath); err == nil {
		if hasMakeTarget(makefilePath, "pre-push") {
			return DetectedSuite{Command: []string{"make", "pre-push"}, Source: "Makefile (pre-push target)"}
		}
		if hasMakeTarget(makefilePath, "check") {
			return DetectedSuite{Command: []string{"make", "check"}, Source: "Makefile (check target)"}
		}
		if hasMakeTarget(makefilePath, "test") {
			return DetectedSuite{Command: []string{"make", "test"}, Source: "Makefile (test target)"}
		}
	}

	// 2. Go project
	if fileExists(filepath.Join(dir, "go.mod")) {
		return DetectedSuite{Command: []string{"go", "test", "./..."}, Source: "go.mod"}
	}

	// 3. Node.js / JavaScript / TypeScript projects
	if fileExists(filepath.Join(dir, "pnpm-lock.yaml")) {
		return DetectedSuite{Command: []string{"pnpm", "test"}, Source: "pnpm-lock.yaml"}
	}
	if fileExists(filepath.Join(dir, "yarn.lock")) {
		return DetectedSuite{Command: []string{"yarn", "test"}, Source: "yarn.lock"}
	}
	if fileExists(filepath.Join(dir, "bun.lockb")) || fileExists(filepath.Join(dir, "bun.lock")) {
		return DetectedSuite{Command: []string{"bun", "test"}, Source: "bun.lock"}
	}
	if fileExists(filepath.Join(dir, "package.json")) {
		return DetectedSuite{Command: []string{"npm", "test"}, Source: "package.json"}
	}

	// 4. Rust project
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return DetectedSuite{Command: []string{"cargo", "test"}, Source: "Cargo.toml"}
	}

	// 5. Python project
	if fileExists(filepath.Join(dir, "pytest.ini")) || fileExists(filepath.Join(dir, "pyproject.toml")) {
		return DetectedSuite{Command: []string{"pytest"}, Source: "Python configuration"}
	}

	// Fallback generic
	return DetectedSuite{Command: []string{"echo", "No automated test suite detected"}, Source: "fallback"}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func hasMakeTarget(makefilePath, target string) bool {
	f, err := os.Open(makefilePath)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, target+":") {
			return true
		}
	}
	return false
}
