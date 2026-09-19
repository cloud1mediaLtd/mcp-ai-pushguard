package core

import (
	"context"
	"testing"
)

// The three thresholds form a ladder, and the ORDER is the design: reclaim
// before warning, warn before refusing. If reclaim ever sinks below the warn
// line, the daemon starts refusing work while still holding the space it could
// have freed — which is the behaviour that let a 926 GB volume reach 100% full
// while enqueue politely declined new jobs.
func TestDiskThresholdLadder(t *testing.T) {
	if DiskReclaimBytes <= DiskWarnBytes {
		t.Fatalf("DiskReclaimBytes (%d) must be ABOVE DiskWarnBytes (%d): reclaim has to happen before enqueue starts warning",
			DiskReclaimBytes, DiskWarnBytes)
	}
	if DiskWarnBytes <= DiskCriticalBytes {
		t.Fatalf("DiskWarnBytes (%d) must be above DiskCriticalBytes (%d)", DiskWarnBytes, DiskCriticalBytes)
	}
}

// Reclaiming is destructive to a shared resource, so it must not fire on a
// healthy disk. This is the guard that keeps an hourly tick from emptying the
// build cache of a machine that has hundreds of gigabytes free.
func TestReclaimBuildCacheSkipsWhenThereIsRoom(t *testing.T) {
	t.Setenv("GOCACHE", t.TempDir())
	// The test's own temp dir is on a filesystem with room; if it were not,
	// the suite could not run at all.
	before, after, ran := ReclaimBuildCache(context.Background(), t.TempDir())
	if ran {
		t.Fatalf("reclaim ran on a filesystem with room (%s free) — it must only fire below DiskReclaimBytes", HumanBytes(before))
	}
	if before != after {
		t.Fatalf("a skipped reclaim must report the same free space twice, got %d then %d", before, after)
	}
}

// A path it cannot stat must be treated as "cannot tell", not as "no space".
// Guessing "full" here would empty the build cache on every tick.
func TestReclaimBuildCacheSkipsWhenItCannotStat(t *testing.T) {
	t.Setenv("GOCACHE", t.TempDir())
	_, _, ran := ReclaimBuildCache(context.Background(), "/definitely/not/a/real/path/pushguard-test")
	if ran {
		t.Fatal("reclaim ran against an unstattable path; a failed stat must not read as a full disk")
	}
}

func TestShouldReclaimBuildCacheAtSizeCeiling(t *testing.T) {
	if !shouldReclaimBuildCache(500<<30, true, BuildCacheReclaimBytes, true) {
		t.Fatal("a cache at the size ceiling must be reclaimed even when the disk has room")
	}
}

func TestShouldReclaimBuildCacheBelowSizeCeiling(t *testing.T) {
	if shouldReclaimBuildCache(500<<30, true, BuildCacheReclaimBytes-1, true) {
		t.Fatal("a cache below the size ceiling on a healthy disk must be retained")
	}
}

func TestShouldReclaimBuildCacheWhenDiskIsLow(t *testing.T) {
	if !shouldReclaimBuildCache(DiskReclaimBytes-1, true, 0, false) {
		t.Fatal("low disk space must reclaim even when cache size cannot be measured")
	}
}

// The engine must not pull the build cache out from under a running gate.
//
// Asserted on idle() rather than on ReclaimDiskIfIdle's return value, because
// that return cannot distinguish "skipped because busy" from "skipped because
// the disk has room" — and on any machine able to run this suite, the disk has
// room. Written the other way first, this test passed with the busy check
// deleted, which is worse than not having it.
func TestEngineIsNotIdleWhileAJobRuns(t *testing.T) {
	e := &Engine{}
	if !e.idle() {
		t.Fatal("a daemon with no jobs must report idle")
	}

	e.cancels = map[string]context.CancelFunc{"pg_busy": func() {}}
	if e.idle() {
		t.Fatal("a daemon with a running job must NOT report idle; clearing the shared build cache mid-gate slows the run holding it")
	}

	// And it recovers, so one stuck entry cannot disable reclaim forever.
	delete(e.cancels, "pg_busy")
	if !e.idle() {
		t.Fatal("idle must return once the job is gone")
	}
}

// The composed call still has to honour it.
func TestReclaimDiskIfIdleSkipsWhileBusy(t *testing.T) {
	e := &Engine{cancels: map[string]context.CancelFunc{"pg_busy": func() {}}}
	if _, _, ran := e.ReclaimDiskIfIdle(context.Background(), t.TempDir()); ran {
		t.Fatal("reclaim ran while a job was active")
	}
}
