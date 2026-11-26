package watcher

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/controlclient"
)

// The give-up mechanism tells the reader to fix their repository and stops
// retrying until somebody comes back. These tests pin which failures are
// allowed to lead there.
//
// Both directions matter, and the second is why the first cannot simply stop
// counting everything: a repository that genuinely cannot build must still
// reach the threshold, or the mechanism is gone.

func TestAFailureQooqCausedItselfDoesNotCountTowardGivingUp(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", ConsecutiveFailures: maxConsecutiveBuildFailures - 1}
	saveTarget(t, w, tg)

	// What an unreachable external manager looks like by the time it reaches
	// recordFailure: marked at the client, then wrapped twice on the way up
	// through the git helpers and the pipeline.
	cause := fmt.Errorf("resolve remote head: %w",
		notTheRepository(errors.New(`control POST /internal/v1/clone-token: context deadline exceeded`)))
	w.recordFailure(tg, cause)

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusFailed {
		t.Errorf("status = %q, want failed: the deploy did stop, and hiding that would be its own lie", got.LastBuildStatus)
	}
	if got.ConsecutiveFailures != maxConsecutiveBuildFailures-1 {
		t.Errorf("consecutive_failures = %d, want it left at %d: a control plane that did not answer is no evidence about this repository",
			got.ConsecutiveFailures, maxConsecutiveBuildFailures-1)
	}
	if got.PausedReason != "" {
		t.Errorf("paused with %q — the repository was never touched", got.PausedReason)
	}
}

func TestARepositoryThatCannotBuildStillGetsPaused(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", ConsecutiveFailures: maxConsecutiveBuildFailures - 1}
	saveTarget(t, w, tg)

	w.recordFailure(tg, errors.New("docker build failed: Dockerfile not found"))

	got := getTargetByID(t, w.db, tg.ID)
	if got.ConsecutiveFailures != maxConsecutiveBuildFailures {
		t.Errorf("consecutive_failures = %d, want %d", got.ConsecutiveFailures, maxConsecutiveBuildFailures)
	}
	if got.PausedReason == "" {
		t.Error("a repository that fails every time was not paused; the mechanism is gone")
	}
}

// Which external-manager answers mean "it did not answer" rather than "no".
//
// Pinned on the status, not the wording: matching on message text is a mistake
// this codebase has corrected twice, and here getting it wrong in one direction
// pauses projects nobody broke.
func TestOnlyAControlPlaneThatDidNotAnswerCountsAsUnreachable(t *testing.T) {
	apiErr := func(code int) error {
		return fmt.Errorf("asking for a credential: %w",
			&controlclient.APIError{Method: "POST", Path: "/internal/v1/clone-token", StatusCode: code, Message: "x"})
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"a timeout, with no status at all", errors.New("context deadline exceeded"), true},
		{"a 200 the client could not use", fmt.Errorf("no token in the response"), true},
		{"the control plane is broken", apiErr(http.StatusInternalServerError), true},
		{"the control plane is down for maintenance", apiErr(http.StatusServiceUnavailable), true},
		{"the app is not installed on that repository", apiErr(http.StatusNotFound), false},
		{"this account may not have it", apiErr(http.StatusForbidden), false},
		{"the watcher's token is wrong", apiErr(http.StatusUnauthorized), false},
		{"the watcher sent something unreadable", apiErr(http.StatusBadRequest), false},
	}
	for _, tc := range cases {
		if got := isControlUnreachable(tc.err); got != tc.want {
			t.Errorf("%s: isControlUnreachable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A successful re-check ends the row, so the count of what it disproved goes
// with the status.
//
// clearStaleFailure is only reached when the remote has been re-checked
// successfully and reports nothing new, which (see its own comment) means the
// failure it is clearing was a transient detect blip. Keeping the number after
// retiring the status left nine disproved hiccups able to pause the target on
// the tenth — a pause whose premise, "failed every time", the clear itself had
// just contradicted.
func TestASuccessfulRecheckAlsoRetiresTheFailuresItDisproved(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{
		Name: "web", ComponentName: "web", LastCommit: "abc123",
		LastBuildStatus: BuildStatusFailed, LastBuildError: "lookup example.test: no such host",
		ConsecutiveFailures: maxConsecutiveBuildFailures - 1,
	}
	saveTarget(t, w, tg)

	w.clearStaleFailure(tg)

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusSuccess {
		t.Errorf("status = %q, want success", got.LastBuildStatus)
	}
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d after a successful re-check, want 0: the row was broken", got.ConsecutiveFailures)
	}
}

// The same rule one layer down: qooqd not answering is not the repository's
// fault either.
//
// Observed in the same way as the external-manager case — a laptop asleep takes
// docker with it, so every target that comes due fails at its first call to
// the daemon. Left counting, a few hours of that pauses projects and tells
// their owners to fix code nobody has touched.
func TestADaemonThatNeverAnsweredDoesNotCountTowardGivingUp(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", ConsecutiveFailures: maxConsecutiveBuildFailures - 1}
	saveTarget(t, w, tg)

	// What connection-refused looks like by the time it reaches recordFailure.
	cause := fmt.Errorf("upsert component (built image web:abc1234, id=sha256:x): %w",
		notTheDaemon(errors.New(`Post "http://127.0.0.1:7777/v1/components": dial tcp 127.0.0.1:7777: connect: connection refused`)))
	w.recordFailure(tg, cause)

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastBuildStatus != BuildStatusFailed {
		t.Errorf("status = %q, want failed: the deploy did stop", got.LastBuildStatus)
	}
	if got.ConsecutiveFailures != maxConsecutiveBuildFailures-1 {
		t.Errorf("consecutive_failures = %d, want it left at %d: a daemon that did not answer is no evidence about this repository",
			got.ConsecutiveFailures, maxConsecutiveBuildFailures-1)
	}
	if got.PausedReason != "" {
		t.Errorf("paused with %q — qooqd was down, the repository was never read", got.PausedReason)
	}
}

// And the direction that keeps the mechanism alive: a build qooqd rejected is
// the repository's.
//
// This is why the daemon rule is narrower than the external-manager one. qooqd
// answers 500 for a build that failed on the caller's Dockerfile — the error
// comes back through the build stream and out via writeComponentErr — so a
// rule that excused every 5xx would excuse exactly the failures worth counting,
// and no repository would ever be paused again.
func TestABuildTheDaemonRejectedStillCounts(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool // want: counted as the repository's fault
	}{
		{
			name: "a Dockerfile that does not build (qooqd answers 500)",
			err:  &client.APIError{StatusCode: http.StatusInternalServerError, Message: "internal error"},
			want: true,
		},
		{
			name: "a build that ran out of time (504)",
			err:  &client.APIError{StatusCode: http.StatusGatewayTimeout, Message: "build timed out"},
			want: true,
		},
		{
			name: "a host port already taken (409)",
			err:  &client.APIError{StatusCode: http.StatusConflict, Message: "port is already allocated"},
			want: true,
		},
		{
			// qooqd is up, its docker is not: no repository caused it.
			name: "qooqd's container runtime is down (503)",
			err:  &client.APIError{StatusCode: http.StatusServiceUnavailable, Message: "the container runtime is not available"},
			want: false,
		},
		{
			name: "no daemon at the other end",
			err:  errors.New("dial tcp 127.0.0.1:7777: connect: connection refused"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newStatusWatcher(t)
			tg := &Target{Name: "web", ComponentName: "web"}
			saveTarget(t, w, tg)

			w.recordFailure(tg, fmt.Errorf("build image: %w", notTheDaemon(tc.err)))

			got := getTargetByID(t, w.db, tg.ID)
			counted := got.ConsecutiveFailures == 1
			if counted != tc.want {
				t.Errorf("counted = %v, want %v (consecutive_failures = %d)",
					counted, tc.want, got.ConsecutiveFailures)
			}
		})
	}
}
