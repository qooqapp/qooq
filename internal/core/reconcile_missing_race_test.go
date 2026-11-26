package core

import (
	"context"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// A pass that saw "no container" before a deploy put one in place must not
// mark the new, running container Exited. Seen for real: 139ms after an
// upsert the record said Exited, the watcher reported the new version as not
// started, and the deploy counted toward pausing a project whose app was fine.
func TestReconcileMissingLeavesAContainerThatArrivedMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot string // ContainerID when the pass took its snapshot
		phase    runtime.Phase
		desired  string
	}{
		{"upsert in flight (empty, then set)", "", runtime.PhaseRunning, DesiredRunning},
		{"replaced by another container", "old-cid", runtime.PhaseRunning, DesiredStopped},
		{"deploy finished while waiting", "", runtime.PhaseCreating, DesiredRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, st := newFakeRuntime(), newFakeStore()
			svc := newTestService(rt, st)
			now := &Component{ID: "c1", Spec: ComponentSpec{Name: "web", Image: "nginx"},
				Status: ComponentStatus{ContainerID: "new-cid", Phase: tc.phase, DesiredState: tc.desired}}
			if err := st.SaveComponent(now); err != nil {
				t.Fatal(err)
			}
			stale := *now
			stale.Status.ContainerID = tc.snapshot

			svc.reconcileMissing(context.Background(), &stale)

			got, _ := st.GetComponent("c1")
			if got.Status.Phase != tc.phase {
				t.Errorf("phase = %s, want %s left alone: the container it judged missing is not this one", got.Status.Phase, tc.phase)
			}
		})
	}
}

// The case it exists for still works: the same container is gone and was not
// wanted running.
func TestReconcileMissingStillMarksAVanishedContainer(t *testing.T) {
	rt, st := newFakeRuntime(), newFakeStore()
	svc := newTestService(rt, st)
	c := &Component{ID: "c1", Spec: ComponentSpec{Name: "web", Image: "nginx"},
		Status: ComponentStatus{ContainerID: "cid", Phase: runtime.PhaseRunning, DesiredState: DesiredStopped}}
	if err := st.SaveComponent(c); err != nil {
		t.Fatal(err)
	}
	snap := *c
	svc.reconcileMissing(context.Background(), &snap)
	got, _ := st.GetComponent("c1")
	if got.Status.Phase != runtime.PhaseExited {
		t.Errorf("phase = %s, want Exited", got.Status.Phase)
	}
}
