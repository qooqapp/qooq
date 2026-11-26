package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// This file extends service_test.go (reusing its fakeStore/fakeRuntime and the
// newTestService/mustDeploy helpers) to close the P1-6 gaps not already covered
// there: RestartPolicy semantics, the full reconcile-drift path (missing
// container), and Rollback.

// ---------------------------------------------------------------------------
// RestartPolicy semantics (restart.go)
// ---------------------------------------------------------------------------

// TestShouldRestart_PolicySemantics pins the policy decision table directly:
// "no"/"" never restart; "on-failure" restarts on a non-zero crash exit or on a
// drift (no exit code to inspect); "always" restarts unconditionally.
func TestShouldRestart_PolicySemantics(t *testing.T) {
	cases := []struct {
		policy   string
		reason   string
		exitCode int
		want     bool
	}{
		{RestartPolicyNo, RestartReasonCrash, 1, false},
		{"", RestartReasonCrash, 1, false}, // empty treated as "no"
		{RestartPolicyOnFailure, RestartReasonCrash, 0, false},
		{RestartPolicyOnFailure, RestartReasonCrash, 1, true},
		{RestartPolicyOnFailure, RestartReasonDrift, 0, true}, // no exit code -> treat as failure
		{RestartPolicyAlways, RestartReasonCrash, 0, true},
		{RestartPolicyAlways, RestartReasonDrift, 0, true},
	}
	for _, c := range cases {
		if got := shouldRestart(c.policy, c.reason, c.exitCode); got != c.want {
			t.Errorf("shouldRestart(%q, %q, %d) = %v, want %v", c.policy, c.reason, c.exitCode, got, c.want)
		}
	}
}

// TestEvaluateRestart_PolicyNoDoesNotRestart confirms end-to-end that a crash
// under an explicit "no" policy schedules nothing — the way to say "if it
// dies, leave it".
func TestEvaluateRestart_PolicyNoDoesNotRestart(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp, err := s.Deploy(context.Background(), ComponentSpec{Name: "web", Image: "img:latest", RestartPolicy: RestartPolicyNo})
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.ContainerID = "" // container down
	fresh.Status.LastExitCode = 1
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)

	got, _ := st.GetComponent(comp.ID)
	if !got.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil = %v, want zero (policy 'no' must not schedule a restart)", got.Status.BackoffUntil)
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0", got.Status.RestartCount)
	}
	if rt.callCount("CreateContainer") != 1 { // only the original Deploy
		t.Errorf("CreateContainer called %d times, want 1", rt.callCount("CreateContainer"))
	}
}

// TestEvaluateRestart_OnFailureOnlyRestartsNonZeroExit confirms the on-failure
// policy distinguishes a clean exit (0) from a crash (non-zero) for the crash
// reason: only the latter schedules a restart.
func TestEvaluateRestart_OnFailureOnlyRestartsNonZeroExit(t *testing.T) {
	t.Run("clean exit 0 -> no restart", func(t *testing.T) {
		rt := newFakeRuntime()
		st := newFakeStore()
		s := newTestService(rt, st)

		comp := mustDeploy(t, s, "web")
		fresh, _ := st.GetComponent(comp.ID)
		fresh.Spec.RestartPolicy = RestartPolicyOnFailure
		fresh.Status.ContainerID = ""
		fresh.Status.LastExitCode = 0
		if err := st.SaveComponent(fresh); err != nil {
			t.Fatal(err)
		}

		s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)

		got, _ := st.GetComponent(comp.ID)
		if !got.Status.BackoffUntil.IsZero() {
			t.Errorf("BackoffUntil = %v, want zero (exit 0 under on-failure must not restart)", got.Status.BackoffUntil)
		}
	})

	t.Run("crash exit 1 -> restart scheduled", func(t *testing.T) {
		rt := newFakeRuntime()
		st := newFakeStore()
		s := newTestService(rt, st)

		comp := mustDeploy(t, s, "web")
		fresh, _ := st.GetComponent(comp.ID)
		fresh.Spec.RestartPolicy = RestartPolicyOnFailure
		fresh.Status.ContainerID = ""
		fresh.Status.LastExitCode = 1
		if err := st.SaveComponent(fresh); err != nil {
			t.Fatal(err)
		}

		s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)

		got, _ := st.GetComponent(comp.ID)
		if got.Status.BackoffUntil.IsZero() {
			t.Errorf("BackoffUntil is zero, want a scheduled restart (non-zero exit under on-failure)")
		}
		if got.Status.RestartCount != 1 {
			t.Errorf("RestartCount = %d, want 1", got.Status.RestartCount)
		}
	})
}

// ---------------------------------------------------------------------------
// Reconcile drift: missing container (reconcile.go)
// ---------------------------------------------------------------------------

// TestReconcile_MissingContainerRunningDesiredRestarts confirms the drift path:
// a Running-desired component whose container has vanished from the runtime is
// handed to the restart manager (RestartPolicy=always), which schedules a
// restart. InspectContainer is stubbed to report the old container as gone so
// EvaluateRestart's "already Running?" short-circuit does not fire.
func TestReconcile_MissingContainerRunningDesiredRestarts(t *testing.T) {
	rt := newFakeRuntime()
	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return nil, nil // no live containers: the component's container is gone
	}
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{}, errors.New("no such container")
	}
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	fresh.Status.StartedAt = time.Now() // recent: stable-window reset must not fire
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, _ := st.GetComponent(comp.ID)
	if got.Status.RestartCount != 1 {
		t.Errorf("RestartCount = %d, want 1 (drift on a Running-desired component must schedule a restart)", got.Status.RestartCount)
	}
	if got.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil is zero, want a scheduled restart")
	}
}

// TestReconcile_MissingContainerStoppedDesiredMarkedExited is the contrasting
// case: a Stopped-desired component whose container is gone must be marked
// Exited, never restarted.
func TestReconcile_MissingContainerStoppedDesiredMarkedExited(t *testing.T) {
	rt := newFakeRuntime()
	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return nil, nil
	}
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.DesiredState = DesiredStopped
	fresh.Status.Phase = runtime.PhaseRunning // stale: container is actually gone
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseExited {
		t.Errorf("Phase = %v, want Exited (Stopped-desired missing container must not restart)", got.Status.Phase)
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0", got.Status.RestartCount)
	}
	if rt.callCount("CreateContainer") != 1 { // only the original Deploy
		t.Errorf("CreateContainer called %d times, want 1 (no restart-triggered recreate)", rt.callCount("CreateContainer"))
	}
}

// ---------------------------------------------------------------------------
// Rollback (service.go)
// ---------------------------------------------------------------------------

// TestRollback_NothingToRollback confirms a component that has never been
// upserted/rolled back (no PreviousImage) returns ErrNothingToRollback.
func TestRollback_NothingToRollback(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")

	_, err := s.Rollback(context.Background(), comp.ID)
	if !errors.Is(err, ErrNothingToRollback) {
		t.Fatalf("Rollback err = %v, want ErrNothingToRollback", err)
	}
}

// TestRollback_TwoRollbacksSwapImage confirms Rollback swaps image with
// PreviousImage, and that two consecutive Rollbacks exchange the pair back and
// forth (rollback to go back a version, rollback again to undo it).
func TestRollback_TwoRollbacksSwapImage(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	// Deploy v1, then upsert to v2 so PreviousImage is recorded as v1.
	if _, err := s.Deploy(context.Background(), ComponentSpec{Name: "web", Image: "img:v1"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	up, _, err := s.Upsert(context.Background(), ComponentSpec{Name: "web", Image: "img:v2"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if up.Spec.Image != "img:v2" || up.Status.PreviousImage != "img:v1" {
		t.Fatalf("after upsert: image=%q previous=%q, want v2/v1", up.Spec.Image, up.Status.PreviousImage)
	}

	// First rollback: back to v1, previous becomes v2.
	r1, err := s.Rollback(context.Background(), up.ID)
	if err != nil {
		t.Fatalf("Rollback #1: %v", err)
	}
	if r1.Spec.Image != "img:v1" {
		t.Errorf("after rollback #1: image = %q, want img:v1", r1.Spec.Image)
	}
	if r1.Status.PreviousImage != "img:v2" {
		t.Errorf("after rollback #1: previous = %q, want img:v2", r1.Status.PreviousImage)
	}

	// Second rollback: swaps back to v2, previous becomes v1 again.
	r2, err := s.Rollback(context.Background(), up.ID)
	if err != nil {
		t.Fatalf("Rollback #2: %v", err)
	}
	if r2.Spec.Image != "img:v2" {
		t.Errorf("after rollback #2: image = %q, want img:v2 (consecutive rollbacks must swap)", r2.Spec.Image)
	}
	if r2.Status.PreviousImage != "img:v1" {
		t.Errorf("after rollback #2: previous = %q, want img:v1", r2.Status.PreviousImage)
	}
	if r2.ID != up.ID {
		t.Errorf("ID changed across rollback: %q -> %q, want stable ULID", up.ID, r2.ID)
	}
}

// TestReconcile_ExitedContainerRunningDesiredNoPolicyShowsExited covers the
// component nothing is going to restart: policy "no" means a crashed container
// stays crashed, so if reconcile deferred to the restart manager without
// adopting the observed phase first, the record would keep claiming Running
// indefinitely — and `component ls`, the one place an operator looks, would
// describe a container that died days ago as healthy.
func TestReconcile_ExitedContainerRunningDesiredNoPolicyShowsExited(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	cid := runtime.ContainerID(fresh.Status.ContainerID)
	fresh.Spec.RestartPolicy = RestartPolicyNo
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}
	// The container is still there, it just exited.
	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{{
			ID:     cid,
			Phase:  runtime.PhaseExited,
			Labels: map[string]string{LabelManaged: "true", LabelComponentID: comp.ID},
		}}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseExited {
		t.Errorf("Phase = %v, want Exited (an exited container must not be reported as Running)", got.Status.Phase)
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0 (policy \"no\" must not restart)", got.Status.RestartCount)
	}
}

// TestReconcile_BackfillsStartedAtForAdoptedContainer covers a running
// component whose record carries no start time — adopted from an orphan, or
// written before the field existed. Without the backfill it reports no uptime
// for as long as it runs, and the restart manager's stable-window reset (which
// keys off StartedAt) never fires for it either.
func TestReconcile_BackfillsStartedAtForAdoptedContainer(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	cid := runtime.ContainerID(fresh.Status.ContainerID)
	fresh.Status.StartedAt = time.Time{}
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-3 * time.Hour)
	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{{
			ID:     cid,
			Phase:  runtime.PhaseRunning,
			Labels: map[string]string{LabelManaged: "true", LabelComponentID: comp.ID},
		}}, nil
	}
	rt.InspectContainerFunc = func(runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: cid, Phase: runtime.PhaseRunning, StartedAt: started}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, _ := st.GetComponent(comp.ID)
	if !got.Status.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v (the container's own start time)", got.Status.StartedAt, started)
	}
}

// A container that was created and never started (its host port was taken, say)
// leaves docker reporting "created", which maps to Creating. Reconcile used to
// adopt that over the Failed the deploy had just recorded, so a failed deploy
// read as still in progress — with the reason for the failure sitting in
// LastError right beside a phase that contradicted it.
func TestReconcileKeepsFailedOverACreatedContainer(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	svc := NewService(rt, st, "docker", "", testLogger())

	comp := &Component{
		ID:   "c1",
		Spec: ComponentSpec{Name: "web", Namespace: "default", Image: "nginx"},
		Status: ComponentStatus{
			Phase:        runtime.PhaseFailed,
			ContainerID:  "cid1",
			DesiredState: DesiredRunning,
			LastError:    "start container: host port already in use",
		},
	}
	if err := st.SaveComponent(comp); err != nil {
		t.Fatalf("save: %v", err)
	}

	changed, err := svc.reconcileAdopt("c1", runtime.ContainerStatus{
		ID:    runtime.ContainerID("cid1"),
		Phase: runtime.PhaseCreating,
	})
	if err != nil {
		t.Fatalf("reconcileAdopt: %v", err)
	}
	if changed {
		t.Error("adopting Creating over Failed reported a change")
	}

	got, err := st.GetComponent("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != runtime.PhaseFailed {
		t.Errorf("phase = %v, want Failed to survive", got.Status.Phase)
	}
	if got.Status.LastError == "" {
		t.Error("the reason for the failure was dropped")
	}
}

// With no policy given a crashed component comes back: the default is
// "always" (2026-11-12), and it is stored as such so component get shows it.
func TestEvaluateRestart_DefaultRestarts(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	if comp.Spec.RestartPolicy != RestartPolicyAlways {
		t.Fatalf("stored policy = %q, want always", comp.Spec.RestartPolicy)
	}
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.ContainerID = ""
	fresh.Status.LastExitCode = 0 // a clean exit too: a reboot looks like one
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}
	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)
	got, _ := st.GetComponent(comp.ID)
	if got.Status.RestartCount != 1 {
		t.Errorf("RestartCount = %d, want 1: the default must bring it back", got.Status.RestartCount)
	}
}
