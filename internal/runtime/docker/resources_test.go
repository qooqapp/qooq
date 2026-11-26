package docker

import (
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

func TestResourcesForCarriesEveryLimit(t *testing.T) {
	r := resourcesFor(runtime.ContainerSpec{CPUNanos: 2e9, MemBytes: 1 << 28, PidsLimit: 2048})
	if r.NanoCPUs != 2e9 || r.Memory != 1<<28 {
		t.Errorf("cpu/mem not carried: %+v", r)
	}
	if r.MemorySwap != 1<<28 {
		t.Errorf("swap not pinned to the memory limit (docker would grant as much again): %d", r.MemorySwap)
	}
	if r.PidsLimit == nil || *r.PidsLimit != 2048 {
		t.Errorf("pids limit not carried: %v", r.PidsLimit)
	}
}

func TestResourcesForLeavesPidsUnsetAtZero(t *testing.T) {
	if r := resourcesFor(runtime.ContainerSpec{}); r.PidsLimit != nil {
		t.Errorf("zero must mean unset, got %d", *r.PidsLimit)
	}
}
