package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(rt *fakeRuntime, st *fakeStore) *Service {
	return NewService(rt, st, "fake", "", testLogger())
}

// newTestServiceInstance is like newTestService but pins a non-default
// instance id, for tests exercising OwnsContainer/instance-isolation.
func newTestServiceInstance(rt *fakeRuntime, st *fakeStore, instance string) *Service {
	return NewService(rt, st, "fake", instance, testLogger())
}

func mustDeploy(t *testing.T, s *Service, name string) *Component {
	t.Helper()
	c, err := s.Deploy(context.Background(), ComponentSpec{Name: name, Image: "img:latest"})
	if err != nil {
		t.Fatalf("Deploy(%s): %v", name, err)
	}
	return c
}

// ---------------------------------------------------------------------------
// 1. backoff calculation
// ---------------------------------------------------------------------------

func TestBackoffDuration(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // 64s -> capped at 60s
		{7, 60 * time.Second}, // count clamped to 6 internally, still capped
		{100, 60 * time.Second},
		{-1, 1 * time.Second}, // negative clamped to 0
	}
	for _, tc := range cases {
		got := backoffDuration(tc.count)
		if got != tc.want {
			t.Errorf("backoffDuration(%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

// TestEvaluateRestart_StableWindowResetsCount exercises the restartStableWindow
// reset logic: a component that has been Running for >= restartStableWindow
// since StartedAt gets RestartCount reset to 0 (and RestartExhausted cleared)
// before the new restart is counted, so a crash after a long stable run is
// treated as attempt #1 again, not a continuation of an old streak.
func TestEvaluateRestart_StableWindowResetsCount(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "stable")
	// Simulate: component has been running for a long time (older than
	// restartStableWindow) and has an existing RestartCount from a past
	// crash-loop streak that should now be forgotten.
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.RestartCount = 5
	fresh.Status.StartedAt = time.Now().Add(-restartStableWindow - time.Second)
	fresh.Status.RestartExhausted = false
	// Force the container to look "down" so EvaluateRestart proceeds past the
	// InspectContainer short-circuit.
	fresh.Status.ContainerID = ""
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)

	got, _ := st.GetComponent(comp.ID)
	if got.Status.RestartCount != 1 {
		t.Fatalf("RestartCount = %d, want 1 (stable window should have reset the old streak)", got.Status.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// 2 & 3. DesiredState semantics in HandleEvent, and stale die events
// ---------------------------------------------------------------------------

func TestStopSetsDesiredStopped_DeployStartSetRunning(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	if comp.Status.DesiredState != DesiredRunning {
		t.Fatalf("Deploy: DesiredState = %q, want %q", comp.Status.DesiredState, DesiredRunning)
	}

	stopped, err := s.Stop(context.Background(), comp.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Status.DesiredState != DesiredStopped {
		t.Fatalf("Stop: DesiredState = %q, want %q", stopped.Status.DesiredState, DesiredStopped)
	}

	started, err := s.Start(context.Background(), comp.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status.DesiredState != DesiredRunning {
		t.Fatalf("Start: DesiredState = %q, want %q", started.Status.DesiredState, DesiredRunning)
	}
}

// TestHandleEvent_DesiredStoppedDieDoesNotCountAsCrash is the M17 regression
// test: when DesiredState is Stopped, a non-zero exit code on a "die" event
// must be recorded as Exited without incrementing CrashCount or triggering a
// restart (an app that ignores SIGTERM exiting 137/143 after an operator Stop
// is not a crash).
func TestHandleEvent_DesiredStoppedDieDoesNotCountAsCrash(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.DesiredState = DesiredStopped
	fresh.Spec.RestartPolicy = RestartPolicyAlways // would restart if this were a real crash
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.HandleEvent(context.Background(), runtime.Event{
		ContainerID: runtime.ContainerID(fresh.Status.ContainerID),
		Action:      "die",
		ExitCode:    137,
		Labels:      map[string]string{LabelComponentID: comp.ID},
	})

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseExited {
		t.Errorf("Phase = %v, want Exited", got.Status.Phase)
	}
	if got.Status.CrashCount != 0 {
		t.Errorf("CrashCount = %d, want 0", got.Status.CrashCount)
	}

	// No restart should have been scheduled: BackoffUntil must remain zero.
	if !got.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil = %v, want zero (no restart should have been scheduled)", got.Status.BackoffUntil)
	}
	if rt.callCount("CreateContainer") != 1 { // only the original Deploy create
		t.Errorf("CreateContainer called %d times, want 1 (no restart-triggered recreate)", rt.callCount("CreateContainer"))
	}
}

// TestHandleEvent_DesiredRunningDieCountsCrash is the contrasting case: a
// non-zero exit while DesiredState is still Running is a genuine crash and
// must increment CrashCount and mark Failed.
func TestHandleEvent_DesiredRunningDieCountsCrash(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")

	s.HandleEvent(context.Background(), runtime.Event{
		ContainerID: runtime.ContainerID(comp.Status.ContainerID),
		Action:      "die",
		ExitCode:    1,
		Labels:      map[string]string{LabelComponentID: comp.ID},
	})

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseFailed {
		t.Errorf("Phase = %v, want Failed", got.Status.Phase)
	}
	if got.Status.CrashCount != 1 {
		t.Errorf("CrashCount = %d, want 1", got.Status.CrashCount)
	}
}

// TestHandleEvent_StaleDieEventIgnored is the second M17 regression: a die
// event whose ContainerID no longer matches the component's current
// ContainerID (e.g. arriving after a health-triggered force-remove already
// swapped in a new container) must be recorded as "died_stale" only, without
// touching Phase/CrashCount.
func TestHandleEvent_StaleDieEventIgnored(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	currentContainerID := fresh.Status.ContainerID
	// Simulate the component having already moved on to a new container.
	fresh.Status.ContainerID = "new-container-id"
	fresh.Status.Phase = runtime.PhaseRunning
	fresh.Status.CrashCount = 0
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.HandleEvent(context.Background(), runtime.Event{
		ContainerID: runtime.ContainerID(currentContainerID), // the OLD/stale id
		Action:      "die",
		ExitCode:    1,
		Labels:      map[string]string{LabelComponentID: comp.ID},
	})

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseRunning {
		t.Errorf("Phase = %v, want unchanged Running", got.Status.Phase)
	}
	if got.Status.CrashCount != 0 {
		t.Errorf("CrashCount = %d, want 0 (stale event must not increment it)", got.Status.CrashCount)
	}
	if got.Status.ContainerID != "new-container-id" {
		t.Errorf("ContainerID = %q, want unchanged", got.Status.ContainerID)
	}

	events, _ := st.ListEvents(comp.ID)
	found := false
	for _, e := range events {
		if e.Type == "died_stale" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a died_stale event to be recorded, events: %+v", events)
	}
}

// ---------------------------------------------------------------------------
// 4. reserveName / deployMu
// ---------------------------------------------------------------------------

func TestDeploy_DuplicateNameConflict(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	mustDeploy(t, s, "dup")
	_, err := s.Deploy(context.Background(), ComponentSpec{Name: "dup", Image: "img:latest"})
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("second Deploy err = %v, want ErrNameConflict", err)
	}
}

func TestDeploy_SameNameAllowedAfterRemove(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "dup")
	if err := s.Remove(context.Background(), comp.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Deploy(context.Background(), ComponentSpec{Name: "dup", Image: "img:latest"}); err != nil {
		t.Fatalf("Deploy after remove: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Upsert: create, replace, TOCTOU fallback
// ---------------------------------------------------------------------------

func TestUpsert_CreatesWhenNameFree(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp, created, err := s.Upsert(context.Background(), ComponentSpec{Name: "svc", Image: "img:v1"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !created {
		t.Errorf("created = false, want true")
	}
	if comp.Spec.Image != "img:v1" {
		t.Errorf("Image = %q, want img:v1", comp.Spec.Image)
	}
}

func TestUpsert_ReplacesExistingSameID(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	orig := mustDeploy(t, s, "svc")

	replaced, created, err := s.Upsert(context.Background(), ComponentSpec{Name: "svc", Image: "img:v2"})
	if err != nil {
		t.Fatalf("Upsert (replace): %v", err)
	}
	if created {
		t.Errorf("created = true, want false (replace path)")
	}
	if replaced.ID != orig.ID {
		t.Errorf("ID = %q, want unchanged %q (ULID must be preserved across replace)", replaced.ID, orig.ID)
	}
	if replaced.Spec.Image != "img:v2" {
		t.Errorf("Image = %q, want img:v2", replaced.Spec.Image)
	}
	if replaced.Status.PreviousImage != "img:latest" {
		t.Errorf("PreviousImage = %q, want img:latest (orig image)", replaced.Status.PreviousImage)
	}
}

// TestUpsert_TOCTOUFallsBackToCreate exercises the race documented on Upsert:
// findIDByName resolves an id, but the component is removed before the
// replace path's per-id lock/GetComponent runs. Upsert must fall back to
// createComponent instead of propagating ErrNotFound.
func TestUpsert_TOCTOUFallsBackToCreate(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	orig := mustDeploy(t, s, "svc")
	// Simulate a concurrent Remove landing between findIDByName and the
	// replace path's GetComponent: delete the store record directly (the
	// Service's own Remove would also clear locks/stats, which is fine here
	// since Upsert allocates a fresh id anyway).
	if err := st.DeleteComponent(orig.ID); err != nil {
		t.Fatal(err)
	}

	comp, created, err := s.Upsert(context.Background(), ComponentSpec{Name: "svc", Image: "img:v3"})
	if err != nil {
		t.Fatalf("Upsert after concurrent remove: %v", err)
	}
	if !created {
		t.Errorf("created = false, want true (TOCTOU fallback must create)")
	}
	if comp.ID == orig.ID {
		t.Errorf("ID = %q, want a fresh id distinct from removed %q", comp.ID, orig.ID)
	}
}

// ---------------------------------------------------------------------------
// 6. reconcileAdopt
// ---------------------------------------------------------------------------

func TestReconcileAdopt_PatchesWithoutClobberingConcurrentFields(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	// Simulate a concurrent write (e.g. HandleEvent bumping CrashCount) that
	// happened after Reconcile's outer ListComponents snapshot was taken.
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.CrashCount = 3
	fresh.Status.Phase = runtime.PhaseFailed
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	changed, err := s.reconcileAdopt(comp.ID, runtime.ContainerStatus{
		ID:    runtime.ContainerID(fresh.Status.ContainerID),
		Phase: runtime.PhaseRunning,
	})
	if err != nil {
		t.Fatalf("reconcileAdopt: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true (phase differs)")
	}

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseRunning {
		t.Errorf("Phase = %v, want Running (adopted)", got.Status.Phase)
	}
	if got.Status.CrashCount != 3 {
		t.Errorf("CrashCount = %d, want preserved 3 (must not clobber concurrent write)", got.Status.CrashCount)
	}
}

func TestReconcileAdopt_NoOpWhenUnchanged(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)

	changed, err := s.reconcileAdopt(comp.ID, runtime.ContainerStatus{
		ID:    runtime.ContainerID(fresh.Status.ContainerID),
		Phase: fresh.Status.Phase,
	})
	if err != nil {
		t.Fatalf("reconcileAdopt: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false (nothing actually differs)")
	}
}

// TestReconcileAdopt_DesiredStoppedFailedExitBecomesExited covers the mirror
// of HandleEvent's stop semantics inside reconcileAdopt: a container the
// runtime reports as Failed (non-zero exit) while DesiredState is Stopped
// must be adopted as Exited, not Failed.
func TestReconcileAdopt_DesiredStoppedFailedExitBecomesExited(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.DesiredState = DesiredStopped
	fresh.Status.Phase = runtime.PhaseRunning // stale, to force a diff
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	changed, err := s.reconcileAdopt(comp.ID, runtime.ContainerStatus{
		ID:    runtime.ContainerID(fresh.Status.ContainerID),
		Phase: runtime.PhaseFailed,
	})
	if err != nil {
		t.Fatalf("reconcileAdopt: %v", err)
	}
	if !changed {
		t.Fatalf("changed = false, want true")
	}

	got, _ := st.GetComponent(comp.ID)
	if got.Status.Phase != runtime.PhaseExited {
		t.Errorf("Phase = %v, want Exited (DesiredStopped must map Failed->Exited)", got.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// 7. Crash-loop terminal state
// ---------------------------------------------------------------------------

// TestEvaluateRestart_ExhaustsAfterMaxConsecutiveRestarts drives EvaluateRestart
// directly (bypassing the real time.AfterFunc-scheduled performRestart, since
// that would require waiting out real backoff) to confirm the RestartCount
// bookkeeping alone reaches the terminal state at maxConsecutiveRestarts.
func TestEvaluateRestart_ExhaustsAfterMaxConsecutiveRestarts(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	fresh.Status.ContainerID = "" // container "down": skip Inspect short-circuit
	fresh.Status.StartedAt = time.Time{}
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	// Drive EvaluateRestart maxConsecutiveRestarts times. Each call increments
	// RestartCount by 1 (StartedAt stays zero, so the stable-window reset
	// never fires) and, since BackoffUntil is set into the future, we manually
	// clear it between iterations to simulate the scheduled restart having
	// already fired without re-running the real container recreation.
	for i := 0; i < maxConsecutiveRestarts; i++ {
		s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)
		got, _ := st.GetComponent(comp.ID)
		if got.Status.RestartExhausted {
			break
		}
		got.Status.BackoffUntil = time.Time{}
		if err := st.SaveComponent(got); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := st.GetComponent(comp.ID)
	if !got.Status.RestartExhausted {
		t.Fatalf("RestartExhausted = false after %d restarts, want true", maxConsecutiveRestarts)
	}
	if got.Status.Phase != runtime.PhaseFailed {
		t.Errorf("Phase = %v, want Failed", got.Status.Phase)
	}
	if got.Status.RestartCount < maxConsecutiveRestarts {
		t.Errorf("RestartCount = %d, want >= %d", got.Status.RestartCount, maxConsecutiveRestarts)
	}

	// Further evaluations must be a no-op: RestartExhausted short-circuits
	// EvaluateRestart before it touches RestartCount again.
	before := got.Status.RestartCount
	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)
	after, _ := st.GetComponent(comp.ID)
	if after.Status.RestartCount != before {
		t.Errorf("RestartCount changed after exhaustion: %d -> %d, want unchanged", before, after.Status.RestartCount)
	}
}

// TestStart_ClearsRestartExhausted confirms a manual Start gives the
// component a fresh run of attempts.
func TestStart_ClearsRestartExhausted(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.RestartExhausted = true
	fresh.Status.RestartCount = 10
	fresh.Status.BackoffUntil = time.Now().Add(time.Minute)
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	started, err := s.Start(context.Background(), comp.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status.RestartExhausted {
		t.Errorf("RestartExhausted = true after manual Start, want false")
	}
	if started.Status.RestartCount != 0 {
		t.Errorf("RestartCount = %d after manual Start, want 0", started.Status.RestartCount)
	}
	if !started.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil = %v after manual Start, want zero", started.Status.BackoffUntil)
	}
}

// TestUpsertReplace_ClearsRestartExhausted confirms replaceComponentLocked
// (used by Upsert's replace path and Rollback) also resets the crash-loop
// bookkeeping.
func TestUpsertReplace_ClearsRestartExhausted(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.RestartExhausted = true
	fresh.Status.RestartCount = 10
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	replaced, _, err := s.Upsert(context.Background(), ComponentSpec{Name: "web", Image: "img:v2"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if replaced.Status.RestartExhausted {
		t.Errorf("RestartExhausted = true after Upsert-replace, want false")
	}
	if replaced.Status.RestartCount != 0 {
		t.Errorf("RestartCount = %d after Upsert-replace, want 0", replaced.Status.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// 8. EvaluateRestart shared path: lock re-entry, Inspect re-confirmation,
//    idempotency across triggers.
// ---------------------------------------------------------------------------

// TestEvaluateRestart_SkipsWhenInspectShowsRunning confirms the crash/drift
// short-circuit: EvaluateRestart re-Inspects before scheduling anything, and
// if the container turns out to already be Running it does nothing (no
// restart scheduled) — this is what makes health/crash/drift triggers race
// safely without double-restarting.
func TestEvaluateRestart_SkipsWhenInspectShowsRunning(t *testing.T) {
	rt := newFakeRuntime()
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Phase: runtime.PhaseRunning}, nil
	}
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonCrash)

	got, _ := st.GetComponent(comp.ID)
	if !got.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil = %v, want zero (Inspect showed Running, should be a no-op)", got.Status.BackoffUntil)
	}
}

// TestEvaluateRestart_HealthReasonBypassesRunningShortCircuit confirms a
// health-triggered restart is NOT skipped even though InspectContainer (or
// the component's own ContainerID check) would otherwise treat "already
// Running" as a no-op — the process is up but unresponsive, so it must still
// be recreated.
func TestEvaluateRestart_HealthReasonBypassesRunningShortCircuit(t *testing.T) {
	rt := newFakeRuntime()
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Phase: runtime.PhaseRunning}, nil
	}
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonHealth)

	got, _ := st.GetComponent(comp.ID)
	if got.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil is zero, want a scheduled restart (health reason must bypass the Running short-circuit)")
	}
}

// TestEvaluateRestart_NoopWhenDesiredStopped confirms EvaluateRestart never
// acts on a component whose DesiredState is Stopped, regardless of trigger.
func TestEvaluateRestart_NoopWhenDesiredStopped(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.DesiredState = DesiredStopped
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonDrift)

	got, _ := st.GetComponent(comp.ID)
	if !got.Status.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil = %v, want zero (DesiredStopped must be a no-op)", got.Status.BackoffUntil)
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0", got.Status.RestartCount)
	}
}

// TestEvaluateRestart_AlreadyScheduledIsIdempotent confirms a second trigger
// arriving while BackoffUntil is already in the future does not schedule a
// second restart (no double-increment of RestartCount).
func TestEvaluateRestart_AlreadyScheduledIsIdempotent(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	fresh.Status.ContainerID = ""
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}

	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonDrift)
	afterFirst, _ := st.GetComponent(comp.ID)
	if afterFirst.Status.RestartCount != 1 {
		t.Fatalf("after first trigger RestartCount = %d, want 1", afterFirst.Status.RestartCount)
	}

	// Second trigger (e.g. a health check firing moments later) while the
	// first restart is still pending must not schedule another one.
	s.EvaluateRestart(context.Background(), comp.ID, RestartReasonDrift)
	afterSecond, _ := st.GetComponent(comp.ID)
	if afterSecond.Status.RestartCount != 1 {
		t.Errorf("after second trigger RestartCount = %d, want still 1 (already scheduled)", afterSecond.Status.RestartCount)
	}
}

// TestPerformRestart_SkipsWhenAlreadyRunning confirms performRestart itself
// (the timer callback) re-validates and is a no-op if the container is
// already Running by the time the timer fires — the "idempotent across
// triggers" guarantee end to end.
func TestPerformRestart_SkipsWhenAlreadyRunning(t *testing.T) {
	rt := newFakeRuntime()
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Phase: runtime.PhaseRunning}, nil
	}
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	createCountBefore := rt.callCount("CreateContainer")

	s.performRestart(context.Background(), comp.ID, RestartReasonCrash)

	if rt.callCount("CreateContainer") != createCountBefore {
		t.Errorf("CreateContainer called again, want no-op since Inspect reports Running")
	}
	if rt.callCount("RemoveContainer") != 0 {
		t.Errorf("RemoveContainer called, want no-op")
	}
}

// TestPerformRestart_RecreatesWhenDown confirms the recreate path itself: a
// down container gets removed then createAndStart runs, bumping
// LastRestartAt.
func TestPerformRestart_RecreatesWhenDown(t *testing.T) {
	rt := newFakeRuntime()
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Phase: runtime.PhaseExited}, nil
	}
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")

	s.performRestart(context.Background(), comp.ID, RestartReasonCrash)

	got, _ := st.GetComponent(comp.ID)
	if got.Status.LastRestartAt.IsZero() {
		t.Errorf("LastRestartAt is zero, want set after recreate")
	}
	if rt.callCount("RemoveContainer") != 1 {
		t.Errorf("RemoveContainer called %d times, want 1", rt.callCount("RemoveContainer"))
	}
	if rt.callCount("CreateContainer") != 2 { // 1 from Deploy + 1 from restart
		t.Errorf("CreateContainer called %d times, want 2", rt.callCount("CreateContainer"))
	}
}

// TestPerformRestart_NoopWhenDesiredStoppedMeanwhile confirms performRestart
// respects a change of mind (Stop) that happened between EvaluateRestart
// scheduling the timer and the timer firing.
func TestPerformRestart_NoopWhenDesiredStoppedMeanwhile(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	if _, err := s.Stop(context.Background(), comp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	createCountBefore := rt.callCount("CreateContainer")

	s.performRestart(context.Background(), comp.ID, RestartReasonCrash)

	if rt.callCount("CreateContainer") != createCountBefore {
		t.Errorf("CreateContainer called again after Stop changed DesiredState, want no-op")
	}
}

// A component whose container cannot be created at all (a bind source that
// is gone) must climb the restart count like a crash loop does, back off, and
// reach RestartExhausted. Its StartedAt used to stay at the last successful
// start, so every failed recreate read as the end of a stable minute and the
// count went back to zero: retried every reconcile tick, forever.
func TestARecreateThatKeepsFailingIsGivenUpOn(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "db")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Status.StartedAt = time.Now().Add(-time.Hour) // last ran long ago
	fresh.Status.ContainerID = ""
	fresh.Spec.RestartPolicy = RestartPolicyAlways
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}
	rt.CreateContainerFunc = func(runtime.ContainerSpec) (runtime.ContainerID, error) {
		return "", errors.New(`invalid mount config for type "bind": bind source path does not exist`)
	}

	ctx := context.Background()
	for i := 1; i <= maxConsecutiveRestarts; i++ {
		// What the reconcile tick does once the scheduled backoff is over.
		c, _ := st.GetComponent(comp.ID)
		c.Status.BackoffUntil = time.Time{}
		_ = st.SaveComponent(c)
		s.EvaluateRestart(ctx, comp.ID, RestartReasonDrift)
		s.performRestart(ctx, comp.ID, RestartReasonDrift)

		got, _ := st.GetComponent(comp.ID)
		if got.Status.RestartExhausted {
			if i != maxConsecutiveRestarts {
				t.Fatalf("gave up after %d attempts, want %d", i, maxConsecutiveRestarts)
			}
			return
		}
		if got.Status.RestartCount != i {
			t.Fatalf("attempt %d: restart_count = %d — a failed recreate reset the count", i, got.Status.RestartCount)
		}
	}
	t.Fatal("never reached RestartExhausted")
}

// An OOM kill used to leave Phase=Failed with no reason anywhere the owner
// looks: the event log had it, LastError did not. The die that follows the OOM
// must not wipe it.
func TestHandleEvent_OOMSaysWhyAndSurvivesTheDie(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	comp := mustDeploy(t, s, "web")
	fresh, _ := st.GetComponent(comp.ID)
	fresh.Spec.MemLimitBytes = 512 << 20
	if err := st.SaveComponent(fresh); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{LabelComponentID: comp.ID}
	cid := runtime.ContainerID(fresh.Status.ContainerID)

	s.HandleEvent(context.Background(), runtime.Event{ContainerID: cid, Action: "oom", Labels: labels})
	s.HandleEvent(context.Background(), runtime.Event{ContainerID: cid, Action: "die", ExitCode: 137, Labels: labels})

	got, _ := st.GetComponent(comp.ID)
	if !strings.Contains(got.Status.LastError, "out of memory") || !strings.Contains(got.Status.LastError, "512Mi") {
		t.Errorf("LastError = %q, want the OOM and the 512Mi limit", got.Status.LastError)
	}
	if got.Status.Phase != runtime.PhaseFailed {
		t.Errorf("Phase = %v, want Failed", got.Status.Phase)
	}
}
