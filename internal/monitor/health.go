package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

// startHealthCheck opens a liveness-probing goroutine for a container whose
// component has a HealthCheck configured, unless one already runs for it.
// Mirrors startStream's per-container goroutine model (M2), but keyed off
// the component's own spec rather than the runtime's stats API.
func (m *Monitor) startHealthCheck(containerID runtime.ContainerID, componentID string) {
	comp, err := m.svc.Get(m.ctx, componentID)
	if err != nil || comp.Spec.HealthCheck == nil {
		return
	}
	hc := comp.Spec.HealthCheck

	m.healthMu.Lock()
	if _, exists := m.healthChecks[containerID]; exists {
		m.healthMu.Unlock()
		return
	}
	hctx, cancel := context.WithCancel(m.ctx)
	h := &healthHandle{cancel: cancel}
	m.healthChecks[containerID] = h
	m.healthMu.Unlock()

	target := resolveHTTPProbeTarget(comp.Spec.Ports, hc)
	go m.runHealthCheck(hctx, containerID, componentID, hc, target, h)
	m.log.Info("health checker started",
		"container", containerID, "component", componentID, "type", hc.Type,
		"interval_s", hc.IntervalSeconds, "timeout_s", hc.TimeoutSeconds,
		"failure_threshold", hc.FailureThreshold, "initial_delay_s", hc.InitialDelaySeconds,
		"probe_host_port", target.hostPort, "probe_container_port", target.containerPort)
}

// healthHandle is the map value for an in-flight health checker. Mirrors
// streamHandle in monitor.go and exists for the same reason: a
// context.CancelFunc cannot be compared with == to tell "am I still the
// current registration" apart from "someone already replaced me", but a
// pointer to this wrapper can.
type healthHandle struct {
	cancel context.CancelFunc
}

// stopHealthCheck cancels a container's health-check goroutine if present,
// and removes its registration synchronously — see stopStream in monitor.go
// for why relying on the cancelled goroutine's own defer to do that later is
// the bug: a fast external stop+start on the same container ID could find the
// stale entry still there and silently skip starting a fresh checker, losing
// both stats-independent liveness probing and its restart protection for that
// component with nothing logged anywhere to explain why.
func (m *Monitor) stopHealthCheck(containerID runtime.ContainerID) {
	m.healthMu.Lock()
	h, ok := m.healthChecks[containerID]
	if ok {
		delete(m.healthChecks, containerID)
	}
	m.healthMu.Unlock()
	if ok {
		h.cancel()
	}
}

// runHealthCheck waits InitialDelaySeconds, then probes on IntervalSeconds
// ticks until either ctx is cancelled (container stopped/died — a fresh
// checker will attach to whatever container ends up running next) or
// FailureThreshold consecutive probes fail, at which point it triggers a
// restart and exits: the recreated container's own "start" event will spin up
// a brand new checker, so this goroutine's job is done either way.
func (m *Monitor) runHealthCheck(ctx context.Context, containerID runtime.ContainerID, componentID string, hc *core.HealthCheck, target httpProbeTarget, h *healthHandle) {
	defer func() {
		m.healthMu.Lock()
		// Delete only if this goroutine's own handle is still current — see
		// streamStats's identical guard in monitor.go for why an
		// unconditional delete here would be wrong: stopHealthCheck now
		// deletes synchronously on cancel, so a fresh checker may already be
		// registered under this containerID by the time this runs.
		if m.healthChecks[containerID] == h {
			delete(m.healthChecks, containerID)
		}
		m.healthMu.Unlock()
	}()

	prober := &healthProber{m: m, containerID: containerID, hc: hc, target: target}

	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(hc.InitialDelaySeconds) * time.Second):
	}

	ticker := time.NewTicker(time.Duration(hc.IntervalSeconds) * time.Second)
	defer ticker.Stop()

	tracker := &healthTracker{threshold: hc.FailureThreshold}
	for {
		switch tracker.record(prober.probe(ctx)) {
		case outcomeFailure:
			m.log.Warn("health probe failed",
				"component", componentID, "container", containerID,
				"failures", tracker.failures, "threshold", hc.FailureThreshold)

		case outcomeRestart:
			m.log.Warn("health check failure threshold reached, triggering restart",
				"component", componentID, "container", containerID)
			m.svc.EvaluateRestart(m.ctx, componentID, core.RestartReasonHealth)
			return

		case outcomeUnreachable:
			detail := unreachableDetail(target, hc)
			m.log.Error("health check has never succeeded; not restarting this component",
				"component", componentID, "container", containerID,
				"probe", probeAddress(target, hc), "detail", detail)
			m.svc.NoteHealthUnreachable(m.ctx, componentID, detail)

		case outcomeRecovered:
			m.log.Info("health probe reachable now; restarts re-enabled for this component",
				"component", componentID, "container", containerID)
			m.svc.NoteHealthy(m.ctx, componentID, string(containerID))

		case outcomeFirstPass:
			// The moment the component became ready rather than merely
			// running. Recorded on the component so a caller waiting to
			// deploy against it can see it.
			m.svc.NoteHealthy(m.ctx, componentID, string(containerID))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// healthTracker decides what a checker should do about each probe result.
//
// The rule it exists for: a probe that has never once succeeded is not evidence
// that the app is broken — far more often it is evidence that the probe cannot
// see the app (health.port naming a port nothing listens on, or a container-only
// port on a docker host whose container network the daemon cannot route to,
// which is the normal case on Docker Desktop). Restarting on that verdict
// recreates a working container forever while never changing the outcome, and
// the operator sees an app that "keeps restarting" with nothing in its own logs
// to explain why.
//
// So restarts are armed only after the probe has proved once that it can
// observe this container. Probing continues either way, so a component that
// becomes reachable later — a slow first start, an ingress attaching on the next
// sync tick — arms itself with no intervention.
type healthTracker struct {
	threshold   int
	failures    int
	everHealthy bool
	// warned keeps the unreachable verdict to one report per checker: it is a
	// standing condition, not an event that recurs every interval.
	warned bool
}

type healthOutcome int

const (
	// outcomeOK is a passing probe with nothing to report.
	outcomeOK healthOutcome = iota
	outcomeFailure
	outcomeRestart
	outcomeUnreachable
	// outcomeRecovered is the first success after an unreachable verdict.
	outcomeRecovered
	// outcomeFirstPass is the first success of a checker that has never
	// warned — the ordinary case of a container that just finished starting.
	outcomeFirstPass
)

func (t *healthTracker) record(ok bool) healthOutcome {
	if ok {
		t.failures = 0
		wasWarned := t.warned
		first := !t.everHealthy
		t.everHealthy = true
		t.warned = false
		switch {
		case wasWarned:
			return outcomeRecovered
		case first:
			return outcomeFirstPass
		}
		return outcomeOK
	}

	t.failures++
	if t.failures < t.threshold {
		return outcomeFailure
	}
	if t.everHealthy {
		return outcomeRestart
	}
	if t.warned {
		// Already reported; keep probing quietly in case it starts working.
		return outcomeOK
	}
	t.warned = true
	return outcomeUnreachable
}

// probeAddress describes where the probe was pointed, in the same form the
// checker actually used.
func probeAddress(target httpProbeTarget, hc *core.HealthCheck) string {
	switch {
	case hc.Type == core.HealthCheckExec:
		return fmt.Sprintf("exec %v", hc.ExecCommand)
	case target.hostPort != 0:
		return fmt.Sprintf("http://127.0.0.1:%d%s", target.hostPort, hc.HTTPPath)
	default:
		return fmt.Sprintf("http://<container-ip>:%d%s", target.containerPort, hc.HTTPPath)
	}
}

// unreachableDetail says what to do about it, in the operator's terms. The two
// http cases fail identically (connection refused) but have opposite fixes, so
// naming the likely cause is the whole value of the message.
func unreachableDetail(target httpProbeTarget, hc *core.HealthCheck) string {
	switch {
	case hc.Type == core.HealthCheckExec:
		return "the exec health probe has never exited 0; check the command exists inside the image"
	case target.hostPort != 0:
		// The port is guaranteed to be one this component declares — core
		// rejects a health check that names any other — so "you wrote the
		// wrong number" is no longer one of the possibilities, and suggesting
		// it would send someone to check something that cannot be wrong.
		//
		// What is left is inside the container. The one that costs the most
		// time is an app that is not listening where its manifest says: a
		// config file that never made it into the image, so the app fell back
		// to a framework default port and everything about it looks healthy
		// from the outside.
		return fmt.Sprintf("nothing answered on 127.0.0.1:%d. The port is published, so the app inside the container is probably not listening on %d — check that its config actually reached the image (a config file left out of the Dockerfile makes a framework fall back to its own default port), or that %s is a path it serves",
			target.hostPort, target.containerPortFor(), hc.HTTPPath)
	default:
		return fmt.Sprintf("qooqd could not reach the container's own address on port %d. That works on Linux and OrbStack but not on Docker Desktop, where containers live in a VM the host cannot route into: use an exec probe, or publish a host port for this component", target.containerPort)
	}
}

// httpProbeTarget says where an http probe should connect for one component.
//
// Historically the answer was always 127.0.0.1:<hc.HTTPPort>, which quietly
// assumed the component publishes a host port. Once an ingress fronts apps,
// most of them will not — and a probe that cannot connect looks exactly like
// an app that has died, so every such component would be restarted forever.
//
// The port the operator wrote in health.port is matched against the spec to
// decide what they meant, rather than adding a field asking them to say it
// twice.
// containerPortFor is the port inside the container that this probe is really
// asking about — what an app has to be listening on for the probe to answer.
// It is what a message about the app's own behaviour has to name, since the
// host port is a number the app never sees.
func (t httpProbeTarget) containerPortFor() int {
	if t.containerPort != 0 {
		return t.containerPort
	}
	// Only reachable for a component deployed before health ports were checked,
	// whose health.port matches nothing it declares. The host port is then the
	// best guess available.
	return t.hostPort
}

type httpProbeTarget struct {
	// hostPort, when non-zero, is a published port reachable at 127.0.0.1 —
	// exactly the pre-existing behaviour.
	hostPort int
	// containerPort is the port inside the container. When hostPort is zero it
	// is also where the probe connects — the container's own address, which
	// has to be looked up and changes on every recreate. When hostPort is set
	// it is informational: the port the app has to be listening on for the
	// published one to answer.
	containerPort int
}

// resolveHTTPProbeTarget interprets hc.HTTPPort against the component's ports.
//
// An unmatched port keeps the old meaning (loopback, as written). That matters
// for components deployed before this existed, whose health.port may name a
// host port that is not in the spec at all — changing those to something
// cleverer would restart working apps.
func resolveHTTPProbeTarget(ports []runtime.PortMapping, hc *core.HealthCheck) httpProbeTarget {
	for _, p := range ports {
		if p.HostPort != 0 && p.HostPort == hc.HTTPPort {
			return httpProbeTarget{hostPort: p.HostPort, containerPort: p.ContainerPort}
		}
	}
	for _, p := range ports {
		if p.ContainerPort == hc.HTTPPort {
			// Both sides are recorded even when the probe dials the host port:
			// where to connect and what the app must be listening on are
			// different facts, and a message about the app's own behaviour has
			// to name the port the app can actually see.
			return httpProbeTarget{hostPort: p.HostPort, containerPort: p.ContainerPort}
		}
	}
	return httpProbeTarget{hostPort: hc.HTTPPort}
}

// healthProber holds one checker's probing state. It exists for the address
// cache: a container-addressed probe needs an IP that costs an inspect call to
// learn and that changes whenever the container is recreated.
type healthProber struct {
	m           *Monitor
	containerID runtime.ContainerID
	hc          *core.HealthCheck
	target      httpProbeTarget
	// ip is the container's last known address. Empty means "not looked up
	// yet"; it is cleared after a failed probe so the next attempt re-resolves
	// rather than retrying an address that may simply have moved.
	ip string
}

// probe runs a single liveness check, bounded by hc.TimeoutSeconds.
func (p *healthProber) probe(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, time.Duration(p.hc.TimeoutSeconds)*time.Second)
	defer cancel()

	switch p.hc.Type {
	case core.HealthCheckHTTP:
		return p.probeHTTP(pctx)
	case core.HealthCheckExec:
		return p.probeExec(pctx)
	default:
		// Deploy-time validation (core.normalizeHealthCheck) rejects any
		// other type, so this should be unreachable; fail closed rather than
		// silently never restarting a misconfigured component.
		return false
	}
}

// probeHTTP GETs the probe path and counts 2xx/3xx as healthy.
//
// A published host port is reached at 127.0.0.1 from qooqd's own namespace, as
// it always was. A container-only port is reached at the container's address,
// which requires the host to be able to route there: true on Linux and
// OrbStack, false on Docker Desktop, where containers live in a VM. That
// combination has to use an exec probe, and it is documented as such.
func (p *healthProber) probeHTTP(ctx context.Context) bool {
	if p.target.hostPort != 0 {
		return p.get(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", p.target.hostPort, p.hc.HTTPPath))
	}

	if p.ip == "" {
		st, err := p.m.rt.InspectContainer(ctx, p.containerID)
		if err != nil || st.IPAddress == "" {
			// No address to probe is not evidence the app is unhealthy, but
			// treating it as healthy would disable the check entirely. Report
			// a failure and let the threshold decide, having said why.
			p.m.log.Warn("health probe: container address unavailable",
				"container", p.containerID, "err", err)
			return false
		}
		p.ip = st.IPAddress
	}

	ok := p.get(ctx, fmt.Sprintf("http://%s:%d%s", p.ip, p.target.containerPort, p.hc.HTTPPath))
	if !ok {
		// The address may have moved rather than the app having died; drop it
		// so the next attempt looks it up again. The failure still counts, so
		// a genuinely dead app is not kept alive by this.
		p.ip = ""
	}
	return ok
}

func (p *healthProber) get(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := p.m.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// probeExec runs hc.ExecCommand inside the container via the runtime's Exec;
// exit code 0 counts as healthy.
func (p *healthProber) probeExec(ctx context.Context) bool {
	code, err := p.m.rt.Exec(ctx, p.containerID, p.hc.ExecCommand)
	if err != nil {
		return false
	}
	return code == 0
}
