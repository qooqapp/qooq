package watcher

import "time"

// Retrying a revision that failed to build.
//
// A failed build's revision used to be rebuilt at every poll — once a minute —
// until ten failures paused the target. A Dockerfile typo fails the same way
// every time, so that was ten builds paid for one mistake; a build that never
// finishes held a build worker for the whole timeout each round, about five
// hours per bad commit at a 30-minute timeout.
//
// Retries still happen, because some failures do go away on their own (a
// package registry that was briefly down), but each waits twice as long as the
// one before: 1, 2, 4, 8 … minutes, up to an hour. Failures qooq's own
// dependencies caused do not count (errNotTheRepository), so those keep
// retrying at the poll interval.
const (
	retryBackoffBase = time.Minute
	retryBackoffMax  = time.Hour
)

// retryBackoff is how long to wait after the nth consecutive failure.
func retryBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := retryBackoffBase
	for i := 1; i < n && d < retryBackoffMax; i++ {
		d *= 2
	}
	if d > retryBackoffMax {
		d = retryBackoffMax
	}
	return d
}

// retryWait is how much longer revision has to wait before it is built again:
// zero unless it is the revision whose build just failed.
func retryWait(t *Target, revision string, now time.Time) time.Duration {
	if t.LastBuildStatus != BuildStatusFailed || t.LastBuildRevision != revision || t.ConsecutiveFailures <= 0 {
		return 0
	}
	if wait := t.LastBuildAt.Add(retryBackoff(t.ConsecutiveFailures)).Sub(now); wait > 0 {
		return wait
	}
	return 0
}
