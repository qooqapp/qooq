package watcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// newTestDB opens an isolated in-memory sqlite DB (one per test, single
// connection so the shared-cache :memory: file is not visible to other tests)
// with the Target schema migrated — the same driver/config the watcher uses at
// runtime, so time round-tripping behaves identically to production.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&Target{}, &Stack{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func getTargetByID(t *testing.T, db *gorm.DB, id uint) Target {
	t.Helper()
	var tg Target
	if err := db.First(&tg, id).Error; err != nil {
		t.Fatalf("load target %d: %v", id, err)
	}
	return tg
}

// TestRescheduleTarget_NormalPathAdvances is the risk #7 normal-path guard:
// with no poke, reschedule advances NextCheckAt to now+interval exactly as
// before. It reads dueBefore via dueTargets (mirroring the sweep loop) so it
// also proves the compare-and-swap's time value round-trips through gorm/sqlite
// losslessly — if it did not, the CAS would silently fail to apply here.
func TestRescheduleTarget_NormalPathAdvances(t *testing.T) {
	db := newTestDB(t)
	tg := &Target{
		Name:                "web",
		RepoURL:             "file:///repo",
		ComponentName:       "web",
		PollIntervalSeconds: 30,
		NextCheckAt:         time.Now().Add(-time.Hour),
	}
	if err := createTarget(db, tg); err != nil {
		t.Fatalf("createTarget: %v", err)
	}

	due, err := dueTargets(db, time.Now(), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("dueTargets returned %d, want 1", len(due))
	}
	dueBefore := due[0].NextCheckAt // what the sweep loop captures pre-build

	applied, err := rescheduleTarget(db, due[0].ID, 30*time.Second, dueBefore)
	if err != nil {
		t.Fatalf("rescheduleTarget: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true (no poke: reschedule must advance NextCheckAt)")
	}

	got := getTargetByID(t, db, due[0].ID)
	if !got.NextCheckAt.After(time.Now()) {
		t.Errorf("NextCheckAt = %v, want in the future (now+interval)", got.NextCheckAt)
	}
	// And the target must no longer be due, confirming it was pushed forward.
	stillDue, _ := dueTargets(db, time.Now(), "")
	if len(stillDue) != 0 {
		t.Errorf("target still due after normal reschedule, want not due")
	}
}

// TestRescheduleTarget_PokeDuringBuildPreserved is the risk #7 fix: a
// triggerNow (webhook/CLI trigger) landing while a target is being processed
// must NOT be overwritten by the post-build reschedule. The sweep captured
// dueBefore before the build; the poke moved NextCheckAt to "now"; reschedule
// with the stale dueBefore guard must be a no-op, leaving the target due
// immediately (picked up next tick) instead of delayed by a full poll interval.
func TestRescheduleTarget_PokeDuringBuildPreserved(t *testing.T) {
	db := newTestDB(t)
	tg := &Target{
		Name:                "web",
		RepoURL:             "file:///repo",
		ComponentName:       "web",
		PollIntervalSeconds: 300, // long interval: a lost poke would delay 5 min
		NextCheckAt:         time.Now().Add(-time.Hour),
	}
	if err := createTarget(db, tg); err != nil {
		t.Fatalf("createTarget: %v", err)
	}

	due, err := dueTargets(db, time.Now(), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("dueTargets returned %d, want 1", len(due))
	}
	dueBefore := due[0].NextCheckAt

	// Simulate a webhook/trigger firing mid-build, from another goroutine's
	// perspective: it sets NextCheckAt to "now".
	if err := triggerNow(db, "web"); err != nil {
		t.Fatalf("triggerNow: %v", err)
	}

	// Post-build reschedule with the pre-build guard value must not apply.
	applied, err := rescheduleTarget(db, due[0].ID, 300*time.Second, dueBefore)
	if err != nil {
		t.Fatalf("rescheduleTarget: %v", err)
	}
	if applied {
		t.Fatal("applied = true, want false (poke arrived mid-build; reschedule must not clobber it)")
	}

	// The poke survives: the target is still due immediately, not pushed out
	// by the poll interval.
	stillDue, _ := dueTargets(db, time.Now(), "")
	if len(stillDue) != 1 {
		t.Fatalf("target not due after poke, want due immediately (poke was lost)")
	}
	got := getTargetByID(t, db, due[0].ID)
	if got.NextCheckAt.After(time.Now()) {
		t.Errorf("NextCheckAt = %v, want <= now (poke preserved as 'check immediately')", got.NextCheckAt)
	}
}
