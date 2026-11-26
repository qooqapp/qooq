package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// This file covers M20's orphan-adoption path (reconcile.go's adoptOrphan):
// reconstructing a Component record from a live, labeled container when the
// store has no matching record (the `docker compose down -v` disaster
// scenario). It reuses service_test.go's fakeStore/fakeRuntime.

// specLabels builds the label set a real createAndStart would have put on an
// orphaned container: qooq.managed, qooq.component_id, qooq.spec (marshaled
// spec) and, if instance is non-empty, qooq.instance.
func specLabels(t *testing.T, componentID, instance string, spec ComponentSpec) map[string]string {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	labels := map[string]string{
		LabelManaged:     "true",
		LabelComponentID: componentID,
		LabelSpec:        string(b),
	}
	if instance != "" {
		labels[LabelInstance] = instance
	}
	return labels
}

// (a) A running orphan with a valid qooq.spec label is adopted as
// DesiredRunning, with the full spec restored.
func TestAdoptOrphan_RunningContainerAdoptedAsDesiredRunning(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	const orphanID = "01ORPHANRUNNING00000000000"
	spec := ComponentSpec{Name: "adoptweb", Image: "nginx:alpine", RestartPolicy: RestartPolicyAlways}
	labels := specLabels(t, orphanID, "", spec)
	created := time.Now().Add(-time.Hour)

	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{{ID: "container-1", Name: "adoptweb", Phase: runtime.PhaseRunning, Labels: labels}}, nil
	}
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Name: "adoptweb", Phase: runtime.PhaseRunning, Labels: labels, Created: created}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, err := st.GetComponent(orphanID)
	if err != nil {
		t.Fatalf("GetComponent(%s): %v", orphanID, err)
	}
	if got.Status.DesiredState != DesiredRunning {
		t.Errorf("DesiredState = %q, want %q", got.Status.DesiredState, DesiredRunning)
	}
	if got.Spec.Name != "adoptweb" || got.Spec.Image != "nginx:alpine" || got.Spec.RestartPolicy != RestartPolicyAlways {
		t.Errorf("Spec not fully restored: %+v", got.Spec)
	}
	if got.Status.ContainerID != "container-1" {
		t.Errorf("ContainerID = %q, want container-1", got.Status.ContainerID)
	}
	if got.Status.Phase != runtime.PhaseRunning {
		t.Errorf("Phase = %v, want Running", got.Status.Phase)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v (from InspectContainer)", got.CreatedAt, created)
	}
	events, _ := st.ListEvents(orphanID)
	found := false
	for _, e := range events {
		if e.Type == "adopted_orphan" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an adopted_orphan event, got %+v", events)
	}
}

// (b) An exited orphan is adopted as DesiredStopped and must not be
// auto-restarted by a subsequent reconcile pass (reconcile only restarts
// Running-desired components).
func TestAdoptOrphan_ExitedContainerAdoptedAsDesiredStoppedNotRestarted(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	const orphanID = "01ORPHANEXITED0000000000000"
	spec := ComponentSpec{Name: "adoptstop", Image: "nginx:alpine", RestartPolicy: RestartPolicyAlways}
	labels := specLabels(t, orphanID, "", spec)

	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{{ID: "container-2", Name: "adoptstop", Phase: runtime.PhaseExited, Labels: labels}}, nil
	}
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Name: "adoptstop", Phase: runtime.PhaseExited, Labels: labels}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	got, err := st.GetComponent(orphanID)
	if err != nil {
		t.Fatalf("GetComponent(%s): %v", orphanID, err)
	}
	if got.Status.DesiredState != DesiredStopped {
		t.Errorf("DesiredState = %q, want %q", got.Status.DesiredState, DesiredStopped)
	}
	if got.Status.Phase != runtime.PhaseExited {
		t.Errorf("Phase = %v, want Exited", got.Status.Phase)
	}

	// A second pass (same live state, now a known component) must not
	// recreate the container: only Running-desired components are ever
	// handed to the restart manager.
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	if n := rt.callCount("CreateContainer"); n != 0 {
		t.Errorf("CreateContainer called %d times, want 0 (Stopped-desired must never be restarted)", n)
	}
	got2, _ := st.GetComponent(orphanID)
	if got2.Status.DesiredState != DesiredStopped {
		t.Errorf("after second pass: DesiredState = %q, want %q (must stay Stopped)", got2.Status.DesiredState, DesiredStopped)
	}
}

// (c) A container labeled for a different qooqd instance is never adopted,
// and produces no component record at all under this instance's store.
func TestAdoptOrphan_DifferentInstanceNotAdopted(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestServiceInstance(rt, st, "primary")

	const orphanID = "01ORPHANOTHERINST000000000"
	spec := ComponentSpec{Name: "betaweb", Image: "nginx:alpine"}
	labels := specLabels(t, orphanID, "beta", spec) // belongs to instance "beta", we are "primary"

	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{{ID: "container-3", Name: "betaweb", Phase: runtime.PhaseRunning, Labels: labels}}, nil
	}
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		t.Fatalf("InspectContainer must not be called for a foreign-instance container")
		return runtime.ContainerStatus{}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := st.GetComponent(orphanID); err == nil {
		t.Errorf("component %s was adopted, want it to remain absent (different instance)", orphanID)
	}
	comps, _ := st.ListComponents()
	if len(comps) != 0 {
		t.Errorf("ListComponents = %+v, want empty (nothing adopted)", comps)
	}
}

// (d) A managed orphan container with no qooq.spec label cannot be
// reconstructed and must be left alone (pre-M20 warn-only behavior).
func TestAdoptOrphan_MissingSpecLabelNotAdopted(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	const orphanID = "01ORPHANNOSPEC00000000000000"
	labels := map[string]string{
		LabelManaged:     "true",
		LabelComponentID: orphanID,
		// no LabelSpec
	}

	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{{ID: "container-4", Name: "legacy", Phase: runtime.PhaseRunning, Labels: labels}}, nil
	}
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		t.Fatalf("InspectContainer must not be called when qooq.spec label is absent")
		return runtime.ContainerStatus{}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := st.GetComponent(orphanID); err == nil {
		t.Errorf("component %s was adopted, want it to remain absent (no qooq.spec label)", orphanID)
	}
}

// (e) A container whose reconstructed spec.Name collides with an existing,
// non-Removed component is not adopted (would create an ambiguous duplicate
// name for the CLI).
func TestAdoptOrphan_NameConflictNotAdopted(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	// An operator has already (re)deployed "web" by hand into the fresh store.
	existing := mustDeploy(t, s, "web")

	const orphanID = "01ORPHANNAMECONFLICT0000000"
	spec := ComponentSpec{Name: "web", Image: "nginx:alpine"} // same name as `existing`
	labels := specLabels(t, orphanID, "", spec)

	rt.ListContainersFunc = func(runtime.Filter) ([]runtime.ContainerStatus, error) {
		return []runtime.ContainerStatus{
			// The already-known component's own container, so Reconcile's
			// normal adopt path (not orphan) handles it without incident.
			{ID: runtime.ContainerID(existing.Status.ContainerID), Name: "web", Phase: runtime.PhaseRunning,
				Labels: map[string]string{LabelManaged: "true", LabelComponentID: existing.ID}},
			// The orphan claiming the same name via its qooq.spec label.
			{ID: "container-5", Name: "web-orphan", Phase: runtime.PhaseRunning, Labels: labels},
		}, nil
	}
	rt.InspectContainerFunc = func(id runtime.ContainerID) (runtime.ContainerStatus, error) {
		return runtime.ContainerStatus{ID: id, Name: "web-orphan", Phase: runtime.PhaseRunning, Labels: labels}, nil
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := st.GetComponent(orphanID); err == nil {
		t.Errorf("component %s was adopted, want it rejected (name %q already in use)", orphanID, spec.Name)
	}
	comps, _ := st.ListComponents()
	if len(comps) != 1 {
		t.Errorf("ListComponents = %+v, want exactly the pre-existing %q component", comps, "web")
	}
}
