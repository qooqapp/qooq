package watcher

import (
	"context"
	"net/http"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// Both unchanged branches used to decide from the source alone, so a component
// that vanished from qooqd was never restored and the target went on reporting
// success. These pin down the three answers componentIsGone has to tell apart,
// because the dangerous one is not "gone" — it is "I could not ask".

func healTarget(name string) *Target {
	return &Target{
		Name:            name,
		ComponentName:   name,
		RepoURL:         "file:///repo",
		LastCommit:      "abc1234",
		LastBuildStatus: BuildStatusSuccess,
	}
}

// TestAMissingComponentIsNoticed is the gap itself: qooqd has nothing under
// this (namespace, name), so the deployment has to be restored.
func TestAMissingComponentIsNoticed(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{}, http.StatusOK)

	if !w.componentIsGone(context.Background(), healTarget("web")) {
		t.Fatal("a component absent from qooqd must be reported gone, or nothing ever redeploys it")
	}
}

// TestAPresentComponentIsLeftAlone keeps this from rebuilding on every sweep,
// which would mean a clone and a build per poll interval for every target.
func TestAPresentComponentIsLeftAlone(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "web", Namespace: "default", Phase: "Running"},
	}, http.StatusOK)

	if w.componentIsGone(context.Background(), healTarget("web")) {
		t.Fatal("a component that is right there was reported gone: every sweep would rebuild it")
	}
}

// TestAStoppedComponentIsNotGone: an operator who ran `qooq component stop`
// decided that. The component is present with a stopped desired state, and
// redeploying it would start it again — overruling the person, not healing
// anything.
func TestAStoppedComponentIsNotGone(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "web", Namespace: "default", Phase: "Exited", DesiredState: "Stopped"},
	}, http.StatusOK)

	if w.componentIsGone(context.Background(), healTarget("web")) {
		t.Fatal("a deliberately stopped component was treated as missing: this would restart what someone stopped on purpose")
	}
}

// TestAnUnreachableDaemonIsNotAnAnswer is the one that matters most. Reading a
// failed lookup as "gone" would rebuild every target on this watcher the
// moment qooqd restarts — turning a blip into a stampede of clones and builds.
func TestAnUnreachableDaemonIsNotAnAnswer(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, nil, http.StatusInternalServerError)

	if w.componentIsGone(context.Background(), healTarget("web")) {
		t.Fatal("an unreachable daemon was read as a missing component: a qooqd restart would rebuild everything at once")
	}
}

// TestADetectFailureIsNotDrift: reaching an unchanged branch while the last
// build is recorded as failed means the failure came from resolving the remote,
// not from deploying. Nothing was deployed on that pass, so absence here says
// nothing — and clearStaleFailure is what should run instead.
func TestADetectFailureIsNotDrift(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{}, http.StatusOK)

	tg := healTarget("web")
	tg.LastBuildStatus = BuildStatusFailed
	if w.componentIsGone(context.Background(), tg) {
		t.Fatal("a target whose last build failed in the detect phase was treated as drift")
	}
}

// TestTheNamespaceIsPartOfTheMatch: two users each running a component
// called "api" are two components. Matching on name alone would let one
// user's component stand in for another's and leave a real absence
// unnoticed — the same rule findComponent already carries, pinned here because
// this is now a second caller depending on it.
func TestTheNamespaceIsPartOfTheMatch(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "api", Namespace: "bob--prod", Phase: "Running"},
	}, http.StatusOK)

	// This target's component resolves to namespace "default", not bob--prod.
	if !w.componentIsGone(context.Background(), healTarget("api")) {
		t.Fatal("someone else's component in another namespace was accepted as ours")
	}
}

// TestNoClientIsNotAnAnswer: a Watcher with no qooqd client has nobody to ask,
// and the honest answer is the same as an unreachable daemon. Found by the
// existing stack tests, which build a Watcher without one — in production the
// panic would have happened inside the sweep goroutine and taken the process
// down with it.
func TestNoClientIsNotAnAnswer(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = nil

	if w.componentIsGone(context.Background(), healTarget("web")) {
		t.Fatal("a Watcher with no client claimed the component was gone")
	}
}

// TestAStacksProjectHealsThroughItsStack: a project belongs to a stack, which
// deploys its projects in the order its manifest declares. That ordering is
// why dueTargets excludes stack-owned rows; a project that rebuilt itself the
// moment its component went missing would break it from the other side.
func TestAStacksProjectHealsThroughItsStack(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{}, http.StatusOK)

	tg := healTarget("api")
	tg.StackName = "acme"
	if w.componentIsGone(context.Background(), tg) {
		t.Fatal("a stack's project decided to redeploy itself, out of the order its stack deploys in")
	}
}
