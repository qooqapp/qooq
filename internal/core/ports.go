package core

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Default range for automatically assigned host ports. Deliberately well above
// the ports an app picks for itself and clear of the ephemeral range the
// kernel hands to outgoing connections, so an assignment does not collide with
// something that was never asked about.
const (
	DefaultAutoPortLow  = 19000
	DefaultAutoPortHigh = 19999
)

// ErrNoFreePort means every port in the configured range is spoken for. It is
// a configuration problem, not a server fault, so the API reports it as a
// conflict with the range named rather than a 500.
var ErrNoFreePort = errors.New("no free host port")

// SetAutoPortRange overrides the range AutoHost ports are drawn from. Called
// once at startup, before any request is served.
func (s *Service) SetAutoPortRange(low, high int) error {
	if low < 1 || high > 65535 || low > high {
		return fmt.Errorf("auto port range %d-%d is not a valid range within 1-65535", low, high)
	}
	s.autoPortLow, s.autoPortHigh = low, high
	return nil
}

// SetPublishIP sets the host address published ports bind to when a spec does
// not name one; "" keeps the engine's 0.0.0.0. On a machine whose apps are
// reached through the ingress, 127.0.0.1 stops one user's container from
// reaching another's published port through the bridge gateway.
// Called once at startup.
func (s *Service) SetPublishIP(ip string) error {
	if ip != "" && net.ParseIP(ip) == nil {
		return fmt.Errorf("publish address %q is not an IP address", ip)
	}
	s.publishIP = ip
	return nil
}

// withPublishIP fills in the publish address on every published port that
// has none, in a copy.
func (s *Service) withPublishIP(ports []runtime.PortMapping) []runtime.PortMapping {
	if s.publishIP == "" || len(ports) == 0 {
		return ports
	}
	out := make([]runtime.PortMapping, len(ports))
	copy(out, ports)
	for i := range out {
		if out[i].HostPort != 0 && out[i].HostIP == "" {
			out[i].HostIP = s.publishIP
		}
	}
	return out
}

// resolveAutoPorts turns every port marked AutoHost into a concrete host port.
//
// The rule that matters is stability: a component keeps the port it was given.
// An address that changes on redeploy is worse than no address at all, because
// everything that referred to it — another app's configuration, a bookmark, a
// CORS allowlist — keeps pointing at the old one and fails somewhere far from
// the change. So a port already assigned for the same container port is reused,
// and allocation only happens for a mapping that has never had one.
//
// prev is the spec being replaced, or nil for a brand-new component. taken
// lists host ports already spoken for by other components; the caller supplies
// it because it holds the lock that makes that list meaningful.
func resolveAutoPorts(ports []runtime.PortMapping, prev []runtime.PortMapping, taken map[int]bool, low, high int) ([]runtime.PortMapping, error) {
	out := make([]runtime.PortMapping, len(ports))
	copy(out, ports)

	// Ports this same spec pins explicitly are not available to its own auto
	// ports either — a manifest asking for 8080:80 and auto:81 must not be
	// handed 8080 for the second one.
	local := make(map[int]bool, len(out))
	for _, p := range out {
		if !p.AutoHost && p.HostPort != 0 {
			local[p.HostPort] = true
		}
	}

	for i, p := range out {
		if !p.AutoHost {
			continue
		}
		if reused := previousHostPort(prev, p); reused != 0 {
			out[i].HostPort, out[i].AutoHost = reused, false
			local[reused] = true
			continue
		}
		port, err := allocateHostPort(taken, local, low, high)
		if err != nil {
			return nil, fmt.Errorf("%w in %d-%d for container port %d", err, low, high, p.ContainerPort)
		}
		out[i].HostPort, out[i].AutoHost = port, false
		local[port] = true
	}
	return out, nil
}

// previousHostPort finds the host port this component already had for the same
// container port and protocol, or 0. Matching on the container port is what
// makes reuse survive a manifest being reordered or gaining another port.
func previousHostPort(prev []runtime.PortMapping, want runtime.PortMapping) int {
	for _, p := range prev {
		if p.ContainerPort == want.ContainerPort && sameProto(p.Protocol, want.Protocol) && p.HostPort != 0 {
			return p.HostPort
		}
	}
	return 0
}

func sameProto(a, b string) bool {
	if a == "" {
		a = "tcp"
	}
	if b == "" {
		b = "tcp"
	}
	return a == b
}

// allocateHostPort picks the lowest port in the range that no component claims
// and that nothing on this host is listening on.
//
// Checking the host as well as the store is what keeps qooq from handing out a
// port some unrelated program already holds: the store only knows about
// components. The check is a moment in time and something could take the port
// between here and the container starting — that race is why a failed bind is
// reported as ErrPortConflict naming the port, rather than a generic failure.
func allocateHostPort(taken, local map[int]bool, low, high int) (int, error) {
	for port := low; port <= high; port++ {
		if taken[port] || local[port] {
			continue
		}
		if !hostPortFree(port) {
			continue
		}
		return port, nil
	}
	return 0, ErrNoFreePort
}

// hostPortFree asks the host, specifically over IPv4 on 0.0.0.0.
//
// The obvious spelling — net.Listen("tcp", ":port") — is wrong here, and
// silently so. It binds the IPv6 wildcard, which on macOS succeeds even while
// a docker port publisher holds the same port on IPv4: every published port on
// the machine reports as free. 0.0.0.0 is also exactly where a published port
// lands ("0.0.0.0:19000->8080/tcp"), so this asks the same question the
// container will.
func hostPortFree(port int) bool {
	ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// takenHostPorts is every host port claimed by a component other than exclude.
// A removed component holds nothing.
func takenHostPorts(comps []*Component, exclude string) map[int]bool {
	taken := make(map[int]bool)
	for _, c := range comps {
		if c.ID == exclude || c.Status.Phase == runtime.PhaseRemoved {
			continue
		}
		for _, p := range c.Spec.Ports {
			if p.HostPort != 0 {
				taken[p.HostPort] = true
			}
		}
	}
	return taken
}

// hasAutoPort reports whether a spec needs resolution at all, so the common
// case — no auto ports — costs nothing.
func hasAutoPort(ports []runtime.PortMapping) bool {
	for _, p := range ports {
		if p.AutoHost {
			return true
		}
	}
	return false
}
