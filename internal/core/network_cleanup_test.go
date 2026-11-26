package core

import (
	"context"
	"testing"
)

// Removing a component also cleans up the network it was created on.
//
// Every deploy with a network ensures one, and nothing ever removed it: after
// enough deploy-and-remove cycles docker ran out of address pools and the next
// deploy failed with "all predefined address pools have been fully subnetted"
// — 26 empty qooq networks on the machine where it was found. The runtime
// decides whether the network can go (only one qooq created, only when no
// container is left on it); core's job is to ask.
func TestRemoveAsksToCleanUpTheComponentsNetwork(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestService(rt, newFakeStore())
	c, err := s.Deploy(context.Background(), ComponentSpec{Name: "web", Image: "img:v1", Network: "qooq-web"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if err := s.Remove(context.Background(), c.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := rt.callArgs("RemoveNetworkIfUnused"); len(got) != 1 || got[0] != "qooq-web" {
		t.Errorf("network cleanup calls = %v, want [qooq-web]", got)
	}
}

func TestRemoveWithoutANetworkTouchesNoNetwork(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestService(rt, newFakeStore())
	c, err := s.Deploy(context.Background(), ComponentSpec{Name: "web", Image: "img:v1"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if err := s.Remove(context.Background(), c.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if n := rt.callCount("RemoveNetworkIfUnused"); n != 0 {
		t.Errorf("RemoveNetworkIfUnused called %d times for a component with no network", n)
	}
}
