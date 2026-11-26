// Package monitor observes managed containers via the runtime abstraction and
// feeds lifecycle events and resource-usage samples into core.Service. It
// depends only on internal/runtime and internal/core — never on a concrete
// engine (no Docker SDK import).
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

// reconnectDelay is the backoff before re-subscribing to the events stream
// after it errors out (e.g. daemon hiccup).
const reconnectDelay = 2 * time.Second

// Monitor runs a single events goroutine plus one stats goroutine per running
// managed container. Stats streams start/stop in lockstep with lifecycle
// events (no polling).
type Monitor struct {
	rt  runtime.Runtime
	svc *core.Service
	log *slog.Logger

	ctx context.Context // base context for spawned stats streams

	mu      sync.Mutex
	streams map[runtime.ContainerID]*streamHandle

	// healthMu guards healthChecks, one cancel func per container currently
	// being liveness-probed. Mirrors the streams map above but is kept
	// separate since a container's stats stream and health checker start/stop
	// independently (a container may have no HealthCheck configured at all).
	healthMu     sync.Mutex
	healthChecks map[runtime.ContainerID]*healthHandle
	httpClient   *http.Client
}

// New wires a Monitor.
func New(rt runtime.Runtime, svc *core.Service, log *slog.Logger) *Monitor {
	return &Monitor{
		rt:           rt,
		svc:          svc,
		log:          log,
		streams:      make(map[runtime.ContainerID]*streamHandle),
		healthChecks: make(map[runtime.ContainerID]*healthHandle),
		httpClient:   &http.Client{},
	}
}

// Start adopts already-running managed containers (reconnecting their stats
// streams) and then runs the events loop until ctx is cancelled. It blocks, so
// callers typically run it in a goroutine.
func (m *Monitor) Start(ctx context.Context) {
	m.ctx = ctx
	m.adoptRunning(ctx)
	m.runEvents(ctx)
	m.stopAll()
}

// adoptRunning opens a stats stream for every currently-running managed
// container belonging to this qooqd instance. This is how monitoring is
// re-attached after a daemon restart — including for a container that
// core.Service's boot-time Reconcile (called synchronously before Monitor
// starts, see cmd/qooqd/main.go) just adopted as an orphan: by the time this
// runs, the component record already exists, so the m.svc.Get lookup inside
// startHealthCheck succeeds and the health checker attaches normally, with no
// special-casing needed here. Orphans discovered by a later periodic
// reconcile pass (after Monitor has already done its one-time adoption scan)
// are a known v1 gap: their stats/health streams only attach on the
// container's next lifecycle event (e.g. a subsequent restart), not
// immediately — acceptable because the primary scenario this feature targets
// (recovering from a wiped datastore) is exactly the boot-time case above,
// and the restart manager still recreates a crashed adopted container
// correctly regardless of whether a stats/health goroutine is attached.
func (m *Monitor) adoptRunning(ctx context.Context) {
	list, err := m.rt.ListContainers(ctx, runtime.Filter{
		Labels: map[string]string{core.LabelManaged: "true"},
		All:    false,
	})
	if err != nil {
		m.log.Warn("monitor: list running containers for adoption", "err", err)
		return
	}
	for _, c := range list {
		if !m.svc.OwnsContainer(c.Labels) {
			continue
		}
		if c.Phase != runtime.PhaseRunning {
			continue
		}
		if cid := c.Labels[core.LabelComponentID]; cid != "" {
			m.startStream(c.ID, cid)
			m.startHealthCheck(c.ID, cid)
			m.log.Info("monitor: adopted running container", "container", c.ID, "component", cid)
		}
	}
}

// runEvents subscribes to managed-container events and dispatches them until
// ctx is cancelled. It reconnects on stream errors.
func (m *Monitor) runEvents(ctx context.Context) {
	filter := runtime.Filter{Labels: map[string]string{core.LabelManaged: "true"}}
	for {
		if ctx.Err() != nil {
			return
		}
		events, errs := m.rt.Events(ctx, filter)
		loop := true
		for loop {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					loop = false
					break
				}
				m.dispatchSafe(ev)
			case err := <-errs:
				if ctx.Err() == nil && err != nil {
					m.log.Warn("monitor: events stream error, reconnecting", "err", err)
				}
				loop = false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// dispatchSafe runs dispatch behind a recover boundary. This is the single
// events goroutine for every managed container on this qooqd instance
// (runEvents): an unrecovered panic while handling one event would not just
// lose that event, it would kill the goroutine and, since nothing above it on
// the call stack recovers either, crash the whole process — taking build,
// health-check and restart-recovery down for every other component along with
// it, over a bug triggered by one event. Recovering here, one event at a
// time, is what stops that.
func (m *Monitor) dispatchSafe(ev runtime.Event) {
	m.recoverDispatchPanic(ev, func() { m.dispatch(ev) })
}

// recoverDispatchPanic runs fn and, if it panics, logs an ERROR (with the
// affected component/container and a full stack trace) and records a
// panic_recovered event on the component if one can be resolved — the same
// two things a caught panic needs everywhere else in this codebase (see
// core.Service.NotePanicRecovered): a trace for whoever is watching logs, and
// a durable, visible record for whoever looks at the component later,
// because a recovered panic with no trace anywhere is worse than a crash —
// a crash at least gets noticed.
func (m *Monitor) recoverDispatchPanic(ev runtime.Event, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			cid := ev.Labels[core.LabelComponentID]
			m.log.Error("monitor: panic recovered while dispatching event, continuing",
				"component", cid, "container", ev.ContainerID, "action", ev.Action,
				"panic", r, "stack", string(stack))
			m.svc.NotePanicRecovered(m.ctx, cid, fmt.Sprintf("event dispatch (%s)", ev.Action), r)
		}
	}()
	fn()
}

// dispatch updates core state for the event and adjusts the stats stream for
// the container's lifecycle transition. Events for containers belonging to a
// different qooqd instance are ignored outright (HandleEvent would also
// reject them, but bailing here additionally skips starting/stopping stats
// and health-check goroutines for a container this instance must never touch).
func (m *Monitor) dispatch(ev runtime.Event) {
	if !m.svc.OwnsContainer(ev.Labels) {
		return
	}
	m.svc.HandleEvent(m.ctx, ev)

	cid := ev.Labels[core.LabelComponentID]
	switch ev.Action {
	case "start":
		if cid != "" {
			m.startStream(ev.ContainerID, cid)
			m.startHealthCheck(ev.ContainerID, cid)
		}
	case "die", "stop", "destroy":
		m.stopStream(ev.ContainerID)
		m.stopHealthCheck(ev.ContainerID)
		if ev.Action == "die" && cid != "" {
			// HandleEvent above already persisted the exit code; the restart
			// manager re-reads the component and decides on its own.
			m.svc.EvaluateRestart(m.ctx, cid, core.RestartReasonCrash)
		}
	}
}

// streamHandle is the map value for an in-flight stats stream. It exists so
// the goroutine's own cleanup defer can tell "am I still the current
// registration for this container" apart from "someone else already replaced
// me" — a context.CancelFunc cannot be compared with == (only to nil), but a
// pointer to this wrapper can.
type streamHandle struct {
	cancel context.CancelFunc
}

// startStream opens a stats goroutine for a container unless one already runs.
func (m *Monitor) startStream(containerID runtime.ContainerID, componentID string) {
	m.mu.Lock()
	if _, exists := m.streams[containerID]; exists {
		m.mu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(m.ctx)
	h := &streamHandle{cancel: cancel}
	m.streams[containerID] = h
	m.mu.Unlock()

	go m.streamStats(sctx, containerID, componentID, h)
}

// streamStats consumes the runtime stats stream and records the latest sample
// per component until the stream ends or its context is cancelled.
func (m *Monitor) streamStats(ctx context.Context, containerID runtime.ContainerID, componentID string, h *streamHandle) {
	defer func() {
		m.mu.Lock()
		// Delete only if this goroutine's own handle is still the one
		// registered. stopStream now deletes synchronously on cancel (see its
		// comment), so by the time this runs after being cancelled, a
		// subsequent startStream may already have installed a fresh handle
		// for a new goroutine — deleting unconditionally here would erase
		// that live registration and make the container look like it has no
		// stats stream at all, which is the exact bug this guards against.
		// A goroutine that instead exits on its own (stream closed/errored,
		// no stopStream involved) still finds its own handle in place and
		// cleans up correctly.
		if m.streams[containerID] == h {
			delete(m.streams, containerID)
		}
		m.mu.Unlock()
	}()

	samples, errs := m.rt.Stats(ctx, containerID)
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-samples:
			if !ok {
				return
			}
			m.svc.RecordStat(componentID, s)
		case err := <-errs:
			if ctx.Err() == nil && err != nil {
				m.log.Debug("monitor: stats stream ended", "container", containerID, "err", err)
			}
			return
		}
	}
}

// stopStream cancels a container's stats goroutine if present, and removes
// its registration synchronously rather than waiting for the cancelled
// goroutine's own deferred cleanup to get around to it.
//
// That distinction matters for the exact sequence an external `docker restart
// <id>` produces: a "die" event immediately followed by a "start" event for
// the same container ID. dispatch calls stopStream then, on the very next
// event, startStream — with no guarantee the goroutine cancelled here has
// even been scheduled again yet, let alone reached its defer. If the map
// entry were still keyed to it, startStream's "already running" check would
// see it, silently skip starting a fresh stream, and this container would
// carry no stats collection (and, for health, no restart protection) until
// some later lifecycle event happened to retrigger it — possibly never, with
// no log line anywhere to explain why.
func (m *Monitor) stopStream(containerID runtime.ContainerID) {
	m.mu.Lock()
	h, ok := m.streams[containerID]
	if ok {
		delete(m.streams, containerID)
	}
	m.mu.Unlock()
	if ok {
		h.cancel()
	}
}

// stopAll cancels every running stats stream and health checker (called on
// shutdown).
func (m *Monitor) stopAll() {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.streams))
	for _, h := range m.streams {
		cancels = append(cancels, h.cancel)
	}
	m.mu.Unlock()
	for _, c := range cancels {
		c()
	}

	m.healthMu.Lock()
	hcancels := make([]context.CancelFunc, 0, len(m.healthChecks))
	for _, h := range m.healthChecks {
		hcancels = append(hcancels, h.cancel)
	}
	m.healthMu.Unlock()
	for _, c := range hcancels {
		c()
	}
}
