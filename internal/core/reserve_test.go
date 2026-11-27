package core

import (
	"context"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

func reservingService(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	s := newTestService(newFakeRuntime(), st)
	if err := s.SetAutoPortRange(testLow, testHigh); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func hostPort(ports []runtime.PortMapping) int {
	if len(ports) == 0 {
		return 0
	}
	return ports[0].HostPort
}

// The address is decided at reservation and is the one the deploy gets: an
// app that baked it into its config before this one ran must find it there.
func TestReservedPortIsTheOneTheDeployGets(t *testing.T) {
	s, st := reservingService(t)
	ctx := context.Background()
	spec := ComponentSpec{Name: "api", Ports: []runtime.PortMapping{auto(8080)}}

	c, reserved, err := s.Reserve(ctx, spec)
	if err != nil || !reserved {
		t.Fatalf("reserve: %v reserved=%v", err, reserved)
	}
	want := hostPort(c.Spec.Ports)
	if want == 0 || c.Spec.Namespace != "default" {
		t.Fatalf("reserved %+v", c.Spec)
	}
	// Asking again is the same answer, not a second port.
	again, _, _ := s.Reserve(ctx, spec)
	if hostPort(again.Spec.Ports) != want {
		t.Errorf("second reserve %d, want %d", hostPort(again.Spec.Ports), want)
	}

	// Someone else deploying in between does not get it.
	other, err := s.Deploy(ctx, ComponentSpec{Name: "other", Image: "img:latest", Ports: []runtime.PortMapping{auto(80)}})
	if err != nil {
		t.Fatal(err)
	}
	if hostPort(other.Spec.Ports) == want {
		t.Errorf("another component was handed the reserved port %d", want)
	}

	got, err := s.Deploy(ctx, ComponentSpec{Name: "api", Image: "img:latest", Ports: []runtime.PortMapping{auto(8080)}})
	if err != nil {
		t.Fatal(err)
	}
	if hostPort(got.Spec.Ports) != want {
		t.Errorf("deployed on %d, reserved %d", hostPort(got.Spec.Ports), want)
	}
	// Used up: the component holds the port now.
	if rs, _ := st.ListReservations(); len(rs) != 0 {
		t.Errorf("reservation left behind after the deploy: %+v", rs)
	}
	// Reserving a name that is deployed answers with the component.
	if c, reserved, _ := s.Reserve(ctx, spec); reserved || c.ID != got.ID {
		t.Errorf("reserve of a deployed name: reserved=%v id=%q", reserved, c.ID)
	}
}

// A replacement elsewhere must not take a reserved port either.
func TestReplaceSkipsReservedPorts(t *testing.T) {
	s, _ := reservingService(t)
	ctx := context.Background()
	web, err := s.Deploy(ctx, ComponentSpec{Name: "web", Image: "img:latest"})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Reserve(ctx, ComponentSpec{Name: "api", Ports: []runtime.PortMapping{auto(8080)}})
	if err != nil {
		t.Fatal(err)
	}
	// web gains an auto port on redeploy: the lowest free one is api's.
	spec, err := s.resolveAutoPortsFor(ComponentSpec{Name: "web", Ports: []runtime.PortMapping{auto(80)}}, web)
	if err != nil {
		t.Fatal(err)
	}
	if hostPort(spec.Ports) == hostPort(r.Spec.Ports) {
		t.Errorf("replacement was handed the reserved port %d", hostPort(r.Spec.Ports))
	}
}

func TestReleaseFreesTheReservation(t *testing.T) {
	s, st := reservingService(t)
	ctx := context.Background()
	if _, _, err := s.Reserve(ctx, ComponentSpec{Name: "api", Ports: []runtime.PortMapping{auto(8080)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "", "api"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := st.ListReservations(); len(rs) != 0 {
		t.Errorf("still reserved: %+v", rs)
	}
	if err := s.Release(ctx, "", "never"); err != nil {
		t.Errorf("releasing nothing: %v", err)
	}
}
