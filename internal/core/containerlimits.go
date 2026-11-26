package core

import "github.com/qooqapp/qooq/internal/runtime"

// DefaultContainerPidsLimit is the pids cap applied to every container unless
// the operator says otherwise. It is a fork-bomb guard: without one, a single
// user's runaway process table can exhaust the host's pid space and take
// every other user down with it. 2048 is far above what a normal app needs,
// but note that the kernel counts threads as pids, so a thread-per-request
// server with thousands of live threads is the one thing that can notice it.
const DefaultContainerPidsLimit = 2048

// ContainerLimits bounds what any one container may consume.
//
// Machine policy, like BuildLimits: the operator of the host decides it. The
// values a component's own spec carries come from the user's manifest, and a
// ceiling the user can set is not a ceiling.
type ContainerLimits struct {
	// PidsLimit caps processes plus threads per container. Zero is unlimited.
	PidsLimit int64
	// MemoryMaxBytes and CPUsMax are ceilings. A spec that asks for nothing
	// gets the ceiling; a spec that asks for more is clamped to it; a spec
	// that asks for less keeps its own value. Zero means no ceiling, which is
	// the right default for a machine running only its operator's own code.
	MemoryMaxBytes int64
	CPUsMax        float64
	// Harden turns on the runtime's container hardening for every container
	// (runtime.ContainerSpec.Harden). Off by default: on a machine running
	// only its operator's own code it buys little and can break an app that
	// pings, uses SCTP, or relies on a setuid binary.
	Harden bool
}

// SetContainerLimits installs the machine's per-container policy.
func (s *Service) SetContainerLimits(l ContainerLimits) { s.containerLimits = l }

// applyContainerLimits folds the machine policy into the resource fields of a
// ContainerSpec about to be created.
//
// It acts on the runtime spec, never on the stored ComponentSpec: the store
// keeps what was asked for, the container gets what policy allows, and loosening
// the flags later takes effect on the next create without a redeploy.
// Every create goes through createAndStart, so restart-manager and reconcile
// rebuilds are covered too. The effective values are visible on the container
// itself (docker inspect), not on `component get`, which shows the request.
func (s *Service) applyContainerLimits(name string, cspec *runtime.ContainerSpec) {
	l := s.containerLimits
	cspec.PidsLimit = l.PidsLimit
	cspec.Harden = l.Harden

	if l.MemoryMaxBytes > 0 {
		switch {
		case cspec.MemBytes == 0:
			cspec.MemBytes = l.MemoryMaxBytes
		case cspec.MemBytes > l.MemoryMaxBytes:
			s.log.Warn("component memory limit exceeds the machine ceiling; clamping",
				"component", name, "requested_bytes", cspec.MemBytes, "ceiling_bytes", l.MemoryMaxBytes)
			cspec.MemBytes = l.MemoryMaxBytes
		}
	}
	if l.CPUsMax > 0 {
		max := int64(l.CPUsMax * 1e9)
		switch {
		case cspec.CPUNanos == 0:
			cspec.CPUNanos = max
		case cspec.CPUNanos > max:
			s.log.Warn("component cpu limit exceeds the machine ceiling; clamping",
				"component", name, "requested_nanos", cspec.CPUNanos, "ceiling_nanos", max)
			cspec.CPUNanos = max
		}
	}
}
