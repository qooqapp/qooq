package watcher

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// newStaleFailureWatcher builds the minimal Watcher clearStaleFailure needs:
// a DB handle and a logger. The rest of the struct is irrelevant here because
// the helper never touches git, the qooqd client, or config.
func newStaleFailureWatcher(t *testing.T) *Watcher {
	t.Helper()
	return &Watcher{db: newTestDB(t), log: zap.NewNop()}
}

// TestClearStaleFailure_ClearsFailedAndPersists is the main case: a target
// whose last sweep failed at the detect phase is re-checked successfully and
// finds nothing new, so the stale failure must be retired both in memory and
// in the DB.
func TestClearStaleFailure_ClearsFailedAndPersists(t *testing.T) {
	w := newStaleFailureWatcher(t)
	buildAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	tg := &Target{
		Name:            "hellotest",
		RepoURL:         "https://example.test/repo",
		ComponentName:   "hellotest",
		LastCommit:      "abc123",
		LastBuildStatus: BuildStatusFailed,
		LastBuildError:  `resolve remote head: lookup example.test: no such host`,
		LastBuildAt:     buildAt,
	}
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatalf("create target: %v", err)
	}

	w.clearStaleFailure(tg)

	if tg.LastBuildStatus != BuildStatusSuccess {
		t.Errorf("in-memory status = %q, want %q", tg.LastBuildStatus, BuildStatusSuccess)
	}
	if tg.LastBuildError != "" {
		t.Errorf("in-memory error = %q, want empty", tg.LastBuildError)
	}

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusSuccess {
		t.Errorf("persisted status = %q, want %q", got.LastBuildStatus, BuildStatusSuccess)
	}
	if got.LastBuildError != "" {
		t.Errorf("persisted error = %q, want empty", got.LastBuildError)
	}
	// LastBuildAt must not move: no build ran during this sweep.
	if !got.LastBuildAt.UTC().Truncate(time.Second).Equal(buildAt) {
		t.Errorf("LastBuildAt = %v, want it untouched at %v", got.LastBuildAt, buildAt)
	}
	// LastCommit is the detect phase's business, not this helper's.
	if got.LastCommit != "abc123" {
		t.Errorf("LastCommit = %q, want it untouched", got.LastCommit)
	}
}

// TestClearStaleFailure_LeavesNonFailedAlone guards the guard clause: every
// other status is a legitimate current state and must survive untouched. In
// particular "never" must not be promoted to "success" — a target that has
// never deployed anything has not succeeded at anything.
func TestClearStaleFailure_LeavesNonFailedAlone(t *testing.T) {
	for _, status := range []string{BuildStatusNever, BuildStatusBuilding, BuildStatusSuccess} {
		t.Run(status, func(t *testing.T) {
			w := newStaleFailureWatcher(t)
			tg := &Target{
				Name:            "t-" + status,
				RepoURL:         "https://example.test/repo",
				ComponentName:   "c",
				LastBuildStatus: status,
				LastBuildError:  "kept",
			}
			if err := w.db.Create(tg).Error; err != nil {
				t.Fatalf("create target: %v", err)
			}

			w.clearStaleFailure(tg)

			if tg.LastBuildStatus != status {
				t.Errorf("in-memory status = %q, want %q", tg.LastBuildStatus, status)
			}
			got := getTargetByID(t, w.db, tg.ID)
			if got.LastBuildStatus != status {
				t.Errorf("persisted status = %q, want %q", got.LastBuildStatus, status)
			}
			if got.LastBuildError != "kept" {
				t.Errorf("persisted error = %q, want it untouched", got.LastBuildError)
			}
		})
	}
}
