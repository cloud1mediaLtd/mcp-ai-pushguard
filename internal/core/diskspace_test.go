package core

import (
	"strings"
	"testing"
)

func TestCheckDiskSpaceAllowsAHealthyFilesystem(t *testing.T) {
	// The test's own temp dir is on a filesystem with room, or the machine
	// running this has bigger problems than the test.
	warning, err := CheckDiskSpace(t.TempDir())
	if err != nil {
		t.Fatalf("refused a healthy filesystem: %v", err)
	}
	_ = warning
}

// A path that cannot be stat'd must not block checks. Refusing to run the test
// suite because a stat failed would be a worse failure than the one this
// guards against.
func TestCheckDiskSpaceIsPermissiveWhenItCannotTell(t *testing.T) {
	if _, ok := FreeDiskBytes("/definitely/not/a/real/path/anywhere"); ok {
		t.Fatal("stat of a nonexistent path reported success")
	}
	warning, err := CheckDiskSpace("/definitely/not/a/real/path/anywhere")
	if err != nil || warning != "" {
		t.Fatalf("an unreadable filesystem must be treated as fine, got warning=%q err=%v", warning, err)
	}
}

func TestFreeDiskBytesReportsSomethingPlausible(t *testing.T) {
	free, ok := FreeDiskBytes(t.TempDir())
	if !ok {
		t.Fatal("could not stat the test temp dir")
	}
	if free == 0 {
		t.Fatal("reported zero free bytes on a filesystem that just accepted a directory")
	}
}

// The thresholds are only useful if the message says what to do about it.
func TestHumanBytesAndThresholdsReadUsefully(t *testing.T) {
	for _, tt := range []struct {
		n    uint64
		want string
	}{
		{512, "512 B"},
		{2 << 30, "2.0 GiB"},
		{1536 << 20, "1.5 GiB"},
	} {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
	if DiskCriticalBytes >= DiskWarnBytes {
		t.Fatal("the critical threshold must be below the warning threshold, or the warning can never fire")
	}
}

func TestDiskSpaceErrorNamesTheRecovery(t *testing.T) {
	// Not reachable through CheckDiskSpace without filling a disk, so assert on
	// the message directly: an error that does not say how to recover just
	// moves the confusion.
	_, err := CheckDiskSpace(t.TempDir())
	if err != nil {
		t.Skipf("this machine is genuinely low on space: %v", err)
	}
	for _, want := range []string{"pushguard clean", "go clean -cache"} {
		if !strings.Contains(diskCriticalMessageSample(), want) {
			t.Errorf("the critical message should name %q as a recovery step", want)
		}
	}
}

func diskCriticalMessageSample() string {
	_, err := checkDiskSpaceAgainst(1 << 20)
	if err == nil {
		return ""
	}
	return err.Error()
}
