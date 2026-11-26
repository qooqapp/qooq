package watcher

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// defaultBuildWorkers is how many targets may be built at once. Three is
// chosen to make the common case — a handful of projects that were pushed
// together — finish in about the time of the slowest one rather than the sum
// of all of them, without letting a machine run enough simultaneous docker
// builds to starve the apps already running on it.
const defaultBuildWorkers = 3

// buildQueueCapacity bounds the pending set. It is far above any plausible
// number of targets on one machine; overflowing is handled by dropping the
// enqueue, which is safe because a target that is still due is simply picked
// up by the next sweep tick.
const buildQueueCapacity = 1024

// buildQueue decides which targets are being worked on right now.
//
// It is not a message queue and deliberately does not become one. The durable
// record of "this target needs attention" is the targets table itself —
// NextCheckAt <= now is the ready set, it survives restarts, and
// rescheduleTarget already guards it against a poke that lands mid-build. A
// broker would be a second answer to the same question, and the two disagree
// in exactly the ways that are hardest to diagnose: a job delivered for a
// target that was deleted, or a target that is due with no job. This type only
// holds the part that is genuinely in-memory — what is in flight — so the same
// target is never built twice at once and a slow build does not stop the sweep
// from noticing everything else.
type buildQueue struct {
	names chan string

	mu       sync.Mutex
	enqueued map[string]bool
	// namespaceOf records which namespace each in-flight target was admitted
	// under, so done() decrements the counter that add() incremented even if
	// the target's deploy spec is edited while it builds.
	namespaceOf map[string]string
	inNamespace map[string]int
	// nsLimit caps how many of one namespace's targets may be in flight at
	// once; 0 means no cap. See newBuildQueue.
	nsLimit int
}

// newBuildQueue builds the queue. nsLimit caps concurrent builds per
// namespace, and 0 means unlimited.
//
// Unlimited is the right default, not a timid one. Every target whose deploy
// spec names no namespace is normalized to "default" (parseDeploySpec), so on
// a single-user install — the offline product, and every developer's laptop
// — all targets share one namespace and a per-namespace cap IS a global cap.
// Setting it to 1 there would serialize one person's three projects and undo
// the whole reason the sweep and the builds were separated.
//
// It earns its keep when namespaces belong to different people: sweep.workers
// is a single global pool, so one user pushing three repositories at once
// fills it and everybody else waits. That needs no malice, which is why the
// answer is a cap rather than a warning.
func newBuildQueue(nsLimit int) *buildQueue {
	return &buildQueue{
		names:       make(chan string, buildQueueCapacity),
		enqueued:    make(map[string]bool),
		namespaceOf: make(map[string]string),
		inNamespace: make(map[string]int),
		nsLimit:     nsLimit,
	}
}

// Why an enqueue was refused. The caller logs these differently because they
// mean different things: one is the ordinary case, one is a user being
// throttled, one is a machine with far more due targets than it can hold.
type enqueueResult int

const (
	queueAccepted enqueueResult = iota
	queueAlreadyInFlight
	queueNamespaceAtLimit
	queueFull
)

// add offers a target for building. It reports false when the target is
// already queued or being built — the ordinary case for a build that outlasts
// several sweep ticks, and the reason a ten-minute build does not accumulate
// sixty duplicate entries.
func (q *buildQueue) add(name, namespace string) enqueueResult {
	q.mu.Lock()
	if q.enqueued[name] {
		q.mu.Unlock()
		return queueAlreadyInFlight
	}
	// Admission control happens here rather than in the worker. A worker that
	// pulled a capped target would have to put it back, and with every queued
	// name belonging to one capped namespace the workers would spin handing
	// the same names to each other. Refusing admission costs nothing instead:
	// sweepOnce does not touch NextCheckAt for a target it could not queue, so
	// it stays due and the next tick offers it again — the same mechanism that
	// already handles a full queue.
	if q.nsLimit > 0 && q.inNamespace[namespace] >= q.nsLimit {
		q.mu.Unlock()
		return queueNamespaceAtLimit
	}
	q.enqueued[name] = true
	q.namespaceOf[name] = namespace
	q.inNamespace[namespace]++
	q.mu.Unlock()

	select {
	case q.names <- name:
		return queueAccepted
	default:
		// Full. Undo the claim so a later tick can try again; the target is
		// still due, so nothing is lost but this attempt.
		q.mu.Lock()
		q.release(name)
		q.mu.Unlock()
		return queueFull
	}
}

// release drops an in-flight claim. Caller holds q.mu.
func (q *buildQueue) release(name string) {
	if ns, ok := q.namespaceOf[name]; ok {
		q.inNamespace[ns]--
		if q.inNamespace[ns] <= 0 {
			// Deleted rather than left at zero: the map is keyed by namespace
			// and a long-lived watcher would otherwise accumulate one entry
			// per namespace that ever built anything.
			delete(q.inNamespace, ns)
		}
		delete(q.namespaceOf, name)
	}
	delete(q.enqueued, name)
}

// done releases a target so it can be queued again once it is due.
func (q *buildQueue) done(name string) {
	q.mu.Lock()
	q.release(name)
	q.mu.Unlock()
}

// inFlight is how many targets are queued or building, for logging.
func (q *buildQueue) inFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.enqueued)
}

// runBuildWorkers starts n workers that build whatever the sweep queues.
//
// Each worker calls the same sweepTarget the sequential loop used to call, and
// sweepTarget still takes that target's own lock — so the invariant that
// matters most is untouched: one target is never worked on by two goroutines.
// What changes is only that a build no longer blocks the detection of every
// other target.
func (w *Watcher) runBuildWorkers(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		go func(id int) {
			for {
				select {
				case <-ctx.Done():
					return
				case name := <-w.queue.names:
					w.sweepTargetSafe(ctx, name)
					w.queue.done(name)
				}
			}
		}(i)
	}
	w.log.Info("build workers started", zap.Int("workers", n))
}

// runStackWorker builds stacks, one at a time, on a goroutine of its own.
//
// Stacks stay strictly sequential among themselves: a stack deploys its
// projects in the order its manifest declares, and running two passes at once
// would interleave those orders. Giving them their own worker instead of the
// sweep goroutine is what stops a long stack from also delaying the detection
// of every standalone target — the problem this whole split exists to fix.
func (w *Watcher) runStackWorker(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stackWake:
				// Released in a defer: a panic inside a stack pass would
				// otherwise leave the claim set and no further pass would ever
				// be accepted — stacks would stop deploying silently, with the
				// sweep still logging that it woke them.
				func() {
					defer w.stackBusy.Store(false)
					w.sweepStacks(ctx)
				}()
			}
		}
	}()
}

// wakeStacks asks the stack worker for a pass, unless one is already running or
// pending. A stack pass looks at every stack, so a second request while one is
// in flight would only repeat work that is about to happen anyway.
func (w *Watcher) wakeStacks() {
	if !w.stackBusy.CompareAndSwap(false, true) {
		return
	}
	select {
	case w.stackWake <- struct{}{}:
	default:
		// Cannot happen with a buffered channel of one and the CAS above, but
		// leaving the claim set would wedge the worker forever.
		w.stackBusy.Store(false)
	}
}
