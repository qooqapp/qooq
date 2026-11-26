package watcher

import (
	"sync"
	"testing"
)

// A build outlasts many sweep ticks, and the target stays due for all of them.
// Without this the queue would accumulate one copy per tick and the same repo
// would be cloned and built over and over.
func TestQueueSkipsWhatIsAlreadyInFlight(t *testing.T) {
	q := newBuildQueue(0)

	if q.add("web", "default") != queueAccepted {
		t.Fatal("first add refused")
	}
	for i := 0; i < 5; i++ {
		if q.add("web", "default") == queueAccepted {
			t.Fatalf("add %d queued a target that is already in flight", i+2)
		}
	}
	if got := q.inFlight(); got != 1 {
		t.Errorf("in flight = %d, want 1", got)
	}

	q.done("web")
	if q.add("web", "default") != queueAccepted {
		t.Error("a finished target could not be queued again")
	}
}

// Different targets are independent — that is the whole point of the change.
func TestQueueAcceptsDifferentTargetsAtOnce(t *testing.T) {
	q := newBuildQueue(0)
	for _, name := range []string{"api", "web", "admin"} {
		if q.add(name, "default") != queueAccepted {
			t.Errorf("%s was refused", name)
		}
	}
	if got := q.inFlight(); got != 3 {
		t.Errorf("in flight = %d, want 3", got)
	}
}

// Two sweeps overlapping (a tick that runs long, say) must not both hand the
// same target to a worker.
func TestQueueAddIsSafeUnderConcurrency(t *testing.T) {
	q := newBuildQueue(0)
	const n = 50

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.add("web", "default") == queueAccepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != 1 {
		t.Errorf("%d goroutines queued the same target, want exactly 1", accepted)
	}
}

// A full queue drops the offer rather than blocking the sweep, and must not
// leave the target claimed — it is still due, so the next tick has to be able
// to offer it again. Leaking the claim would strand a target forever.
func TestQueueOverflowLeavesNoClaimBehind(t *testing.T) {
	q := newBuildQueue(0)
	// Fill the channel without draining it.
	for i := 0; i < buildQueueCapacity; i++ {
		if q.add(uniqueName(i), "default") != queueAccepted {
			t.Fatalf("add %d refused before the queue was full", i)
		}
	}

	overflow := "one-too-many"
	if q.add(overflow, "default") == queueAccepted {
		t.Fatal("a full queue accepted another target")
	}
	// Drain one slot, as a worker would, and the refused target must now fit.
	<-q.names
	if q.add(overflow, "default") != queueAccepted {
		t.Error("the refused target stayed claimed and could never be queued again")
	}
}

func uniqueName(i int) string {
	return "t" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

// --- per-namespace concurrency (build isolation, step 3) -------------------

// sweep.workers is one global pool, so without a per-namespace cap a user
// pushing three repositories at once fills it and everybody else waits. That
// needs no malice, which is why this is a cap and not a warning.
func TestQueueHoldsATargetWhoseNamespaceIsAtTheLimit(t *testing.T) {
	q := newBuildQueue(1)

	if got := q.add("acme-api", "acme"); got != queueAccepted {
		t.Fatalf("first acme target = %v, want accepted", got)
	}
	if got := q.add("acme-web", "acme"); got != queueNamespaceAtLimit {
		t.Errorf("second acme target = %v, want held by the namespace limit", got)
	}
	// The cap is per namespace, not global: another user must not be made to
	// wait for acme, which is the entire point.
	if got := q.add("globex-api", "globex"); got != queueAccepted {
		t.Errorf("globex target = %v, want accepted — the cap is per namespace", got)
	}
}

// A held target must be left unclaimed, or it could never be queued again:
// sweepOnce does not touch NextCheckAt for a target it failed to queue, so the
// next tick is the only thing that will offer it.
func TestAHeldTargetIsNotLeftClaimed(t *testing.T) {
	q := newBuildQueue(1)
	q.add("acme-api", "acme")
	q.add("acme-web", "acme") // held

	if got := q.inFlight(); got != 1 {
		t.Fatalf("in flight = %d, want 1 — a held target must not count as in flight", got)
	}
	q.done("acme-api")
	if got := q.add("acme-web", "acme"); got != queueAccepted {
		t.Errorf("after a slot freed, the held target = %v, want accepted", got)
	}
}

// Finishing a build has to release the namespace's slot, not just the target's
// name. Releasing one without the other wedges the namespace at its cap with
// nothing actually building.
func TestFinishingABuildFreesTheNamespaceSlot(t *testing.T) {
	q := newBuildQueue(2)
	q.add("a", "acme")
	q.add("b", "acme")
	if got := q.add("c", "acme"); got != queueNamespaceAtLimit {
		t.Fatalf("third = %v, want held", got)
	}

	q.done("a")
	if got := q.add("c", "acme"); got != queueAccepted {
		t.Errorf("after one finished, third = %v, want accepted", got)
	}

	q.done("b")
	q.done("c")
	q.mu.Lock()
	n := len(q.inNamespace)
	q.mu.Unlock()
	// Not merely tidiness: the map is keyed by namespace, and a watcher that
	// runs for months would otherwise hold one entry per namespace that ever
	// built anything.
	if n != 0 {
		t.Errorf("inNamespace still holds %d entries after everything finished", n)
	}
}

// The default, and the one that matters for the offline product: every target
// without a namespace normalizes to "default", so a cap there would act as a
// global cap and serialize one person's projects.
func TestNoLimitLetsOneNamespaceUseEveryWorker(t *testing.T) {
	q := newBuildQueue(0)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if got := q.add(name, "default"); got != queueAccepted {
			t.Fatalf("%s = %v, want accepted — 0 must mean no limit", name, got)
		}
	}
	if got := q.inFlight(); got != 5 {
		t.Errorf("in flight = %d, want 5", got)
	}
}

// done() must decrement the counter add() incremented. The namespace a target
// was admitted under is remembered for exactly this reason: a deploy spec
// edited mid-build would otherwise send the decrement to a different counter,
// leaving the original namespace permanently one slot short.
func TestTheSlotIsReturnedToTheNamespaceThatTookIt(t *testing.T) {
	q := newBuildQueue(1)
	q.add("movable", "acme")

	q.mu.Lock()
	q.namespaceOf["movable"] = "acme" // as recorded at admission
	q.mu.Unlock()

	q.done("movable")
	if got := q.add("another", "acme"); got != queueAccepted {
		t.Errorf("acme = %v after its only build finished, want accepted", got)
	}
}

// Concurrent sweeps must not both slip past the cap.
func TestTheNamespaceLimitHoldsUnderConcurrency(t *testing.T) {
	q := newBuildQueue(2)
	const n = 50

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if q.add(uniqueName(i), "acme") == queueAccepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if accepted != 2 {
		t.Errorf("%d targets were admitted for one namespace, want exactly 2", accepted)
	}
}

// An account has several namespaces. Capping per namespace let one account
// fill every build worker; with a build group the cap is per account, and a
// target with none is still counted by namespace (the offline product).
func TestTheBuildCapCountsByBuildGroupWhenThereIsOne(t *testing.T) {
	q := newBuildQueue(1)
	prod := &Target{Name: "acme--prod_api", BuildGroup: "acme", DeploySpec: `{"namespace":"acme--prod"}`}
	staging := &Target{Name: "acme--staging_api", BuildGroup: "acme", DeploySpec: `{"namespace":"acme--staging"}`}
	other := &Target{Name: "bob--prod_api", BuildGroup: "bob", DeploySpec: `{"namespace":"bob--prod"}`}

	if r := q.add(prod.Name, buildGroupOf(prod)); r != queueAccepted {
		t.Fatalf("first build of the account: %v", r)
	}
	if r := q.add(staging.Name, buildGroupOf(staging)); r != queueNamespaceAtLimit {
		t.Errorf("a second namespace of the same account got its own share: %v", r)
	}
	if r := q.add(other.Name, buildGroupOf(other)); r != queueAccepted {
		t.Errorf("another account was held back: %v", r)
	}

	// No build group: counted by namespace, as before.
	a := &Target{Name: "a", DeploySpec: `{"namespace":"one"}`}
	b := &Target{Name: "b", DeploySpec: `{"namespace":"two"}`}
	if q.add(a.Name, buildGroupOf(a)) != queueAccepted || q.add(b.Name, buildGroupOf(b)) != queueAccepted {
		t.Error("targets without a build group were not counted per namespace")
	}
}
