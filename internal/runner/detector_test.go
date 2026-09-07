package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectCommand(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pushguard-detect-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Case 1: Empty fallback
	d1 := DetectCommand(tempDir)
	if d1.Source != "fallback" {
		t.Errorf("expected fallback, got %s", d1.Source)
	}

	// Case 2: go.mod
	goMod := filepath.Join(tempDir, "go.mod")
	_ = os.WriteFile(goMod, []byte("module test\n"), 0644)
	d2 := DetectCommand(tempDir)
	expectedCmd := []string{"go", "test", "./..."}
	if !reflect.DeepEqual(d2.Command, expectedCmd) {
		t.Errorf("expected %v, got %v", expectedCmd, d2.Command)
	}

	// Case 3: Makefile pre-push
	makefile := filepath.Join(tempDir, "Makefile")
	_ = os.WriteFile(makefile, []byte("pre-push:\n\t@echo ok\n"), 0644)
	d3 := DetectCommand(tempDir)
	if !reflect.DeepEqual(d3.Command, []string{"make", "pre-push"}) {
		t.Errorf("expected make pre-push, got %v", d3.Command)
	}
}
