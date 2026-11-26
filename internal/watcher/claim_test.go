package watcher

import (
	"testing"
	"time"

	"gorm.io/gorm"
)

// The claim exists for one reason: two watchers sharing a database must not
// both build the same target. Every test here is written so that removing the
// guard it checks makes it fail — the mechanism cannot be verified by the
// configuration it was written for (two processes), so the row-level contract
// is what gets pinned down instead.

func newClaimTarget(t *testing.T, db *gorm.DB, name string) *Target {
	t.Helper()
	tg := &Target{
		Name:                name,
		RepoURL:             "file:///repo",
		ComponentName:       name,
		PollIntervalSeconds: 30,
		NextCheckAt:         time.Now().Add(-time.Hour),
	}
	if err := createTarget(db, tg); err != nil {
		t.Fatalf("createTarget: %v", err)
	}
	return tg
}

// TestOnlyOneWatcherCanHoldATarget is the whole point. Drop the claimed_by
// condition from claimTarget's WHERE and both calls return true, which is
// exactly the production failure: both watchers clone, both build, both upsert
// the same component, and both record success.
func TestOnlyOneWatcherCanHoldATarget(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")
	now := time.Now()

	first, err := claimTarget(db, "web", "watcher-a", now)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first {
		t.Fatal("the first watcher must get the claim on an unheld target")
	}

	second, err := claimTarget(db, "web", "watcher-b", now)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatal("a second watcher got the same target: both would build it and both would report success")
	}
}

// TestAClaimIsReentrantForItsOwner keeps the /deploy handler and the sweep
// goroutine of one process from deadlocking behind the database as well as the
// mutex. They are already serialized by targetLocks, so the second claim is
// the same worker arriving again, not a competitor.
func TestAClaimIsReentrantForItsOwner(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")
	now := time.Now()

	if ok, err := claimTarget(db, "web", "watcher-a", now); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err := claimTarget(db, "web", "watcher-a", now.Add(time.Second))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !ok {
		t.Fatal("a watcher must be able to reclaim a target it already holds")
	}
}

// TestAnExpiredClaimCanBeTakenOver is why this is a lease and not a lock. A
// watcher killed mid-build leaves its claim behind; without expiry that target
// would never be built again by anyone, and the symptom would be one project
// silently frozen while every other project kept deploying.
func TestAnExpiredClaimCanBeTakenOver(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")

	dead := time.Now().Add(-time.Hour)
	if ok, err := claimTarget(db, "web", "watcher-a", dead); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}

	// claimLease after `dead` is still long in the past, so the lease has
	// lapsed by now.
	ok, err := claimTarget(db, "web", "watcher-b", time.Now())
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !ok {
		t.Fatal("an expired claim must be takeable, or a watcher that died holding one freezes that target forever")
	}
}

// TestRenewalKeepsALongBuildsClaimAlive: a build may run for the full
// -build-timeout, which is far longer than claimLease, so without renewal a
// watcher would lose the target it is actively building and a second one would
// start on it.
func TestRenewalKeepsALongBuildsClaimAlive(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")
	start := time.Now()

	if ok, err := claimTarget(db, "web", "watcher-a", start); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// Mid-build, well past the original lease.
	later := start.Add(claimLease + time.Minute)
	if ok, err := renewTargetClaim(db, "web", "watcher-a", later); err != nil || !ok {
		t.Fatalf("renew: ok=%v err=%v", ok, err)
	}
	// Another watcher must still be locked out at that later time.
	ok, err := claimTarget(db, "web", "watcher-b", later.Add(time.Second))
	if err != nil {
		t.Fatalf("competing claim: %v", err)
	}
	if ok {
		t.Fatal("renewal did not extend the lease: a second watcher took over a build in progress")
	}
}

// TestRenewalFailsOnceTheClaimIsGone stops a watcher from yanking a target
// back out from under whoever took it over after its lease lapsed. Renewal has
// to be conditional on still owning the row, not an unconditional write.
func TestRenewalFailsOnceTheClaimIsGone(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")
	now := time.Now()

	if ok, err := claimTarget(db, "web", "watcher-a", now.Add(-time.Hour)); err != nil || !ok {
		t.Fatalf("claim a: ok=%v err=%v", ok, err)
	}
	if ok, err := claimTarget(db, "web", "watcher-b", now); err != nil || !ok {
		t.Fatalf("takeover by b: ok=%v err=%v", ok, err)
	}

	ok, err := renewTargetClaim(db, "web", "watcher-a", now)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if ok {
		t.Fatal("the old owner renewed a claim it had lost, which would point two watchers at one build")
	}
}

// TestReleaseFreesTheTargetImmediately: the next tick should pick a finished
// target up without waiting out the rest of the lease, otherwise every build
// is followed by up to claimLease of nothing happening.
func TestReleaseFreesTheTargetImmediately(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")
	now := time.Now()

	if ok, err := claimTarget(db, "web", "watcher-a", now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := releaseTargetClaim(db, "web", "watcher-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err := claimTarget(db, "web", "watcher-b", now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if !ok {
		t.Fatal("a released target must be claimable at once, not after the lease runs out")
	}
}

// TestReleasingSomeoneElsesClaimDoesNothing guards the deferred release in
// Watcher.claim: if a renewal failed and the target was taken over, the losing
// watcher's release must not clear the new owner's claim on its way out.
func TestReleasingSomeoneElsesClaimDoesNothing(t *testing.T) {
	db := newTestDB(t)
	tg := newClaimTarget(t, db, "web")
	now := time.Now()

	if ok, err := claimTarget(db, "web", "watcher-b", now); err != nil || !ok {
		t.Fatalf("claim b: ok=%v err=%v", ok, err)
	}
	if err := releaseTargetClaim(db, "web", "watcher-a"); err != nil {
		t.Fatalf("release by a: %v", err)
	}
	got := getTargetByID(t, db, tg.ID)
	if got.ClaimedBy != "watcher-b" {
		t.Fatalf("claimed_by = %q, want it left on watcher-b", got.ClaimedBy)
	}
}

// TestDueTargetsHidesWhatAnotherWatcherHolds keeps every watcher from spending
// a build worker per tick to rediscover work it cannot do. The expired half of
// the test matters just as much: an abandoned claim must come back into the
// batch, or nothing ever takes the work over.
func TestDueTargetsHidesWhatAnotherWatcherHolds(t *testing.T) {
	db := newTestDB(t)
	newClaimTarget(t, db, "web")

	if ok, err := claimTarget(db, "web", "watcher-b", time.Now()); err != nil || !ok {
		t.Fatalf("claim b: ok=%v err=%v", ok, err)
	}

	due, err := dueTargets(db, time.Now(), "watcher-a")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("got %d due targets, want 0: a target another watcher is building must stay out of the batch", len(due))
	}

	// The holder itself still sees it — otherwise a single watcher could never
	// re-examine a target it had claimed.
	mine, err := dueTargets(db, time.Now(), "watcher-b")
	if err != nil {
		t.Fatalf("dueTargets for holder: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("holder saw %d due targets, want 1", len(mine))
	}

	// Lease lapsed: it has to reappear for everyone.
	stale := time.Now().Add(claimLease + time.Minute)
	back, err := dueTargets(db, stale, "watcher-a")
	if err != nil {
		t.Fatalf("dueTargets after expiry: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("got %d due targets after the lease expired, want 1", len(back))
	}
}

// TestInstanceIDsAreDistinct: two watchers on one host must not be able to
// claim each other's work by accident, so the id cannot be the hostname alone.
func TestInstanceIDsAreDistinct(t *testing.T) {
	a, b := newInstanceID(), newInstanceID()
	if a == b {
		t.Fatalf("two instance ids came out identical (%q): two watchers on one host would share claims", a)
	}
	if a == "" {
		t.Fatal("instance id must not be empty: an empty claimed_by reads as unclaimed")
	}
}
