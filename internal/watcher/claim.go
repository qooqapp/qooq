package watcher

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Claims are the cross-process half of targetLocks.
//
// targetLocks (module.go) is a sync.Map of mutexes, so it serializes "act on
// target X" between the goroutines of *one* watcher process. That was the
// whole problem for as long as there was only ever one. The moment two
// watchers share a database — which is what the postgres driver exists for —
// process A's mutex says nothing to process B: both see the same row come due,
// both clone, both build, both upsert the same component, and both record
// last_build_status = success. Whichever finishes last wins, and nothing
// anywhere reports a conflict. This repo has already fixed that shape twice
// (checkComponentUnclaimed, checkComponentDuplicate); this is the same failure
// one level up.
//
// The mechanism is a lease held in the row itself, taken with a conditional
// UPDATE: exactly the compare-and-swap idiom rescheduleTarget already uses, so
// correctness rests on the row's atomicity rather than on any ordering between
// processes.
//
// Deliberately *not* `SELECT ... FOR UPDATE SKIP LOCKED` held for the duration
// of the work, which is where the design note started: a build may run for the
// full -build-timeout (30 minutes by default), and a transaction open that long
// pins a connection and holds back vacuum for the whole table. SKIP LOCKED is
// the right tool when the work is short; a lease is the right tool when the
// work is long and the worker may die holding it.
//
// It also runs on sqlite, unchanged, and that is on purpose: a mechanism only
// exercised in the configuration nobody runs locally is the one that is broken
// when it finally matters. On a single process the lease is simply always
// granted — the mutex got there first.
const (
	// claimLease is how long a claim stays valid without renewal. It bounds
	// how long another watcher waits before it may take over work abandoned by
	// a process that was killed mid-build, so it wants to be short. It does
	// not bound the work itself: the renewal goroutine keeps extending it.
	claimLease = 2 * time.Minute

	// claimRenewInterval must leave room for a missed tick or a slow database
	// round trip inside claimLease, or a live worker could have its own claim
	// stolen while it is still building.
	claimRenewInterval = 30 * time.Second
)

// newInstanceID identifies this watcher process in claimed_by. The hostname is
// there so an operator reading the column learns which machine is holding a
// target; the ULID is what actually makes it unique, because two watchers on
// one host (or one host restarted into a reused pid) must not be able to claim
// each other's work by accident.
func newInstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "watcher"
	}
	return fmt.Sprintf("%s/%s", host, ulid.Make().String())
}

// claimTarget tries to take the lease on one target for owner.
//
// It succeeds when the row is unclaimed, already held by owner (a claim is
// re-entrant, so the /deploy handler and the sweep goroutine of one process do
// not deadlock each other behind the DB as well as the mutex), or held by
// someone whose lease has expired. It reports false — not an error — when the
// lease belongs to a live claimant: that is the ordinary outcome of two
// watchers seeing the same row, and the caller's job is to move on, not to log
// a failure.
func claimTarget(db *gorm.DB, name, owner string, now time.Time) (bool, error) {
	res := db.Model(&Target{}).
		Where("name = ?", name).
		Where("claimed_by = '' OR claimed_by IS NULL OR claimed_by = ? OR claimed_until IS NULL OR claimed_until < ?", owner, now).
		Updates(map[string]any{
			"claimed_by":    owner,
			"claimed_until": now.Add(claimLease),
		})
	if res.Error != nil {
		return false, res.Error
	}
	// Zero rows means either "someone else holds it" or "the row is gone".
	// Both end in the same place for every caller — there is nothing to do
	// with this target on this pass — so they are not worth distinguishing.
	return res.RowsAffected == 1, nil
}

// renewTargetClaim extends an existing claim. It only matches rows this owner
// still holds: if the lease expired and another watcher took over, renewal must
// fail rather than yank the target back out from under a build that is already
// running elsewhere.
func renewTargetClaim(db *gorm.DB, name, owner string, now time.Time) (bool, error) {
	res := db.Model(&Target{}).
		Where("name = ? AND claimed_by = ?", name, owner).
		Update("claimed_until", now.Add(claimLease))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// releaseTargetClaim drops a claim this owner holds, so the next sweep tick —
// here or on another watcher — can pick the target up without waiting out the
// lease. Releasing a claim held by someone else is a no-op by construction,
// which matters for the deferred release in Watcher.claim: a watcher whose
// renewal failed must not clear the new owner's claim on its way out.
func releaseTargetClaim(db *gorm.DB, name, owner string) error {
	return db.Model(&Target{}).
		Where("name = ? AND claimed_by = ?", name, owner).
		Updates(map[string]any{"claimed_by": "", "claimed_until": nil}).Error
}

// claim takes the lease for name and returns a release func, or ok=false when
// another live watcher holds it.
//
// The returned func stops the renewal goroutine and then releases, so a build
// that outlives claimLease keeps its claim for as long as it is actually
// running, and gives it up the moment it is not. A renewal that fails because
// the claim was lost stops renewing and says so in the log: the work in flight
// is then unprotected, but shouting about it is all this layer can honestly do
// — the build itself has no cancellation point here, and killing it would be a
// worse answer than letting it finish and be overwritten.
func (w *Watcher) claim(name string) (release func(), ok bool) {
	taken, err := claimTarget(w.db, name, w.instanceID, time.Now())
	if err != nil {
		// A database error is not "someone else has it". Refusing to proceed
		// is the safe reading: acting without a claim is exactly the race this
		// exists to prevent.
		w.log.Error("claim target failed", zap.String("target", name), zap.Error(err))
		return func() {}, false
	}
	if !taken {
		return func() {}, false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(claimRenewInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				held, err := renewTargetClaim(w.db, name, w.instanceID, time.Now())
				if err != nil {
					w.log.Error("renew target claim failed",
						zap.String("target", name), zap.Error(err))
					continue
				}
				if !held {
					w.log.Error("lost the claim on a target while working on it — another watcher may have taken it over",
						zap.String("target", name), zap.String("instance", w.instanceID))
					return
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
		if err := releaseTargetClaim(w.db, name, w.instanceID); err != nil {
			w.log.Error("release target claim failed", zap.String("target", name), zap.Error(err))
		}
	}, true
}
