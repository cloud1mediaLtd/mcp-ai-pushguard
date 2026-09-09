package core

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// DiskReclaimBytes is where the daemon reclaims the Go build cache on its own.
//
// It sits ABOVE the enqueue thresholds on purpose, so the ladder reads:
//
//	20 GiB  reclaim the build cache (here, hourly, only when idle)
//	 5 GiB  enqueue warns                          (DiskWarnBytes)
//	 2 GiB  enqueue refuses                        (DiskCriticalBytes)
//
// Reclaiming first means the lower two are a backstop rather than the normal
// experience. Refusing to enqueue is the correct behaviour but it is not a
// solution: it stops new work without freeing anything, so a full disk stays
// full and every queued gate keeps failing.
const DiskReclaimBytes = 20 << 30

// ReclaimBuildCache empties the Go build cache when free disk has fallen below
// DiskReclaimBytes.
//
// This exists because the Go build cache has NO size limit, which is not
// obvious from its documentation. Its only self-management is deleting entries
// unused for five days, and that cannot bound a cache whose entries are
// re-touched constantly: every gate builds the full tree, and every commit
// changes enough content hashes that new entries appear faster than old ones
// age out. On 2026-09-09 it reached 171 GB and took a 926 GB volume to 100%
// full with three agent sessions active.
//
// The failure is worse than a full disk because it does not look like one. A
// gate dies with "FAIL <package> [build failed]" and errno=28 buried among
// hundreds of passing packages, naming packages nobody touched. Three gates
// were misread as code failures that day before anyone ran df.
//
// Returns the bytes free before and after, and whether it actually ran. It is
// deliberately all-or-nothing: `go clean -cache` is the only cache eviction the
// toolchain exposes, so there is no way to trim to a target size. The cost is a
// cold cache on the next build, which is minutes; the cost of not doing it is a
// gate that fails with a misattributed compile error, which is an hour.
//
// The caller is responsible for ensuring no job is running — see
// Engine.PruneDiskArtifacts. Clearing the cache under a live build is not
// corrupting (the toolchain rebuilds what it cannot find) but it does produce
// spurious "directory not empty" errors and slows the very run that is holding
// the lock.
func ReclaimBuildCache(ctx context.Context, probePath string) (before, after uint64, ran bool) {
	before, ok := FreeDiskBytes(probePath)
	if !ok || before >= DiskReclaimBytes {
		return before, before, false
	}
	if _, err := exec.LookPath("go"); err != nil {
		// No toolchain on the daemon's PATH: nothing to reclaim, and this is
		// not an error worth surfacing on a machine that only runs shell gates.
		return before, before, false
	}

	cleanCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := runGoCleanCache(cleanCtx); err != nil {
		// A concurrent writer can make the first pass fail with
		// "unlinkat ...: directory not empty" while having removed most of the
		// tree. One retry settles it; observed 2026-09-09.
		if strings.Contains(err.Error(), "directory not empty") {
			_ = runGoCleanCache(cleanCtx)
		}
	}

	after, ok = FreeDiskBytes(probePath)
	if !ok {
		return before, before, true
	}
	return before, after, true
}

func runGoCleanCache(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "go", "clean", "-cache")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &cleanError{err: err, output: strings.TrimSpace(string(out))}
	}
	return nil
}

type cleanError struct {
	err    error
	output string
}

func (e *cleanError) Error() string {
	if e.output == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + e.output
}

// HumanBytes formats a byte count for an operator-facing message.
func HumanBytes(n uint64) string { return humanBytes(n) }
