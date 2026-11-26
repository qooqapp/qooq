package core

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// The gap this closes: nothing bounded an image build. A Dockerfile that never
// finishes held a build worker for as long as the daemon ran, and the watcher
// has a small fixed pool of them — so a few such repositories stopped builds for
// everybody, with nothing in any log saying why.
func TestABuildThatNeverFinishesIsAborted(t *testing.T) {
	rt := &fakeRuntime{buildBlocks: true}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{Timeout: 100 * time.Millisecond})

	start := time.Now()
	_, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"})
	if err == nil {
		t.Fatal("a build that never finishes returned success")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the build was not aborted promptly (took %s)", time.Since(start))
	}
	// A named error, so the API can report it as policy rather than masking it
	// as a 500 — which is what sends the author looking at their Dockerfile's
	// last line instead of at the clock.
	if !errors.Is(err, ErrBuildTimeout) {
		t.Errorf("error is not ErrBuildTimeout: %v", err)
	}
	// And the message names the limit that stopped it.
	if !strings.Contains(err.Error(), "100ms") {
		t.Errorf("error does not name the limit: %v", err)
	}
}

// Zero means unbounded, which is the pre-existing behaviour and stays the
// behaviour for a machine that never sets a limit.
func TestNoTimeoutConfiguredMeansNoDeadline(t *testing.T) {
	rt := &fakeRuntime{}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{})

	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if rt.lastBuild.MemoryBytes != 0 || rt.lastBuild.CPUs != 0 {
		t.Errorf("unset limits reached the runtime as %+v", rt.lastBuild)
	}
}

// Limits are machine policy and must reach the runtime exactly as configured —
// nothing between here and docker gets to reinterpret them.
func TestConfiguredLimitsReachTheRuntime(t *testing.T) {
	rt := &fakeRuntime{}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{MemoryBytes: 512 << 20, CPUs: 1.5})

	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if rt.lastBuild.MemoryBytes != 512<<20 {
		t.Errorf("memory = %d, want %d", rt.lastBuild.MemoryBytes, 512<<20)
	}
	if rt.lastBuild.CPUs != 1.5 {
		t.Errorf("cpus = %v, want 1.5", rt.lastBuild.CPUs)
	}
	// The build label policy still applies: limits must not have displaced it.
	if rt.lastBuild.Labels[LabelBuilt] != "true" {
		t.Error("the qooq.built label went missing")
	}
}

// Build cache is how a machine's disk actually disappears, and docker has no
// per-build quota — so the ceiling is maintained after the build.
func TestBuildCacheIsPrunedOnlyWhenACeilingIsSet(t *testing.T) {
	rt := &fakeRuntime{}
	svc := newTestService(rt, newFakeStore())

	svc.SetBuildLimits(BuildLimits{})
	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if rt.pruneCalls != 0 {
		t.Errorf("pruned with no ceiling configured (%d calls)", rt.pruneCalls)
	}

	svc.SetBuildLimits(BuildLimits{CacheReservedBytes: 10 << 30})
	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if rt.pruneCalls != 1 {
		t.Fatalf("prune calls = %d, want 1", rt.pruneCalls)
	}
	if rt.lastReserved != 10<<30 {
		t.Errorf("reserved = %d, want %d", rt.lastReserved, 10<<30)
	}
}

// Housekeeping must not fail a build that worked: refusing to report a
// successful build because the cache could not be trimmed turns a disk problem
// into a deployment failure.
func TestAFailedPruneDoesNotFailTheBuild(t *testing.T) {
	rt := &fakeRuntime{pruneErr: errors.New("daemon busy")}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{CacheReservedBytes: 1 << 30})

	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Fatalf("a failed prune failed the build: %v", err)
	}
}

// The gap this closes: the ceiling was maintained only after a SUCCESSFUL
// build, and the build that fills the disk fails — a `RUN dd` ends with "No
// space left on device" and a non-zero status. So the one disk bound docker
// lets us enforce at all was not enforced on the single path that breaches it.
func TestTheCacheCeilingIsMaintainedWhenTheBuildFails(t *testing.T) {
	rt := &fakeRuntime{buildFails: errors.New("No space left on device")}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{CacheReservedBytes: 1 << 30})

	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err == nil {
		t.Fatal("a failing build returned success")
	}
	if rt.pruneCalls != 1 {
		t.Fatalf("prune calls after a failed build = %d, want 1", rt.pruneCalls)
	}
	if rt.lastReserved != 1<<30 {
		t.Errorf("reserved = %d, want %d", rt.lastReserved, int64(1)<<30)
	}
}

// And the timeout path specifically, which is the one a deliberate attack
// takes: fill the disk slowly enough and the build is abandoned for taking too
// long rather than for failing.
//
// The assertion on the context is the load-bearing half. By the time this
// prune runs, the build's own context is cancelled — so handing it through
// would make the prune return instantly, log a warning, and leave the disk as
// full as it was. Counting the call alone would pass against that, which is
// the kind of check this repository has twice recorded as certifying nothing.
func TestTheCeilingIsMaintainedAfterATimeoutAndNotWithADeadContext(t *testing.T) {
	rt := &fakeRuntime{buildBlocks: true}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{
		Timeout:            50 * time.Millisecond,
		CacheReservedBytes: 1 << 30,
	})

	_, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"})
	if !errors.Is(err, ErrBuildTimeout) {
		t.Fatalf("want ErrBuildTimeout, got %v", err)
	}
	if rt.pruneCalls != 1 {
		t.Fatalf("prune calls after a timed-out build = %d, want 1", rt.pruneCalls)
	}
	if rt.pruneCtxErr != nil {
		t.Errorf("the prune was handed an already-cancelled context (%v), so it could not have reclaimed anything", rt.pruneCtxErr)
	}
}

var _ = runtime.BuildOptions{}

// A full disk takes every database on the machine down with it, so builds stop
// first — before the build is even started.
func TestABuildIsRefusedWhileTheDiskIsTooFull(t *testing.T) {
	rt := &fakeRuntime{}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{MinFreeDisk: 0.15, FreeDisk: func() (uint64, uint64, error) { return 9, 100, nil }})

	_, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"})
	if !errors.Is(err, ErrLowDisk) || !strings.Contains(err.Error(), "9% free") {
		t.Fatalf("err = %v, want ErrLowDisk with the numbers", err)
	}
	if rt.callCount("BuildImage") != 0 {
		t.Error("the build ran anyway")
	}

	svc.SetBuildLimits(BuildLimits{MinFreeDisk: 0.15, FreeDisk: func() (uint64, uint64, error) { return 40, 100, nil }})
	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Errorf("with 40%% free: %v", err)
	}
}

// A check that cannot run does not stop builds: that would be an outage of
// qooq's own making.
func TestAFreeDiskCheckThatFailsDoesNotStopBuilds(t *testing.T) {
	svc := newTestService(&fakeRuntime{}, newFakeStore())
	svc.SetBuildLimits(BuildLimits{MinFreeDisk: 0.15, FreeDisk: func() (uint64, uint64, error) { return 0, 0, errors.New("statfs: nope") }})
	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"x:1"}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestAnImageOverTheLimitIsRemovedAndRefused(t *testing.T) {
	rt := &fakeRuntime{builtSize: 3 << 30}
	svc := newTestService(rt, newFakeStore())
	svc.SetBuildLimits(BuildLimits{ImageMaxBytes: 2 << 30})

	_, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"app:abc", "reg/app:abc"})
	if !errors.Is(err, ErrImageTooLarge) || !strings.Contains(err.Error(), "3.0 GB") || !strings.Contains(err.Error(), "2.0 GB") {
		t.Fatalf("err = %v, want ErrImageTooLarge with both sizes", err)
	}
	if rt.callCount("RemoveImage") != 2 {
		t.Errorf("removed %d tags, want both", rt.callCount("RemoveImage"))
	}

	rt.builtSize = 1 << 30
	if _, err := svc.BuildImage(context.Background(), bytes.NewReader(nil), "Dockerfile", []string{"app:def"}); err != nil {
		t.Errorf("a 1 GB image was refused: %v", err)
	}
}
