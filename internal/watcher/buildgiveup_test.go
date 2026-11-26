package watcher

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/glebarez/sqlite"
)

func giveUpWatcher(t *testing.T) *Watcher {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/w.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Target{}, &Stack{}, &Secret{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Watcher{log: zap.NewNop(), db: db}
}

func mkTarget(t *testing.T, w *Watcher, name string) *Target {
	t.Helper()
	tg := &Target{Name: name, ComponentName: name, RepoURL: "file:///x", Branch: "main"}
	if err := createTarget(w.db, tg); err != nil {
		t.Fatalf("create target: %v", err)
	}
	return tg
}

// The gap: a repository that can never build was retried once per poll interval
// forever, each attempt cloning and occupying one of a small pool of build
// workers. On a shared watcher that is capacity taken from other users.
func TestATargetThatAlwaysFailsEventuallyStopsBeingRetried(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "broken")

	for i := 1; i < maxConsecutiveBuildFailures; i++ {
		if reason := w.noteBuildFailure(tg); reason != "" {
			t.Fatalf("paused after only %d failures", i)
		}
	}
	reason := w.noteBuildFailure(tg)
	if reason == "" {
		t.Fatalf("still not paused after %d failures", maxConsecutiveBuildFailures)
	}
	// The reason has to name the command that resumes, or the target is simply
	// stuck as far as anyone looking at it can tell.
	if !strings.Contains(reason, "qooq watch trigger broken") {
		t.Errorf("reason does not say how to resume: %q", reason)
	}

	// And it drops out of the work queue, which is the whole point: checking
	// inside the build would still pay for the clone.
	due, err := dueTargets(w.db, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	for _, d := range due {
		if d.Name == "broken" {
			t.Fatal("a paused target is still being scheduled")
		}
	}
}

// Below the threshold it keeps trying: the limit has to absorb a registry hiccup
// or a DNS blip without giving up on a repository that is fine.
func TestATargetIsStillScheduledBelowTheThreshold(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "flaky")

	for i := 0; i < maxConsecutiveBuildFailures-1; i++ {
		w.noteBuildFailure(tg)
	}
	due, err := dueTargets(w.db, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	var found bool
	for _, d := range due {
		if d.Name == "flaky" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a target gave up after only %d failures", maxConsecutiveBuildFailures-1)
	}
}

// A success clears the count, so an intermittent failure never accumulates
// towards a pause across unrelated weeks.
func TestASuccessClearsTheCount(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "recovers")

	for i := 0; i < 5; i++ {
		w.noteBuildFailure(tg)
	}
	w.clearBuildFailures(tg)
	if tg.ConsecutiveFailures != 0 || tg.PausedReason != "" {
		t.Fatalf("not cleared: %+v", tg)
	}

	var stored Target
	w.db.First(&stored, "name = ?", "recovers")
	if stored.ConsecutiveFailures != 0 {
		t.Fatalf("the database still holds %d", stored.ConsecutiveFailures)
	}
}

// Triggering is the resume — no second verb, because "check this now" is exactly
// what someone does after fixing the repository.
func TestTriggeringResumesAPausedTarget(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "broken")
	for i := 0; i < maxConsecutiveBuildFailures; i++ {
		w.noteBuildFailure(tg)
	}

	if err := triggerNow(w.db, "broken"); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	var stored Target
	w.db.First(&stored, "name = ?", "broken")
	if stored.PausedReason != "" {
		t.Fatalf("still paused after a trigger: %q", stored.PausedReason)
	}
	if stored.ConsecutiveFailures != 0 {
		t.Fatalf("count not reset: %d", stored.ConsecutiveFailures)
	}

	due, err := dueTargets(w.db, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	var found bool
	for _, d := range due {
		if d.Name == "broken" {
			found = true
		}
	}
	if !found {
		t.Fatal("a resumed target is still not being scheduled")
	}
}

// The pause has to be visible on the API, or a target that stopped building
// looks like one the sweep forgot.
func TestThePauseIsVisibleOnTheWireDTO(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "broken")
	for i := 0; i < maxConsecutiveBuildFailures; i++ {
		w.noteBuildFailure(tg)
	}
	var stored Target
	w.db.First(&stored, "name = ?", "broken")
	if got := stored.toResponse().PausedReason; got == "" {
		t.Fatal("PausedReason is not carried to the API response")
	}
}
