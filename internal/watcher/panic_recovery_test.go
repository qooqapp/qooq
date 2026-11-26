package watcher

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// These tests exercise the recover boundaries added around per-item work in
// the build worker (sweepTargetSafe/recoverTargetPanic, sweep.go) and the
// stack worker (sweepStackSafe/recoverStackPanic, stack_deploy.go).
//
// Reproducing a genuine panic deep inside sweepTarget/sweepStack would mean
// faking out git clones and the qooqd client, which tests real panic-recovery
// mechanics with a mountain of unrelated setup. Instead these drive the exact
// recover+record functions the worker loops call per item, with a
// deliberately panicking closure standing in for "the pipeline blew up" —
// same mechanism, same code path, no pipeline needed to prove it.

// ---------------------------------------------------------------------------
// Build worker (queue.go's runBuildWorkers calls sweepTargetSafe per item)
// ---------------------------------------------------------------------------

// TestRecoverTargetPanicDoesNotPropagate proves the boundary itself: a panic
// inside the wrapped function must not escape recoverTargetPanic. This is
// what lets runBuildWorkers' for-select loop reach its next iteration instead
// of dying (and, since nothing catches it further up that goroutine, taking
// the whole qooq-watcher process down over one target's bad luck).
func TestRecoverTargetPanicDoesNotPropagate(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	seedTarget(t, w.db, "a")

	escaped := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				escaped = true
			}
		}()
		w.recoverTargetPanic("a", func() { panic("boom") })
	}()
	if escaped {
		t.Fatal("panic escaped recoverTargetPanic; it must be recovered here so the worker loop survives")
	}
}

// TestRecoverTargetPanicRecordsFailure proves the panic left a durable trace:
// LastBuildStatus/LastBuildError, the same fields an ordinary build failure
// sets, so it is visible from `qooq watch get` — not just a log line nobody
// happened to be watching for.
func TestRecoverTargetPanicRecordsFailure(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	tg := seedTarget(t, w.db, "a")

	w.recoverTargetPanic("a", func() { panic("boom") })

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusFailed {
		t.Errorf("LastBuildStatus = %q, want %q", got.LastBuildStatus, BuildStatusFailed)
	}
	if !strings.Contains(got.LastBuildError, "boom") {
		t.Errorf("LastBuildError = %q, want it to mention the recovered panic value", got.LastBuildError)
	}
}

// TestBuildWorkerLoopContinuesPastAPanickingItem simulates the worker loop's
// own body (queue.go's runBuildWorkers: pull a name, process it, move on) with
// two items where the first panics — proving processing continues to the
// second rather than the whole loop dying with it.
func TestBuildWorkerLoopContinuesPastAPanickingItem(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	seedTarget(t, w.db, "a")
	seedTarget(t, w.db, "b")

	var processed []string
	for _, name := range []string{"a", "b"} {
		name := name
		w.recoverTargetPanic(name, func() {
			if name == "a" {
				panic("boom")
			}
			processed = append(processed, name)
		})
	}

	if len(processed) != 1 || processed[0] != "b" {
		t.Fatalf("worker loop did not continue past the panicking item: processed=%v", processed)
	}
}

// TestRecoverTargetPanicOnVanishedTargetDoesNotError confirms a panic
// recovered for a target name that no longer exists (deleted/renamed between
// being queued and processed) does not itself error or panic — there is
// simply nothing left to record the failure on, same as sweepTarget's own
// "vanished before processing" case.
func TestRecoverTargetPanicOnVanishedTargetDoesNotError(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}

	escaped := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				escaped = true
			}
		}()
		w.recoverTargetPanic("does-not-exist", func() { panic("boom") })
	}()
	if escaped {
		t.Fatal("panic escaped recoverTargetPanic for a target that no longer exists")
	}
}

// ---------------------------------------------------------------------------
// Stack worker (queue.go's runStackWorker -> sweepStacks -> sweepStackSafe
// per due stack)
// ---------------------------------------------------------------------------

func TestRecoverStackPanicDoesNotPropagate(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	seedStack(t, w.db, "acme-prod")

	escaped := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				escaped = true
			}
		}()
		w.recoverStackPanic("acme-prod", func() { panic("boom") })
	}()
	if escaped {
		t.Fatal("panic escaped recoverStackPanic; it must be recovered here so the stack worker goroutine survives")
	}
}

func TestRecoverStackPanicRecordsFailure(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	seedStack(t, w.db, "acme-prod")

	w.recoverStackPanic("acme-prod", func() { panic("boom") })

	got, err := getStackByName(w.db, "acme-prod")
	if err != nil {
		t.Fatalf("getStackByName: %v", err)
	}
	if got.Status != StackStatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, StackStatusFailed)
	}
	if !strings.Contains(got.LastError, "boom") {
		t.Errorf("LastError = %q, want it to mention the recovered panic value", got.LastError)
	}
}

// TestStackWorkerPassContinuesPastAPanickingStack simulates sweepStacks' own
// loop body over two due stacks where the first panics — proving the pass
// reaches the second instead of the whole worker goroutine dying with it
// (which, per runStackWorker's comment, would silently stop every stack from
// ever deploying again).
func TestStackWorkerPassContinuesPastAPanickingStack(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	seedStack(t, w.db, "a")
	seedStack(t, w.db, "b")

	var processed []string
	for _, name := range []string{"a", "b"} {
		name := name
		w.recoverStackPanic(name, func() {
			if name == "a" {
				panic("boom")
			}
			processed = append(processed, name)
		})
	}

	if len(processed) != 1 || processed[0] != "b" {
		t.Fatalf("stack worker pass did not continue past the panicking stack: processed=%v", processed)
	}
}

func TestRecoverStackPanicOnVanishedStackDoesNotError(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}

	escaped := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				escaped = true
			}
		}()
		w.recoverStackPanic("does-not-exist", func() { panic("boom") })
	}()
	if escaped {
		t.Fatal("panic escaped recoverStackPanic for a stack that no longer exists")
	}
}

// ---------------------------------------------------------------------------
// Seeding helpers
// ---------------------------------------------------------------------------

// seedTarget creates a minimal, valid Target row so recordTargetPanic (via
// getTargetByName + recordFailure) has something real to update.
func seedTarget(t *testing.T, db *gorm.DB, name string) *Target {
	t.Helper()
	tg := &Target{
		Name:          name,
		RepoURL:       "https://example.test/" + name + ".git",
		ComponentName: name,
	}
	if err := db.Create(tg).Error; err != nil {
		t.Fatalf("seed target %q: %v", name, err)
	}
	return tg
}

// seedStack creates a minimal, valid Stack row so recordStackPanic (via
// getStackByName + touchStack) has something real to update.
func seedStack(t *testing.T, db *gorm.DB, name string) *Stack {
	t.Helper()
	s := &Stack{
		Name:    name,
		RepoURL: "https://example.test/" + name + ".git",
	}
	if err := db.Create(s).Error; err != nil {
		t.Fatalf("seed stack %q: %v", name, err)
	}
	return s
}
