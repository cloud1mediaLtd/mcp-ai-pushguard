package hook

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestInstallPrePushHook(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pushguard-hook-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	// 1. Install sync hook
	hookPath, err := InstallPrePushHook(tempDir, false)
	if err != nil {
		t.Fatalf("failed to install sync hook: %v", err)
	}

	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("hook file missing: %v", err)
	}
	// Check executable bits
	if info.Mode()&0111 == 0 {
		t.Errorf("expected hook to be executable, mode is %v", info.Mode())
	}

	content, _ := os.ReadFile(hookPath)
	if !strings.Contains(string(content), "--wait") {
		t.Errorf("expected sync hook to contain '--wait'")
	}

	// 2. Install async hook (should backup existing)
	hookPath2, err := InstallPrePushHook(tempDir, true)
	if err != nil {
		t.Fatalf("failed to install async hook: %v", err)
	}

	content2, _ := os.ReadFile(hookPath2)
	if !strings.Contains(string(content2), "--auto-push") {
		t.Errorf("expected async hook to contain '--auto-push'")
	}

	backupPath := hookPath + ".backup"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Errorf("expected backup file %s to exist", backupPath)
	}
}
