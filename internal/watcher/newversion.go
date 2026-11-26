package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// A deploy is not finished when the new container has been started: it is
// finished when the new version is shown to stay up. Upsert replaces the old
// container before the new one has proved anything — two containers cannot
// share a published host port, so "start the new one, then stop the old one"
// is not available — and so a version that crashes on startup used to leave
// the service down with last_build_status=success, until somebody noticed.
//
// Now the pipeline waits for the new version (waitForNewVersion) and, when it
// does not come up, puts the previous image back (RollbackComponent), records
// the deploy as failed with the crash output, and remembers the commit as
// rejected so polling does not deploy it again.

// stableWindow is how long a component with no health check must stay Running
// on the same container before it counts as up. Without a probe, Running is
// all there is to go on, and a process that exits on its first request or
// its first timer is Running for a moment first.
// A var so tests can shorten it.
var stableWindow = 10 * time.Second

// crashLogLines is how much of the failed container's output goes into the
// failure message: enough for a stack trace's head, short enough to read in
// a list.
const (
	crashLogLines    = 20
	crashLogMaxBytes = 2000
)

// errVersionDidNotStart carries why the new version was judged not to be up.
type errVersionDidNotStart struct{ reason string }

func (e errVersionDidNotStart) Error() string { return e.reason }

// waitForNewVersion waits until the component just deployed is up, or proves
// it is not. It differs from waitForComponentReady in what it treats as an
// answer: a dependency that exits is waited for (a restart may bring it
// back), but a new version that exits is the answer, because the old version
// is what should be serving in the meantime.
func (w *Watcher) waitForNewVersion(ctx context.Context, id, label string) error {
	deadline := time.Now().Add(serviceReadyTimeout)
	var firstContainer string
	var runningSince time.Time

	for {
		comp, err := w.client.GetComponent(ctx, id)
		if err != nil {
			return fmt.Errorf("check %s: %w", label, notTheDaemon(err))
		}
		if firstContainer == "" {
			firstContainer = comp.ContainerID
		}

		switch {
		case comp.Phase == "Exited" || comp.Phase == "Failed":
			return errVersionDidNotStart{fmt.Sprintf("it stopped (phase %s, exit code %d%s)",
				comp.Phase, comp.ExitCode, lastErrorSuffix(comp.LastError))}
		case comp.RestartExhausted:
			return errVersionDidNotStart{"it kept failing and qooq gave up restarting it" + lastErrorSuffix(comp.LastError)}
		case comp.RestartCount > 0:
			// Upsert zeroes the count, so any restart since is this version's.
			return errVersionDidNotStart{fmt.Sprintf("it exited (exit code %d) and was restarted", comp.ExitCode)}
		case comp.ContainerID != "" && comp.ContainerID != firstContainer:
			// No restart counted, yet the container is another one: something
			// replaced it while this check was running (a manual rollback or
			// deploy). Not proof the version is broken, but it was not seen
			// to run either, so it is not kept — and the message says which.
			return errVersionDidNotStart{"its container was replaced by something else before it had run long enough to be trusted"}
		}

		if comp.Phase == "Running" {
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			switch {
			case comp.HealthCheck == nil:
				if time.Since(runningSince) >= stableWindow {
					return nil
				}
			case !comp.HealthyAt.IsZero():
				return nil
			case comp.HealthCheck.Type != "exec" && time.Since(runningSince) > healthGraceTimeout:
				// Same reasoning as waitForComponentReady: an http probe can be
				// unable to see a healthy app at all, and rolling back a good
				// version over the probe's blind spot would be worse.
				w.log.Warn("new version is running but its http probe has not passed; keeping it",
					zap.String("component", label), zap.Duration("waited", healthGraceTimeout))
				return nil
			}
		} else {
			runningSince = time.Time{}
		}

		if time.Now().After(deadline) {
			if comp.Phase == "Running" {
				return errVersionDidNotStart{fmt.Sprintf("its health probe did not pass within %s%s",
					serviceReadyTimeout, lastErrorSuffix(comp.LastError))}
			}
			return errVersionDidNotStart{fmt.Sprintf("it did not reach Running within %s (phase %s%s)",
				serviceReadyTimeout, comp.Phase, lastErrorSuffix(comp.LastError))}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(servicePollInterval):
		}
	}
}

// verifyOrRollBack is the step after Upsert in both git pipelines. nil means
// the new version is up. Otherwise it has already done what can be done —
// rolled back when there is something to roll back to, marked headHash
// rejected — and the error says what happened, for LastBuildError.
func (w *Watcher) verifyOrRollBack(ctx context.Context, t *Target, comp *client.ComponentResponse, headHash string) error {
	return w.verifyOrRollBackPlan(ctx, t, comp, rollbackPlan{rejectKey: headHash, newLabel: shortSHA(headHash)})
}

// rollbackPlan is what differs between the git and the image paths.
type rollbackPlan struct {
	// rejectKey is what polling must not deploy again: the commit, or the
	// image digest.
	rejectKey string
	// newLabel names the new version in the message.
	newLabel string
	// prevRef is the image to go back to when the component's PreviousImage
	// cannot be: an image service on a moving tag (":latest") deploys the
	// same ref every time, so "the previous image" is the broken one under
	// its old name. The old one is still on the machine by its digest
	// (repo@sha256:…). Empty for git builds, whose tags are per commit.
	prevRef   string
	prevLabel string
}

func (w *Watcher) verifyOrRollBackPlan(ctx context.Context, t *Target, comp *client.ComponentResponse, plan rollbackPlan) error {
	label := w.componentRefFor(t).String()
	err := w.waitForNewVersion(ctx, comp.ID, label)
	if err == nil {
		return nil
	}
	var notUp errVersionDidNotStart
	if !errors.As(err, &notUp) {
		// We could not find out (qooqd unreachable, ctx cancelled). Rolling
		// back on no evidence would undo a deploy that may well be fine.
		return err
	}

	// Read the output now: the rollback replaces this container.
	logs := w.crashLogTail(ctx, comp.ID)
	w.markRejected(t, plan.rejectKey)

	newVersion := plan.newLabel
	prev := comp.PreviousImage
	// Named by its tag (the commit, for a git build), not the image: the
	// image name carries the qualified namespace, which the external manager
	// rewrites for the user into a name that does not exist.
	prevVersion := versionOf(prev)
	var rollBack func() error
	switch {
	case prev != "" && prev != comp.Image:
		rollBack = func() error {
			_, err := w.client.RollbackComponent(ctx, comp.ID)
			return err
		}
	case plan.prevRef != "" && plan.prevRef != comp.Image:
		prev, prevVersion = plan.prevRef, plan.prevLabel
		rollBack = func() error {
			req, err := w.buildDeployRequest(t, plan.prevRef, deploySpecFields{})
			if err != nil {
				return err
			}
			_, err = w.client.Upsert(ctx, req)
			return err
		}
	default:
		return fmt.Errorf("the new version (%s) did not start: %s; there is no earlier version to go back to%s",
			newVersion, notUp.reason, logs)
	}

	w.log.Warn("new version did not start; rolling back",
		zap.String("component", label), zap.String("reason", notUp.reason), zap.String("back_to", prev))
	if rbErr := rollBack(); rbErr != nil {
		return fmt.Errorf("the new version (%s) did not start: %s; rolling back to %s also failed: %v%s",
			newVersion, notUp.reason, prevVersion, notTheDaemon(rbErr), logs)
	}
	if backErr := w.waitForNewVersion(ctx, comp.ID, label); backErr != nil {
		return fmt.Errorf("the new version (%s) did not start: %s; it was rolled back to %s, but that is not up either: %v%s",
			newVersion, notUp.reason, prevVersion, backErr, logs)
	}
	return fmt.Errorf("the new version (%s) did not start: %s; the previous version (%s) was put back and is running%s",
		newVersion, notUp.reason, prevVersion, logs)
}

// imageRollbackPlan is the plan for an image deploy: digests name versions,
// and the previous digest is reachable as repo@digest when the tag moved.
func imageRollbackPlan(newRef, newDigest, prevDigest string) rollbackPlan {
	plan := rollbackPlan{rejectKey: newDigest, newLabel: shortDigest(newDigest)}
	if plan.newLabel == "" {
		plan.newLabel = newRef
	}
	if prevDigest != "" && prevDigest != newDigest {
		plan.prevRef = imageRepo0(newRef) + "@" + prevDigest
		plan.prevLabel = shortDigest(prevDigest)
	}
	return plan
}

// shortDigest is "sha256:" and the first seven hex digits.
func shortDigest(d string) string {
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "sha256:") {
		d = "sha256:" + d
	}
	if len(d) > len("sha256:")+7 {
		d = d[:len("sha256:")+7]
	}
	return d
}

// imageRepo0 is a reference without its tag or digest:
// "reg:5000/team/app:1.2" -> "reg:5000/team/app". (A port's colon comes before
// the last slash; a tag's comes after it.)
func imageRepo0(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// versionOf is the tag of an image reference ("api:973bee4" -> "973bee4"),
// or the reference unchanged when it has none (a process artifact path).
func versionOf(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[i+1:]
	}
	return ref
}

// markRejected stops polling from redeploying headHash. ForceRebuild is
// cleared with it: left set, the next sweep would deploy the same commit
// again and take the service down again.
func (w *Watcher) markRejected(t *Target, headHash string) {
	t.RejectedCommit = headHash
	t.ForceRebuild = false
	if w.db == nil {
		return
	}
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"rejected_commit": headHash,
		"force_rebuild":   false,
	}).Error; err != nil {
		w.log.Warn("failed to record rejected commit", zap.String("target", t.Name), zap.Error(err))
	}
}

// crashLogTail returns the last lines of the component's output, formatted to
// append to an error, or "" when there is none to show.
func (w *Watcher) crashLogTail(ctx context.Context, id string) string {
	rc, err := w.client.Logs(ctx, id, false, crashLogLines, false)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 64<<10))
	out := strings.TrimSpace(string(b))
	if len(out) > crashLogMaxBytes {
		out = "…" + out[len(out)-crashLogMaxBytes:]
	}
	if out == "" {
		return ""
	}
	return "\n--- last output of the new version ---\n" + out
}

func lastErrorSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ", last error: " + s
}
