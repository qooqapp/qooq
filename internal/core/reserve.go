package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/pkg/client"
)

// Reservation sets host ports aside for a component that does not exist yet,
// so its address is known before it is deployed.
//
// It exists for components that reference each other: an api whose CORS list
// names the web app's URL, and a web app whose config names the api's. Each
// one's environment needs the other's address, and with "auto:" ports that
// address is only decided when the component is created — so neither can be
// deployed first, and both wait forever. An address is only an allocation; it
// does not need the component to be running. Reserving it when the deploy is
// first prepared breaks the loop.
//
// Spec holds what the address is computed from (name, namespace, ports with
// their host ports already assigned, ingress settings). It is never started:
// the reservation is consumed when a component of that name is created, which
// reuses the ports here exactly the way a redeploy reuses its own.
type Reservation struct {
	Spec      ComponentSpec `json:"spec"`
	CreatedAt time.Time     `json:"created_at"`
}

// Reserve returns the component spec's address-bearing fields as they will be
// once it is deployed: an existing component is returned as it is (reserved
// false), otherwise its auto ports are assigned now and kept (reserved true).
// Calling it again for the same name returns the same ports.
func (s *Service) Reserve(ctx context.Context, spec ComponentSpec) (comp *Component, reserved bool, err error) {
	if spec.Name == "" {
		return nil, false, errors.New("name is required")
	}
	spec.Namespace = client.NormalizeNamespace(spec.Namespace)
	if err := validateNaming(spec); err != nil {
		return nil, false, err
	}

	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	existing, err := s.store.ListComponents()
	if err != nil {
		return nil, false, err
	}
	for _, c := range existing {
		if sameIdentity(c.Spec, spec) && c.Status.Phase != runtime.PhaseRemoved {
			return c, false, nil
		}
	}
	all, err := s.store.ListReservations()
	if err != nil {
		return nil, false, err
	}
	var prev []runtime.PortMapping
	created := time.Now()
	if r := findReservation(all, spec); r != nil {
		prev, created = r.Spec.Ports, r.CreatedAt
	}
	taken := takenHostPorts(existing, "")
	addReservedPorts(taken, all, spec)
	ports, err := resolveAutoPorts(spec.Ports, prev, taken, s.autoPortLow, s.autoPortHigh)
	if err != nil {
		return nil, false, err
	}
	keep := ComponentSpec{
		Namespace: spec.Namespace,
		Name:      spec.Name,
		Ports:     ports,
		Network:   spec.Network,
		Domain:    spec.Domain,
		Ingress:   spec.Ingress,
	}
	if err := s.store.SaveReservation(&Reservation{Spec: keep, CreatedAt: created}); err != nil {
		return nil, false, err
	}
	return &Component{Spec: keep}, true, nil
}

// Reservations lists the reservations not yet consumed by a deploy.
func (s *Service) Reservations(ctx context.Context) ([]*Reservation, error) {
	return s.store.ListReservations()
}

// Release drops a reservation; releasing one that does not exist is not an
// error, so a target removed before it ever deployed can always call it.
func (s *Service) Release(ctx context.Context, namespace, name string) error {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	return s.store.DeleteReservation(client.NormalizeNamespace(namespace), name)
}

func findReservation(all []*Reservation, spec ComponentSpec) *Reservation {
	for _, r := range all {
		if sameIdentity(r.Spec, spec) {
			return r
		}
	}
	return nil
}

// addReservedPorts marks the ports reserved for any component other than
// self as taken: a reservation is a promise, and handing its port to someone
// else breaks the address another app already has baked into its config.
func addReservedPorts(taken map[int]bool, all []*Reservation, self ComponentSpec) {
	for _, r := range all {
		if sameIdentity(r.Spec, self) {
			continue
		}
		for _, p := range r.Spec.Ports {
			if p.HostPort != 0 {
				taken[p.HostPort] = true
			}
		}
	}
}

// reservedFor is the ports reserved for spec and the full reservation list.
// A store error is returned: guessing "nothing reserved" would hand out a
// promised port.
func (s *Service) reservedFor(spec ComponentSpec) ([]runtime.PortMapping, []*Reservation, error) {
	all, err := s.store.ListReservations()
	if err != nil {
		return nil, nil, fmt.Errorf("list reservations: %w", err)
	}
	if r := findReservation(all, spec); r != nil {
		return r.Spec.Ports, all, nil
	}
	return nil, all, nil
}
