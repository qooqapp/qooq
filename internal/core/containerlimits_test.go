package core

import (
	"context"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// createdSpec deploys a component under the given policy and returns the
// ContainerSpec the runtime was actually handed.
func createdSpec(t *testing.T, l ContainerLimits, cpu float64, mem int64) runtime.ContainerSpec {
	t.Helper()
	var got runtime.ContainerSpec
	rt := &fakeRuntime{CreateContainerFunc: func(s runtime.ContainerSpec) (runtime.ContainerID, error) {
		got = s
		return "c1", nil
	}}
	svc := newTestService(rt, newFakeStore())
	svc.SetContainerLimits(l)
	_, err := svc.Deploy(context.Background(), ComponentSpec{Name: "lim", Image: "img:1", CPULimit: cpu, MemLimitBytes: mem})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	return got
}

func TestContainerLimitPolicy(t *testing.T) {
	const g = int64(1 << 30)
	cases := []struct {
		name     string
		l        ContainerLimits
		cpu      float64
		mem      int64
		wantMem  int64
		wantNano int64
		wantPids int64
	}{
		{"unwritten gets the ceiling", ContainerLimits{MemoryMaxBytes: g, CPUsMax: 2}, 0, 0, g, 2e9, 0},
		{"above the ceiling is clamped", ContainerLimits{MemoryMaxBytes: g, CPUsMax: 2}, 8, 4 * g, g, 2e9, 0},
		{"below the ceiling is kept", ContainerLimits{MemoryMaxBytes: g, CPUsMax: 2}, 0.5, g / 2, g / 2, 5e8, 0},
		{"no ceiling leaves the spec alone", ContainerLimits{}, 8, 4 * g, 4 * g, 8e9, 0},
		{"no ceiling and nothing asked stays unset", ContainerLimits{}, 0, 0, 0, 0, 0},
		{"pids passes through", ContainerLimits{PidsLimit: 2048}, 0, 0, 0, 0, 2048},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := createdSpec(t, c.l, c.cpu, c.mem)
			if got.MemBytes != c.wantMem || got.CPUNanos != c.wantNano || got.PidsLimit != c.wantPids {
				t.Errorf("mem=%d cpu=%d pids=%d, want mem=%d cpu=%d pids=%d",
					got.MemBytes, got.CPUNanos, got.PidsLimit, c.wantMem, c.wantNano, c.wantPids)
			}
		})
	}
}

func TestContainerPidsDefaultIs2048(t *testing.T) {
	if DefaultContainerPidsLimit != 2048 {
		t.Errorf("DefaultContainerPidsLimit = %d, want 2048", DefaultContainerPidsLimit)
	}
}

// The store keeps what was asked for; only the container gets the clamped value.
func TestClampingDoesNotRewriteTheStoredSpec(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(&fakeRuntime{}, st)
	svc.SetContainerLimits(ContainerLimits{MemoryMaxBytes: 1 << 30})
	c, err := svc.Deploy(context.Background(), ComponentSpec{Name: "lim", Image: "img:1", MemLimitBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.MemLimitBytes != 4<<30 {
		t.Errorf("stored spec rewritten to %d", c.Spec.MemLimitBytes)
	}
}

// Hardening is machine policy, carried to every container the runtime is
// asked to create — off unless the operator turned it on.
func TestHardeningReachesTheRuntime(t *testing.T) {
	if createdSpec(t, ContainerLimits{}, 0, 0).Harden {
		t.Error("hardened without being asked")
	}
	if !createdSpec(t, ContainerLimits{Harden: true}, 0, 0).Harden {
		t.Error("-harden-containers did not reach the container spec")
	}
}
