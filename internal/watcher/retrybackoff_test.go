package watcher

import (
	"testing"
	"time"
)

func TestRetryBackoffDoublesUpToAnHour(t *testing.T) {
	want := map[int]time.Duration{0: 0, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 6: 32 * time.Minute, 7: time.Hour, 10: time.Hour, 99: time.Hour}
	for n, d := range want {
		if got := retryBackoff(n); got != d {
			t.Errorf("retryBackoff(%d) = %v, want %v", n, got, d)
		}
	}
}

func TestOnlyTheRevisionThatJustFailedWaits(t *testing.T) {
	now := time.Now()
	failed := &Target{LastBuildStatus: BuildStatusFailed, LastBuildRevision: "abc", ConsecutiveFailures: 3, LastBuildAt: now.Add(-time.Minute)}

	if w := retryWait(failed, "abc", now); w != 3*time.Minute {
		t.Errorf("the failed revision waits %v, want 3m (4m backoff, 1m already passed)", w)
	}
	// The fix somebody just pushed is built at once.
	if w := retryWait(failed, "def", now); w != 0 {
		t.Errorf("a new revision waits %v, want 0", w)
	}
	// Once the backoff has passed, it is retried.
	if w := retryWait(failed, "abc", now.Add(10*time.Minute)); w != 0 {
		t.Errorf("after the backoff it still waits %v", w)
	}
	// A failure that did not count (qooq's own dependency down) does not wait.
	notCounted := *failed
	notCounted.ConsecutiveFailures = 0
	if w := retryWait(&notCounted, "abc", now); w != 0 {
		t.Errorf("an uncounted failure waits %v, want 0", w)
	}
	ok := *failed
	ok.LastBuildStatus = BuildStatusSuccess
	if w := retryWait(&ok, "abc", now); w != 0 {
		t.Errorf("a success waits %v", w)
	}
}
