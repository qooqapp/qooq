package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

func TestNormalizeHealthCheckNilPassThrough(t *testing.T) {
	got, err := normalizeHealthCheck(nil, nil)
	if err != nil {
		t.Fatalf("nil in should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("nil in should give nil out, got %+v", got)
	}
}

func TestNormalizeHealthCheckInvalid(t *testing.T) {
	cases := []struct {
		name string
		hc   HealthCheck
	}{
		{"unknown type", HealthCheck{Type: "tcp"}},
		{"empty type", HealthCheck{Type: ""}},
		{"http missing path", HealthCheck{Type: HealthCheckHTTP, HTTPPort: 8080}},
		{"http missing port", HealthCheck{Type: HealthCheckHTTP, HTTPPath: "/healthz"}},
		{"http path is a host", HealthCheck{Type: HealthCheckHTTP, HTTPPath: "@evil.example/", HTTPPort: 8080}},
		{"http path without slash", HealthCheck{Type: HealthCheckHTTP, HTTPPath: "healthz", HTTPPort: 8080}},
		{"http path scheme-relative", HealthCheck{Type: HealthCheckHTTP, HTTPPath: "//evil.example/", HTTPPort: 8080}},
		{"exec empty command", HealthCheck{Type: HealthCheckExec}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := c.hc
			got, err := normalizeHealthCheck(&hc, declared8080)
			if !errors.Is(err, ErrInvalidHealthCheck) {
				t.Fatalf("want ErrInvalidHealthCheck, got err=%v", err)
			}
			if got != nil {
				t.Fatalf("invalid input should give nil out, got %+v", got)
			}
		})
	}
}

func TestNormalizeHealthCheckDefaults(t *testing.T) {
	in := HealthCheck{Type: HealthCheckHTTP, HTTPPath: "/healthz", HTTPPort: 8080}
	got, err := normalizeHealthCheck(&in, declared8080)
	if err != nil {
		t.Fatalf("valid http check should not error: %v", err)
	}
	want := HealthCheck{
		Type:                HealthCheckHTTP,
		HTTPPath:            "/healthz",
		HTTPPort:            8080,
		IntervalSeconds:     defaultHealthIntervalSeconds,
		TimeoutSeconds:      defaultHealthTimeoutSeconds,
		FailureThreshold:    defaultHealthFailureThreshold,
		InitialDelaySeconds: defaultHealthInitialDelaySeconds,
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("defaults not applied:\n got %+v\nwant %+v", *got, want)
	}
	// The caller's value must never be mutated.
	if in.IntervalSeconds != 0 {
		t.Fatalf("input was mutated: IntervalSeconds=%d", in.IntervalSeconds)
	}
}

func TestNormalizeHealthCheckKeepsExplicitValues(t *testing.T) {
	in := HealthCheck{
		Type:                HealthCheckExec,
		ExecCommand:         []string{"true"},
		IntervalSeconds:     30,
		TimeoutSeconds:      15,
		FailureThreshold:    7,
		InitialDelaySeconds: 20,
	}
	got, err := normalizeHealthCheck(&in, nil)
	if err != nil {
		t.Fatalf("valid exec check should not error: %v", err)
	}
	if !reflect.DeepEqual(*got, in) {
		t.Fatalf("explicit values should be preserved:\n got %+v\nwant %+v", *got, in)
	}
}

// declared8080 is a component that exposes 8080 with the host port left to the
// daemon — the shape the auto-port work made ordinary.
var declared8080 = []runtime.PortMapping{{ContainerPort: 8080, AutoHost: true}}

// A probe port belonging to no port of this component is the worst mistake to
// accept quietly: the probe falls back to 127.0.0.1:<port>, which sooner or
// later is a different component, so a broken app is checked against a healthy
// stranger and never restarted.
func TestNormalizeHealthCheckRejectsAPortThisComponentDoesNotHave(t *testing.T) {
	hc := HealthCheck{Type: HealthCheckHTTP, HTTPPath: "/healthz", HTTPPort: 19000}
	got, err := normalizeHealthCheck(&hc, declared8080)
	if !errors.Is(err, ErrInvalidHealthCheck) {
		t.Fatalf("want ErrInvalidHealthCheck, got %v", err)
	}
	if got != nil {
		t.Fatalf("invalid input should give nil out, got %+v", got)
	}
	// The usual cause is a number copied from somewhere it no longer applies,
	// so the error has to say what this component does have.
	if !strings.Contains(err.Error(), "auto:8080") {
		t.Errorf("error should list the available ports, got %q", err)
	}
}

// Both spellings are accepted: the container port, which keeps working when
// the host port is chosen by the daemon, and a published host port, which is
// what manifests written before auto ports say.
func TestNormalizeHealthCheckAcceptsEitherSide(t *testing.T) {
	ports := []runtime.PortMapping{{ContainerPort: 8080, HostPort: 19000}}
	for _, port := range []int{8080, 19000} {
		hc := HealthCheck{Type: HealthCheckHTTP, HTTPPath: "/healthz", HTTPPort: port}
		if _, err := normalizeHealthCheck(&hc, ports); err != nil {
			t.Errorf("port %d rejected: %v", port, err)
		}
	}
}

// An exec probe has no port, so the ports a component declares are irrelevant
// to it — a component with none must still be able to use one.
func TestNormalizeHealthCheckExecNeedsNoPorts(t *testing.T) {
	hc := HealthCheck{Type: HealthCheckExec, ExecCommand: []string{"true"}}
	if _, err := normalizeHealthCheck(&hc, nil); err != nil {
		t.Errorf("exec check with no ports rejected: %v", err)
	}
}
