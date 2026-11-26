package watcher

import (
	"errors"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// A target that fails every time eventually stops being retried.
//
// Without this a repository that can never build is retried once per poll
// interval, forever. Each attempt clones, tars, and occupies one of a small
// fixed pool of build workers for however long the failure takes to happen — and
// for a build that fails by timing out, that is the whole timeout, plus (under
// the classic builder) a build step container that keeps running after the
// timeout releases the worker. A handful of such repositories is sustained load
// nobody asked for, and on a shared watcher it is load taken from other users.
//
// The shape deliberately mirrors two things this codebase already settled:
//
//   - core's RestartExhausted: a component that has crash-looped past a
//     threshold stops being restarted, and says so, rather than retrying until
//     someone notices.
//   - the stack layer's pause: N consecutive failures sets a reason that names
//     the command to run after fixing it.
//
// Resuming is `qooq watch trigger`, not a new verb. It already means "check this
// now", which is exactly what someone does after fixing the repository, and a
// second command for the same intent is a second thing to document and forget.

// errNotTheRepository marks a failure the repository had no part in.
//
// The pause below says "fix the repository" and stops the target being retried
// until a person comes back to it. That is right for a repository that cannot
// build. It is wrong twice over for one of qooq's own dependencies being
// unreachable: the sentence is false, and it is told to someone whose
// repository is fine — and on a shared watcher it is told to everyone at once,
// so a few minutes of external-manager outage becomes every user's deployments
// switched off until each of them clicks a button.
//
// It also disables the thing that already cleans up after this. clearStaleFailure
// exists to retire exactly this kind of transient detect failure on the next
// successful check, but a paused target is excluded from dueTargets — so that
// check never happens, and the net is taken away at the moment it would have
// caught someone.
//
// Observed rather than imagined: a laptop asleep overnight produced ten of
// these over five hours, each one a timeout talking to a external manager whose
// database was in a sleeping VM. The target was paused, told to fix a
// repository nobody had touched, while its container was running the whole
// time.
//
// Such a failure is still recorded and still shown — it did stop a deploy, and
// hiding it would be a different lie. It just does not count toward giving up.
type errNotTheRepository struct{ err error }

func (e errNotTheRepository) Error() string { return e.err.Error() }
func (e errNotTheRepository) Unwrap() error { return e.err }

// notTheRepository marks err, keeping its message and its chain.
func notTheRepository(err error) error {
	if err == nil {
		return nil
	}
	return errNotTheRepository{err: err}
}

// isNotTheRepository reports whether err was marked anywhere in its chain.
//
// errors.As rather than a type assertion, because the mark is applied where the
// cause is known — down in the external-manager client — and wrapped several times
// on the way up through the git helpers and the pipeline.
func isNotTheRepository(err error) bool {
	var marked errNotTheRepository
	return errors.As(err, &marked)
}

// notTheDaemon marks a failure that happened because qooqd never answered.
//
// The same situation as an unreachable external manager, one layer down: the
// laptop sleeps, docker goes away with it, and every target that comes due
// fails at the first call to the daemon — through no fault of any repository.
// Left counting, five hours of that is enough to pause a project and tell its
// owner to fix code nobody has touched.
//
// Deliberately NARROWER than isControlUnreachable, which also treats 5xx as
// qooq's own fault. qooqd answers 500 for a build that failed because of the
// caller's Dockerfile — the error travels back through the build stream and
// out via writeComponentErr — so counting 5xx here would stop counting exactly
// the failures this mechanism exists for. What is left is the transport layer:
// connection refused, DNS, a timeout, a daemon that is not listening. pkg/client
// returns the raw error rather than an *APIError for those, so StatusCode is 0.
//
// The one 5xx that IS counted out is 503: qooqd answers it when its own
// container runtime does not answer (it pings before calling anything a 500),
// which no repository can cause or fix. Found by stopping docker under a
// running qooqd: every build came back 500, and A3's retry backoff alone
// carried a project to the pause in a few hours.
//
// Applied at the call site, never in bulk: away from the client call, an error
// with no status code is usually something qooq worked out for itself (a
// manifest that will not parse, a path that escapes the repo), and those are
// the repository's.
func notTheDaemon(err error) error {
	if err == nil {
		return nil
	}
	if code := client.StatusCode(err); code != 0 && code != http.StatusServiceUnavailable {
		return err
	}
	return notTheRepository(err)
}

// maxConsecutiveBuildFailures is how many failures in a row stop a target.
//
// The same value the stack layer chose, for the same reason: generous enough to
// absorb a registry hiccup or a DNS blip without giving up, small enough that a
// genuinely broken repository stops consuming build capacity the same day.
const maxConsecutiveBuildFailures = 10

// noteBuildFailure counts a failure and gives up once there have been too many.
//
// Returns the pause reason when this failure is the one that crosses the
// threshold, so the caller can log it once rather than on every subsequent
// sweep.
func (w *Watcher) noteBuildFailure(t *Target) string {
	n := t.ConsecutiveFailures + 1
	t.ConsecutiveFailures = n

	updates := map[string]any{"consecutive_failures": n}
	reason := ""
	if n >= maxConsecutiveBuildFailures && t.PausedReason == "" {
		reason = fmt.Sprintf(
			"build failed %d times in a row; fix the repository and run `qooq watch trigger %s` to resume",
			n, t.Name)
		t.PausedReason = reason
		updates["paused_reason"] = reason
	}

	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(updates).Error; err != nil {
		w.log.Warn("could not record build failure count",
			zap.String("target", t.Name), zap.Error(err))
	}
	return reason
}

// clearBuildFailures resets the counter after a success.
//
// Also clears any pause: a target that has just built successfully is by
// definition no longer the broken one this mechanism exists to stop, and leaving
// the reason set would make a working target look paused.
func (w *Watcher) clearBuildFailures(t *Target) {
	if t.ConsecutiveFailures == 0 && t.PausedReason == "" {
		return
	}
	t.ConsecutiveFailures = 0
	t.PausedReason = ""
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).
		Updates(map[string]any{"consecutive_failures": 0, "paused_reason": ""}).Error; err != nil {
		w.log.Warn("could not clear build failure count",
			zap.String("target", t.Name), zap.Error(err))
	}
}
