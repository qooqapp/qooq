package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/qooqapp/qooq/internal/runtime"
)

// HealthCheck probe types.
const (
	HealthCheckHTTP = "http"
	HealthCheckExec = "exec"
)

// Defaults applied to any unset (zero-valued) numeric HealthCheck field at
// Deploy time, matching the CPULimit/MemLimitBytes convention in this package
// where 0 means "unset".
const (
	defaultHealthIntervalSeconds     = 10
	defaultHealthTimeoutSeconds      = 5
	defaultHealthFailureThreshold    = 3
	defaultHealthInitialDelaySeconds = 5
)

// ErrInvalidHealthCheck is returned by Deploy when HealthCheck.Type is not
// "http"/"exec", or the fields required by that type are missing.
var ErrInvalidHealthCheck = errors.New(`health_check: type must be "http" (with http_path and http_port) or "exec" (with exec_command)`)

// HealthCheck configures liveness probing for a component. A consecutive run
// of FailureThreshold probe failures triggers a restart via
// Service.EvaluateRestart(reason=RestartReasonHealth) — the same recreate
// path used for crash/drift, just a different trigger source. Probing itself
// runs in internal/monitor (a goroutine per running container), not here.
//
// v1 limitation (accepted): the "http" type is probed from qooqd's own
// network namespace against the component's published host port, so it only
// works for components with a HostPort mapping. Components without one must
// use "exec" instead.
type HealthCheck struct {
	Type                string   `json:"type"` // "http" or "exec"
	HTTPPath            string   `json:"http_path,omitempty"`
	HTTPPort            int      `json:"http_port,omitempty"` // published host port
	ExecCommand         []string `json:"exec_command,omitempty"`
	IntervalSeconds     int      `json:"interval_seconds,omitempty"`
	TimeoutSeconds      int      `json:"timeout_seconds,omitempty"`
	FailureThreshold    int      `json:"failure_threshold,omitempty"`
	InitialDelaySeconds int      `json:"initial_delay_seconds,omitempty"`
}

// normalizeHealthCheck validates hc and returns a copy with unset numeric
// fields filled in from the package defaults. nil in, nil out (no health
// check configured is valid). The caller's value is never mutated.
func normalizeHealthCheck(hc *HealthCheck, ports []runtime.PortMapping) (*HealthCheck, error) {
	if hc == nil {
		return nil, nil
	}
	out := *hc
	switch out.Type {
	case HealthCheckHTTP:
		if out.HTTPPath == "" || out.HTTPPort == 0 {
			return nil, ErrInvalidHealthCheck
		}
		// The probe URL is built as "http://127.0.0.1:<port>" + HTTPPath, so
		// a path that does not start with "/" decides the host: "@evil.example/"
		// turns it into a GET from this machine to evil.example, and "//x"
		// is a scheme-relative reference to the same effect. Only deploys are
		// checked: a component stored before this check with a bad path is
		// left alone (rejecting it at boot reconcile would take the daemon
		// down for an old record), and any new deploy of it is refused.
		if !strings.HasPrefix(out.HTTPPath, "/") || strings.HasPrefix(out.HTTPPath, "//") {
			return nil, fmt.Errorf("%w: http_path %q must start with a single \"/\" (a path on this component, not a host)",
				ErrInvalidHealthCheck, out.HTTPPath)
		}
		// The probe port has to be one of this component's own ports.
		//
		// A port belonging to nothing here is the worst possible mistake to
		// accept: the probe falls back to 127.0.0.1:<that port>, which sooner
		// or later belongs to a *different* component — so a broken app is
		// checked against a healthy stranger, reports itself healthy, and is
		// never restarted. Nothing about the symptom points at the cause.
		if !portDeclared(ports, out.HTTPPort) {
			return nil, fmt.Errorf("%w: health check port %d is not one of this component's ports (%s)",
				ErrInvalidHealthCheck, out.HTTPPort, describePorts(ports))
		}
	case HealthCheckExec:
		if len(out.ExecCommand) == 0 {
			return nil, ErrInvalidHealthCheck
		}
	default:
		return nil, ErrInvalidHealthCheck
	}
	if out.IntervalSeconds <= 0 {
		out.IntervalSeconds = defaultHealthIntervalSeconds
	}
	if out.TimeoutSeconds <= 0 {
		out.TimeoutSeconds = defaultHealthTimeoutSeconds
	}
	if out.FailureThreshold <= 0 {
		out.FailureThreshold = defaultHealthFailureThreshold
	}
	if out.InitialDelaySeconds <= 0 {
		out.InitialDelaySeconds = defaultHealthInitialDelaySeconds
	}
	return &out, nil
}

// portDeclared reports whether port is one this component publishes or exposes.
// Either side counts: naming the container port is the spelling that keeps
// working when the host port is chosen by the daemon, and naming the host port
// is what manifests written before that existed say.
func portDeclared(ports []runtime.PortMapping, port int) bool {
	for _, p := range ports {
		if p.ContainerPort == port || (p.HostPort != 0 && p.HostPort == port) {
			return true
		}
	}
	return false
}

// describePorts lists what was available, since the usual cause is a number
// copied from somewhere it no longer applies.
func describePorts(ports []runtime.PortMapping) string {
	if len(ports) == 0 {
		return "it declares none"
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		switch {
		case p.AutoHost:
			out = append(out, fmt.Sprintf("auto:%d", p.ContainerPort))
		case p.HostPort != 0:
			out = append(out, fmt.Sprintf("%d:%d", p.HostPort, p.ContainerPort))
		default:
			out = append(out, strconv.Itoa(p.ContainerPort))
		}
	}
	return strings.Join(out, ", ")
}
