package watcher

import (
	"errors"
	"testing"

	"go.uber.org/zap"
)

// newStatusWatcher is the smallest Watcher that setBuilding needs: a db and a
// logger. The pipeline around it is not exercised here — what is under test is
// the bookkeeping every path through it depends on.
func newStatusWatcher(t *testing.T) *Watcher {
	t.Helper()
	return &Watcher{db: newTestDB(t), log: zap.NewNop()}
}

func saveTarget(t *testing.T, w *Watcher, tg *Target) {
	t.Helper()
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatalf("create target: %v", err)
	}
}

// A build that starts must not keep serving the previous attempt's error.
// Leaving it in place is what made an automated caller conclude that a fix had
// failed the same way as the code it was fixing: the status said "building"
// while the error text still described the run before it.
func TestSetBuildingRetiresThePreviousError(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{
		Name:            "web",
		ComponentName:   "web",
		LastCommit:      "old111",
		LastBuildStatus: BuildStatusFailed,
		LastBuildError:  "invalid yaml: mapping values are not allowed in this context",
	}
	saveTarget(t, w, tg)

	w.setBuilding(tg, "new222")

	for _, got := range []Target{*tg, getTargetByID(t, w.db, tg.ID)} {
		if got.LastBuildStatus != BuildStatusBuilding {
			t.Errorf("status = %q, want %q", got.LastBuildStatus, BuildStatusBuilding)
		}
		if got.LastBuildError != "" {
			t.Errorf("error survived the start of a new build: %q", got.LastBuildError)
		}
		if got.LastBuildRevision != "new222" {
			t.Errorf("revision = %q, want the commit being built", got.LastBuildRevision)
		}
	}
}

// The revision is what makes the status answerable: LastCommit only moves on
// success, so while a build runs it still names the previous one. A caller
// polling for "the build for the commit I pushed" has to read the revision.
func TestBuildRevisionLeadsLastCommitWhileBuilding(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", LastCommit: "old111"}
	saveTarget(t, w, tg)

	w.setBuilding(tg, "new222")

	if tg.LastCommit != "old111" {
		t.Errorf("last_commit moved before the build finished: %q", tg.LastCommit)
	}
	if tg.LastBuildRevision != "new222" {
		t.Errorf("last_build_revision = %q, want new222", tg.LastBuildRevision)
	}
}

// A retry and a forced rebuild both run at a revision that has not changed, so
// the revision alone cannot say whether a new attempt began. The generation can.
func TestBuildGenerationRisesEvenWhenTheRevisionRepeats(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web"}
	saveTarget(t, w, tg)

	if tg.BuildGeneration != 0 {
		t.Fatalf("a target that never built has generation %d, want 0", tg.BuildGeneration)
	}
	w.setBuilding(tg, "same333")
	first := tg.BuildGeneration
	w.setBuilding(tg, "same333")

	if first != 1 {
		t.Errorf("first build generation = %d, want 1", first)
	}
	if tg.BuildGeneration != 2 {
		t.Errorf("generation after a second build = %d, want 2", tg.BuildGeneration)
	}
	if got := getTargetByID(t, w.db, tg.ID); got.BuildGeneration != 2 {
		t.Errorf("persisted generation = %d, want 2", got.BuildGeneration)
	}
}

// A success reports the same revision it started with, so the pair a caller
// polls on stays consistent from "building" through to the final status.
func TestRevisionSurvivesToTheRecordedSuccess(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", LastCommit: "old111"}
	saveTarget(t, w, tg)

	w.setBuilding(tg, "new222")
	if err := w.recordSuccess(tg, "new222"); err != nil {
		t.Fatalf("recordSuccess: %v", err)
	}

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusSuccess {
		t.Errorf("status = %q, want success", got.LastBuildStatus)
	}
	if got.LastBuildRevision != "new222" || got.LastCommit != "new222" {
		t.Errorf("revision/commit = %q/%q, want both new222", got.LastBuildRevision, got.LastCommit)
	}
	if got.LastBuildError != "" {
		t.Errorf("success left an error behind: %q", got.LastBuildError)
	}
}

// A failure keeps the revision it failed at, so "which commit is broken" is
// answerable without inferring it from LastCommit (which deliberately does not
// advance past a failure).
func TestFailureKeepsTheRevisionItFailedAt(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", LastCommit: "old111"}
	saveTarget(t, w, tg)

	w.setBuilding(tg, "new222")
	w.recordFailure(tg, errors.New("docker build failed"))

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.LastBuildStatus)
	}
	if got.LastBuildRevision != "new222" {
		t.Errorf("revision = %q, want the commit that failed", got.LastBuildRevision)
	}
	if got.LastCommit != "old111" {
		t.Errorf("last_commit advanced past a failure: %q", got.LastCommit)
	}
}
