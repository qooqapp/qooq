package watcher

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"go.uber.org/zap"
)

// sweepInterval is how often the sweep loop checks for due targets. It is
// intentionally decoupled from any single target's PollIntervalSeconds: the
// sweep just polls "what's due" every tick, and each target's own interval
// controls how often it individually becomes due again.
const sweepInterval = 10 * time.Second

// runSweepLoop polls for due targets every sweepInterval and hands them to the
// build workers. Stops when ctx is cancelled.
//
// The loop itself does no work beyond the query: it used to build each due
// target in turn, which meant one ten-minute build stopped every other target
// from even being looked at — including one added while it ran, which sat with
// no next_check_at at all and looked broken. Webhooks lost their point too,
// since "check now" only moved a target to the back of the same line.
func (w *Watcher) runSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("sweep loop stopped")
			return
		case <-ticker.C:
			w.sweepOnce(ctx)
		}
	}
}

func (w *Watcher) sweepOnce(ctx context.Context) {
	due, err := dueTargets(w.db, time.Now(), w.instanceID)
	if err != nil {
		w.log.Error("sweep: list due targets failed", zap.Error(err))
		return
	}
	queued, held, full := 0, 0, 0
	for i := range due {
		// A target already queued or building is skipped rather than queued
		// again: it stays due for as long as its build runs, so every tick
		// would otherwise pile up another copy of the same work.
		switch w.queue.add(due[i].Name, buildGroupOf(&due[i])) {
		case queueAccepted:
			queued++
		case queueNamespaceAtLimit:
			held++
		case queueFull:
			full++
		}
	}
	if queued > 0 || held > 0 {
		w.log.Debug("sweep: queued targets",
			zap.Int("queued", queued), zap.Int("held_by_namespace_limit", held),
			zap.Int("in_flight", w.queue.inFlight()))
	}
	if full > 0 {
		// Not debug: the pending set is sized far above any plausible number
		// of targets on one machine, so reaching it means something is wrong
		// rather than busy.
		w.log.Warn("sweep: build queue full, targets deferred to the next tick",
			zap.Int("deferred", full))
	}

	// Stacks run on a worker of their own, and strictly one pass at a time: a
	// stack deploys its projects in the order its manifest declares, so two
	// passes at once would interleave those orders. They are kept off the
	// build workers for the same reason dueTargets excludes stack-owned
	// targets — nothing may rebuild one of a stack's projects underneath it.
	w.wakeStacks()
}

// sweepTarget processes one due target under its per-target lock. The row is
// re-read inside the lock rather than trusting the dueTargets batch snapshot:
// a POST /v1/targets/{name}/deploy (M20b) may have updated ImageRef/
// LastImageDigest between the batch read and lock acquisition, and acting on
// the stale snapshot would redeploy the pre-/deploy image right over it.
func (w *Watcher) sweepTarget(ctx context.Context, name string) {
	unlock := w.lockTarget(name)
	defer unlock()

	// The mutex above only speaks to the other goroutines of this process. The
	// claim speaks to the other watchers sharing this database; without it two
	// of them build the same target and both report success (claim.go).
	release, ok := w.claim(name)
	if !ok {
		w.log.Debug("sweep: another watcher is working on this target", zap.String("target", name))
		return
	}
	defer release()

	t, err := getTargetByName(w.db, name)
	if err != nil {
		// Deleted between the batch read and now — nothing to do.
		w.log.Debug("sweep: target vanished before processing", zap.String("target", name), zap.Error(err))
		return
	}
	// dueBefore is this target's NextCheckAt as read before the (possibly
	// minutes-long) build. rescheduleTarget uses it as the compare-and-swap
	// guard so a webhook/trigger that moved NextCheckAt to "now" mid-build
	// (risk #7) is not overwritten by now+interval.
	dueBefore := t.NextCheckAt
	// M18: AuthCredential/WebhookSecret may be AES-GCM-encrypted at rest;
	// decrypt in place before the pipeline uses t.AuthCredential to clone.
	// A failure here (secret.key changed since this target was created)
	// is recorded as a build failure like any other pipeline error,
	// rather than panicking or silently cloning with garbage credentials.
	if err := w.decryptTarget(t); err != nil {
		w.log.Error("sweep: decrypt credentials failed", zap.String("target", t.Name), zap.Error(err))
		w.recordFailure(t, err)
	} else if t.DeployMode == DeployModeImage {
		w.checkAndMaybeDeployImage(ctx, t)
	} else {
		// Source and upload both: checkAndMaybeBuild asks the target for its
		// current revision, which for an upload is the last archive stored.
		w.checkAndMaybeBuild(ctx, t)
	}
	interval := time.Duration(t.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultPollIntervalSeconds * time.Second
	}
	applied, err := rescheduleTarget(w.db, t.ID, interval, dueBefore)
	if err != nil {
		w.log.Error("sweep: reschedule failed", zap.String("target", t.Name), zap.Error(err))
	} else if !applied {
		// A webhook/trigger poked this target while it was building; its
		// "check now" NextCheckAt is preserved for the next sweep tick.
		w.log.Debug("sweep: reschedule skipped, poke pending", zap.String("target", t.Name))
	}
}

// sweepTargetSafe runs sweepTarget for one target behind a recover boundary.
//
// Each build worker (runBuildWorkers, queue.go) is a long-lived goroutine that
// pulls one target at a time off the shared queue; an unrecovered panic while
// processing any one of them would kill that goroutine and, with nothing else
// on the stack to catch it, the whole qooq-watcher process — taking every
// other target's build/deploy pipeline down over a bug in one repo's.
func (w *Watcher) sweepTargetSafe(ctx context.Context, name string) {
	w.recoverTargetPanic(name, func() { w.sweepTarget(ctx, name) })
}

// recoverTargetPanic runs fn and, if it panics, records the outcome against
// the named target instead of letting the panic escape: an ERROR log (with a
// full stack trace) for whoever is watching daemon logs, and a
// LastBuildStatus/LastBuildError update — the same fields an ordinary build
// failure sets — so the fact that it happened is visible from `qooq watch
// get` and not just a log line nobody happened to be watching for.
func (w *Watcher) recoverTargetPanic(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			w.recordTargetPanic(name, r, debug.Stack())
		}
	}()
	fn()
}

// recordTargetPanic is recoverTargetPanic's log+persist half, split out so
// tests can drive it directly without needing to trigger a real panic deep in
// the build/deploy pipeline.
func (w *Watcher) recordTargetPanic(name string, r any, stack []byte) {
	w.log.Error("build worker: panic recovered, continuing",
		zap.String("target", name), zap.Any("panic", r), zap.ByteString("stack", stack))
	t, err := getTargetByName(w.db, name)
	if err != nil {
		// Deleted or renamed since the panic — nothing left to record it on;
		// the ERROR log above is the only trace, same as sweepTarget's own
		// "vanished before processing" case.
		return
	}
	w.recordFailure(t, fmt.Errorf("internal error: panic recovered: %v", r))
}

// buildGroupOf is what the concurrent-build cap counts this target under: its
// build group when the operator set one (the external manager uses the account),
// otherwise its namespace.
func buildGroupOf(t *Target) string {
	if t.BuildGroup != "" {
		return "group:" + t.BuildGroup
	}
	return namespaceOf(t)
}
