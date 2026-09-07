package core

import (
	"fmt"
	"syscall"
)

// Disk thresholds for the filesystem holding worktrees.
//
// A worktree is a full checkout: c1m-server's is ~900MB. Running out of space
// part-way through a gate does not produce a clean error — the Go build cache
// starts evicting entries it is mid-way through writing, and the run fails with
// something like "could not import crypto/internal/fips140/subtle: could not
// load export data", which points at the toolchain and not at the disk. That is
// a genuinely expensive thing to debug, so it is worth refusing early.
const (
	// DiskCriticalBytes is where enqueue refuses outright.
	DiskCriticalBytes = 2 << 30 // 2 GiB
	// DiskWarnBytes is where enqueue proceeds but says so.
	DiskWarnBytes = 5 << 30 // 5 GiB
)

// FreeDiskBytes reports the free space available on the filesystem containing
// path. A path it cannot stat reports a false ok, so callers can tell "no
// space" apart from "could not tell".
func FreeDiskBytes(path string) (free uint64, ok bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, false
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), true
}

// CheckDiskSpace reports whether there is room to create another worktree.
//
// It returns a fatal error below DiskCriticalBytes, a non-empty warning below
// DiskWarnBytes, and nothing when there is room. A filesystem it cannot read is
// treated as fine: refusing to run checks because a stat failed would be a worse
// failure than the one being guarded against.
func CheckDiskSpace(path string) (warning string, err error) {
	free, ok := FreeDiskBytes(path)
	if !ok {
		return "", nil
	}
	return checkDiskSpaceAgainst(free)
}

// checkDiskSpaceAgainst is the decision, split from the stat so the thresholds
// and the wording can be tested without filling a real disk.
func checkDiskSpaceAgainst(free uint64) (warning string, err error) {
	switch {
	case free < DiskCriticalBytes:
		return "", fmt.Errorf("only %s free on the filesystem holding worktrees; a run that fills the disk fails with confusing toolchain errors rather than a disk error. Free space (`pushguard clean`, or `go clean -cache`) and re-enqueue", humanBytes(free))
	case free < DiskWarnBytes:
		return fmt.Sprintf("only %s free on the filesystem holding worktrees — a large checkout may exhaust it mid-run", humanBytes(free)), nil
	}
	return "", nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
