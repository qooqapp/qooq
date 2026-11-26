package core

import (
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// A high range nothing on a test machine is likely to hold, so these exercise
// the allocation rules rather than whatever happens to be listening.
const (
	testLow  = 41000
	testHigh = 41099
)

func auto(container int) runtime.PortMapping {
	return runtime.PortMapping{ContainerPort: container, AutoHost: true}
}

func fixed(host, container int) runtime.PortMapping {
	return runtime.PortMapping{HostPort: host, ContainerPort: container}
}

func resolve(t *testing.T, ports, prev []runtime.PortMapping, taken map[int]bool) []runtime.PortMapping {
	t.Helper()
	got, err := resolveAutoPorts(ports, prev, taken, testLow, testHigh)
	if err != nil {
		t.Fatalf("resolveAutoPorts: %v", err)
	}
	return got
}

// The rule the whole feature rests on: a component keeps the port it was
// given. An address that changes on redeploy is worse than no address, because
// everything pointing at the old one fails far from the change.
func TestRedeployKeepsTheSamePort(t *testing.T) {
	first := resolve(t, []runtime.PortMapping{auto(8080)}, nil, nil)
	assigned := first[0].HostPort
	if assigned < testLow || assigned > testHigh {
		t.Fatalf("assigned %d, want a port inside %d-%d", assigned, testLow, testHigh)
	}

	// Same spec again, now with the component's own previous ports in hand,
	// and with that port marked taken — which is what the store reports once
	// the component holds it.
	again := resolve(t, []runtime.PortMapping{auto(8080)}, first, map[int]bool{assigned: true})
	if again[0].HostPort != assigned {
		t.Errorf("redeploy moved the port from %d to %d", assigned, again[0].HostPort)
	}
}

// Reuse matches on the container port, so adding or reordering ports in a
// manifest does not shuffle the addresses of the ones that already existed.
func TestReuseSurvivesReorderingAndAdditions(t *testing.T) {
	prev := []runtime.PortMapping{fixed(41010, 8080), fixed(41011, 9090)}
	got := resolve(t, []runtime.PortMapping{auto(9090), auto(7070), auto(8080)}, prev, map[int]bool{41010: true, 41011: true})

	if got[0].HostPort != 41011 {
		t.Errorf("9090 moved to %d, want its previous 41011", got[0].HostPort)
	}
	if got[2].HostPort != 41010 {
		t.Errorf("8080 moved to %d, want its previous 41010", got[2].HostPort)
	}
	if got[1].HostPort == 41010 || got[1].HostPort == 41011 {
		t.Errorf("the new port took one already in use: %d", got[1].HostPort)
	}
}

// Resolution must clear the flag: a stored component has to name the port it
// actually got, or nothing downstream — the label, the response, the ingress
// route — can tell where it is published.
func TestResolutionLeavesNoAutoBehind(t *testing.T) {
	got := resolve(t, []runtime.PortMapping{auto(8080)}, nil, nil)
	if got[0].AutoHost {
		t.Error("AutoHost survived resolution")
	}
	if got[0].HostPort == 0 {
		t.Error("resolved to no port at all")
	}
}

// Two auto ports in one spec are different ports, and neither may land on a
// port the same spec pins explicitly.
func TestOneSpecDoesNotCollideWithItself(t *testing.T) {
	got := resolve(t, []runtime.PortMapping{fixed(41000, 80), auto(8080), auto(9090)}, nil, nil)
	if got[1].HostPort == got[2].HostPort {
		t.Errorf("both auto ports got %d", got[1].HostPort)
	}
	for _, i := range []int{1, 2} {
		if got[i].HostPort == 41000 {
			t.Errorf("an auto port took 41000, which this same spec pins")
		}
	}
}

func TestAllocationSkipsPortsOtherComponentsHold(t *testing.T) {
	taken := map[int]bool{}
	for p := testLow; p < testLow+5; p++ {
		taken[p] = true
	}
	got := resolve(t, []runtime.PortMapping{auto(8080)}, nil, taken)
	if taken[got[0].HostPort] {
		t.Errorf("allocated %d, which another component holds", got[0].HostPort)
	}
}

// Running out is a configuration problem — the range is too small — so it has
// to say so rather than surfacing as a failed container start.
func TestExhaustedRangeSaysSo(t *testing.T) {
	_, err := resolveAutoPorts([]runtime.PortMapping{auto(8080)}, nil, map[int]bool{41000: true}, 41000, 41000)
	if err == nil {
		t.Fatal("want an error when the range is full")
	}
	if !isNoFreePort(err) {
		t.Errorf("got %v, want ErrNoFreePort", err)
	}
}

// A spec with no auto ports must come back byte-for-byte, so the common case
// cannot be perturbed by a feature it does not use.
func TestSpecWithoutAutoIsUntouched(t *testing.T) {
	in := []runtime.PortMapping{fixed(8080, 80), {ContainerPort: 9090}}
	if hasAutoPort(in) {
		t.Fatal("hasAutoPort said yes for a spec with none")
	}
	got := resolve(t, in, nil, nil)
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("port %d changed: %+v -> %+v", i, in[i], got[i])
		}
	}
}

func TestTakenHostPortsIgnoresRemovedAndSelf(t *testing.T) {
	comps := []*Component{
		{ID: "self", Spec: ComponentSpec{Ports: []runtime.PortMapping{fixed(41000, 80)}}},
		{ID: "other", Spec: ComponentSpec{Ports: []runtime.PortMapping{fixed(41001, 80)}}},
		{
			ID:     "gone",
			Spec:   ComponentSpec{Ports: []runtime.PortMapping{fixed(41002, 80)}},
			Status: ComponentStatus{Phase: runtime.PhaseRemoved},
		},
	}
	taken := takenHostPorts(comps, "self")
	if taken[41000] {
		t.Error("a component's own port counted against it")
	}
	if !taken[41001] {
		t.Error("another component's port was not counted")
	}
	if taken[41002] {
		t.Error("a removed component still holds a port")
	}
}

func isNoFreePort(err error) bool {
	for err != nil {
		if err == ErrNoFreePort {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
