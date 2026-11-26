package monitor

import (
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

// record every outcome of a run of probe results, so a test reads as the story
// the checker lived through.
func run(t *healthTracker, results ...bool) []healthOutcome {
	out := make([]healthOutcome, 0, len(results))
	for _, ok := range results {
		out = append(out, t.record(ok))
	}
	return out
}

func want(t *testing.T, got []healthOutcome, expect ...healthOutcome) {
	t.Helper()
	if len(got) != len(expect) {
		t.Fatalf("got %v outcomes, want %v", got, expect)
	}
	for i := range got {
		if got[i] != expect[i] {
			t.Fatalf("outcome %d: got %v, want %v (full: %v)", i, got[i], expect[i], got)
		}
	}
}

// The case this rule exists for: a probe that never worked must not restart the
// component, however long it goes on failing.
func TestNeverHealthyNeverRestarts(t *testing.T) {
	tr := &healthTracker{threshold: 3}
	got := run(tr, false, false, false, false, false, false, false)
	want(t, got,
		outcomeFailure, outcomeFailure, outcomeUnreachable,
		outcomeOK, outcomeOK, outcomeOK, outcomeOK)
}

// Once the probe has proved it can see the container, the old behaviour is
// exactly what should happen.
func TestRestartsOnceTheProbeHasWorked(t *testing.T) {
	tr := &healthTracker{threshold: 3}
	got := run(tr, true, false, false, false)
	want(t, got, outcomeFirstPass, outcomeFailure, outcomeFailure, outcomeRestart)
}

// A component that becomes reachable later — a slow first start, an ingress
// route that attaches on the next sync tick — arms itself with no intervention.
func TestUnreachableThenReachableArmsRestarts(t *testing.T) {
	tr := &healthTracker{threshold: 2}
	got := run(tr, false, false, false, true, false, false)
	want(t, got,
		outcomeFailure, outcomeUnreachable, outcomeOK,
		outcomeRecovered,
		outcomeFailure, outcomeRestart)
}

// Recovery is reported only when there was something to recover from: an
// ordinary passing probe is silent.
func TestRecoveredOnlyAfterAWarning(t *testing.T) {
	tr := &healthTracker{threshold: 3}
	got := run(tr, false, true, false, true)
	want(t, got, outcomeFailure, outcomeFirstPass, outcomeFailure, outcomeOK)
}

// threshold 1 leaves no room between "failed" and "acted on", which is where an
// off-by-one would hide.
func TestThresholdOne(t *testing.T) {
	tr := &healthTracker{threshold: 1}
	want(t, run(tr, false, false), outcomeUnreachable, outcomeOK)

	armed := &healthTracker{threshold: 1}
	want(t, run(armed, true, false), outcomeFirstPass, outcomeRestart)
}

// The two http failures are indistinguishable at the socket and have opposite
// fixes, so the message has to name the likely cause to be worth anything.
func TestUnreachableDetailNamesTheLikelyCause(t *testing.T) {
	hc := &core.HealthCheck{Type: core.HealthCheckHTTP, HTTPPath: "/healthz", HTTPPort: 8080}

	container := unreachableDetail(httpProbeTarget{containerPort: 8080}, hc)
	if !strings.Contains(container, "Docker Desktop") || !strings.Contains(container, "exec probe") {
		t.Errorf("container-address detail should point at the Docker Desktop case and the way out, got: %s", container)
	}

	host := unreachableDetail(httpProbeTarget{hostPort: 19999}, hc)
	if !strings.Contains(host, "19999") || strings.Contains(host, "Docker Desktop") {
		t.Errorf("host-port detail should be about the published port, got: %s", host)
	}

	exec := unreachableDetail(httpProbeTarget{}, &core.HealthCheck{Type: core.HealthCheckExec, ExecCommand: []string{"true"}})
	if !strings.Contains(exec, "exec") {
		t.Errorf("exec detail should be about the command, got: %s", exec)
	}
}

func TestProbeAddressMatchesWhatTheProberDoes(t *testing.T) {
	hc := &core.HealthCheck{Type: core.HealthCheckHTTP, HTTPPath: "/healthz"}
	if got := probeAddress(httpProbeTarget{hostPort: 8080}, hc); got != "http://127.0.0.1:8080/healthz" {
		t.Errorf("host port: got %s", got)
	}
	if got := probeAddress(httpProbeTarget{containerPort: 3000}, hc); got != "http://<container-ip>:3000/healthz" {
		t.Errorf("container port: got %s", got)
	}
}

// Both sides are reported whenever both are known: where to connect and what
// the app has to be listening on are different facts, and a message about the
// app's own behaviour has to name the port the app can see — the host port is
// a number it never hears about.
func TestResolveHTTPProbeTarget(t *testing.T) {
	tests := []struct {
		name     string
		ports    []runtime.PortMapping
		probe    int
		wantHost int
		wantCtr  int
	}{
		{
			// The pre-ingress case, and the one health.port's documentation
			// describes: the operator wrote the published host port.
			name:     "matches a published host port",
			ports:    []runtime.PortMapping{{ContainerPort: 80, HostPort: 19000}},
			probe:    19000,
			wantHost: 19000,
			wantCtr:  80,
		},
		{
			// The case that ingress makes normal: nothing is published, so the
			// only address that exists is the container's own.
			name:    "matches a container-only port",
			ports:   []runtime.PortMapping{{ContainerPort: 8080}},
			probe:   8080,
			wantCtr: 8080,
		},
		{
			// Naming the container port of a published mapping should still
			// probe the host port: it is reachable, cheaper, and needs no
			// inspect call.
			name:     "container port of a published mapping prefers the host port",
			ports:    []runtime.PortMapping{{ContainerPort: 80, HostPort: 19000}},
			probe:    80,
			wantHost: 19000,
			wantCtr:  80,
		},
		{
			// Components deployed before any of this exists may name a host
			// port that was never in the spec. Changing what those mean would
			// restart working apps, so an unmatched port keeps its old meaning.
			name:     "unmatched port keeps the historical loopback meaning",
			ports:    []runtime.PortMapping{{ContainerPort: 80, HostPort: 19000}},
			probe:    3000,
			wantHost: 3000,
		},
		{
			name:     "no ports at all keeps the historical meaning",
			ports:    nil,
			probe:    8080,
			wantHost: 8080,
		},
		{
			// Host-port matching wins over container-port matching even when a
			// later mapping's container port collides with an earlier host
			// port, because the host port is the reachable one.
			name: "host port match wins over a later container port collision",
			ports: []runtime.PortMapping{
				{ContainerPort: 3000, HostPort: 8080},
				{ContainerPort: 8080},
			},
			probe:    8080,
			wantHost: 8080,
			wantCtr:  3000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveHTTPProbeTarget(tc.ports, &core.HealthCheck{HTTPPort: tc.probe})
			if got.hostPort != tc.wantHost || got.containerPort != tc.wantCtr {
				t.Errorf("resolveHTTPProbeTarget() = {host:%d container:%d}, want {host:%d container:%d}",
					got.hostPort, got.containerPort, tc.wantHost, tc.wantCtr)
			}
		})
	}
}

// The unreachable message is the only thing an operator gets for an app whose
// probe has never worked, so it has to name the cause that costs the most time
// to find on one's own: an app that is not listening where its manifest says.
func TestUnreachableDetailNamesTheContainerPort(t *testing.T) {
	target := resolveHTTPProbeTarget(
		[]runtime.PortMapping{{ContainerPort: 8080, HostPort: 19000}},
		&core.HealthCheck{HTTPPort: 8080, HTTPPath: "/healthz"},
	)
	got := unreachableDetail(target, &core.HealthCheck{HTTPPort: 8080, HTTPPath: "/healthz"})

	for _, want := range []string{"127.0.0.1:19000", "8080", "/healthz", "config"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail should mention %q, got: %s", want, got)
		}
	}
	// The port cannot be wrong any more — core rejects a health check naming a
	// port the component does not declare — so suggesting it wastes the reader's
	// time on something that cannot be the cause.
	if strings.Contains(got, "names a published host port") {
		t.Errorf("detail still suggests a cause validation rules out: %s", got)
	}
}

// The first success is reported distinctly from every one after it, because
// that transition is the only moment worth a store write: it is what turns
// "running" into "ready" for anything waiting on this component. Reporting it
// on every probe instead would write to the store forever for a value that
// never changes again.
func TestFirstPassIsReportedOnceAndOnlyOnce(t *testing.T) {
	tr := &healthTracker{threshold: 3}
	want(t, run(tr, true, true, true), outcomeFirstPass, outcomeOK, outcomeOK)
}

// A probe that fails before it ever passes must still report its first
// success: a slow-starting database fails a probe or two on the way up, and
// treating those as "already reported" would leave it never marked ready.
func TestFirstPassSurvivesEarlyFailures(t *testing.T) {
	tr := &healthTracker{threshold: 3}
	want(t, run(tr, false, false, true, true),
		outcomeFailure, outcomeFailure, outcomeFirstPass, outcomeOK)
}

// After an unreachable verdict the first success is a recovery, not a first
// pass — the caller acts on both, but only one of them says restarts just got
// armed, and collapsing them would lose that log line.
func TestRecoveryOutranksFirstPass(t *testing.T) {
	tr := &healthTracker{threshold: 1}
	want(t, run(tr, false, true), outcomeUnreachable, outcomeRecovered)
}
