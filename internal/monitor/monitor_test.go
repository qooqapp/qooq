package monitor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/internal/store/bbolt"
)

// discardLogger is a slog.Logger that writes nowhere, for tests that only
// care about behavior, not log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRuntime is a minimal runtime.Runtime stand-in for monitor tests. Only
// Stats and Exec are ever asserted on; everything else is a no-op required
// solely to satisfy the interface.
type fakeRuntime struct {
	mu         sync.Mutex
	statsCalls int
	execCalls  int
}

func (f *fakeRuntime) Stats(ctx context.Context, id runtime.ContainerID) (<-chan runtime.StatSample, <-chan error) {
	f.mu.Lock()
	f.statsCalls++
	f.mu.Unlock()
	// Channels that never fire: the streaming goroutine parks on ctx.Done()
	// until the test cancels it, which is all these tests need.
	return make(chan runtime.StatSample), make(chan error)
}

func (f *fakeRuntime) statsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statsCalls
}

func (f *fakeRuntime) Exec(ctx context.Context, id runtime.ContainerID, cmd []string) (int, error) {
	f.mu.Lock()
	f.execCalls++
	f.mu.Unlock()
	return 0, nil
}

func (f *fakeRuntime) execCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execCalls
}

// The rest of runtime.Runtime: unused by these tests, present only to satisfy
// the interface.
func (f *fakeRuntime) PullImage(ctx context.Context, ref string, opts runtime.PullOptions, registryAuth string) (runtime.ImageID, error) {
	return "", nil
}
func (f *fakeRuntime) LoadImage(ctx context.Context, tarball io.Reader) ([]runtime.ImageID, error) {
	return nil, nil
}
func (f *fakeRuntime) BuildImage(ctx context.Context, buildCtx io.Reader, opts runtime.BuildOptions) (runtime.ImageID, error) {
	return "", nil
}
func (f *fakeRuntime) ListImages(ctx context.Context) ([]runtime.ImageSummary, error) {
	return nil, nil
}
func (f *fakeRuntime) ResolveImageDigest(ctx context.Context, ref string, registryAuth string) (string, error) {
	return "", nil
}

func (f *fakeRuntime) PushImage(ctx context.Context, ref string, registryAuth string) error {
	return nil
}

func (f *fakeRuntime) PruneBuildCache(ctx context.Context, reservedBytes int64) (uint64, error) {
	return 0, nil
}
func (f *fakeRuntime) RemoveImage(ctx context.Context, ref string) error            { return nil }
func (f *fakeRuntime) EnsureNetwork(ctx context.Context, name string) error         { return nil }
func (f *fakeRuntime) RemoveNetworkIfUnused(ctx context.Context, name string) error { return nil }
func (f *fakeRuntime) DisconnectNetwork(ctx context.Context, id runtime.ContainerID, network string) error {
	return nil
}

func (f *fakeRuntime) ConnectNetwork(ctx context.Context, id runtime.ContainerID, network string, aliases []string) error {
	return nil
}
func (f *fakeRuntime) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	return "", nil
}
func (f *fakeRuntime) StartContainer(ctx context.Context, id runtime.ContainerID) error { return nil }
func (f *fakeRuntime) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	return nil
}
func (f *fakeRuntime) RemoveContainer(ctx context.Context, id runtime.ContainerID, force bool) error {
	return nil
}
func (f *fakeRuntime) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerStatus, error) {
	return runtime.ContainerStatus{}, nil
}
func (f *fakeRuntime) ListContainers(ctx context.Context, filter runtime.Filter) ([]runtime.ContainerStatus, error) {
	return nil, nil
}
func (f *fakeRuntime) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}
func (f *fakeRuntime) Events(ctx context.Context, filter runtime.Filter) (<-chan runtime.Event, <-chan error) {
	return nil, nil
}
func (f *fakeRuntime) Ping(ctx context.Context) error { return nil }
func (f *fakeRuntime) Close() error                   { return nil }

// newTestServiceAndStore builds a real core.Service over a temp-file bbolt
// store (mirrors internal/api/router_test.go's testRouter helper), so tests
// can seed components directly and let Monitor drive the real Service code
// path (m.svc.Get, NotePanicRecovered, ListEvents) rather than a hand-rolled
// double.
func newTestServiceAndStore(t *testing.T, rt runtime.Runtime) (*core.Service, core.Store) {
	t.Helper()
	st, err := bbolt.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return core.NewService(rt, st, "test", "", discardLogger()), st
}

func newTestMonitor(t *testing.T, rt runtime.Runtime, svc *core.Service) *Monitor {
	t.Helper()
	m := New(rt, svc, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx = ctx
	return m
}

// pollUntil polls cond every millisecond until it returns true or timeout
// elapses, failing the test in the latter case. Used instead of a fixed sleep
// to wait for a goroutine this test just started to reach its first
// observable side effect.
func pollUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Problem 1: stop*/start* must not silently skip a fresh registration because
// the previous goroutine's own deferred cleanup had not run yet.
// ---------------------------------------------------------------------------

// TestStopStreamRemovesRegistrationSynchronously is the narrowest possible
// regression test: stopStream must delete the map entry itself, not leave
// that to the cancelled goroutine's own defer. Before the fix, stopStream
// only called cancel() and this assertion failed every time, deterministically
// (no goroutine scheduling involved — the entry was seeded by hand, not by a
// real goroutine that might or might not have gotten around to its defer).
func TestStopStreamRemovesRegistrationSynchronously(t *testing.T) {
	m := newTestMonitor(t, &fakeRuntime{}, mustService(t))

	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.streams["c1"] = &streamHandle{cancel: cancel}

	m.stopStream("c1")

	m.mu.Lock()
	_, stillThere := m.streams["c1"]
	m.mu.Unlock()
	if stillThere {
		t.Fatal("stopStream left the registration in place; it must delete it itself instead of relying on the cancelled goroutine's own deferred cleanup to do it later")
	}
}

// TestStopHealthCheckRemovesRegistrationSynchronously is stopStream's sibling
// test for the health-checker map.
func TestStopHealthCheckRemovesRegistrationSynchronously(t *testing.T) {
	m := newTestMonitor(t, &fakeRuntime{}, mustService(t))

	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.healthChecks["c1"] = &healthHandle{cancel: cancel}

	m.stopHealthCheck("c1")

	m.healthMu.Lock()
	_, stillThere := m.healthChecks["c1"]
	m.healthMu.Unlock()
	if stillThere {
		t.Fatal("stopHealthCheck left the registration in place; it must delete it itself instead of relying on the cancelled goroutine's own deferred cleanup to do it later")
	}
}

// TestStreamRestartsAfterStopEvenBeforeOldGoroutineCleansUp reproduces the
// failure an external `docker restart <id>` causes: a "die" event immediately
// followed by a "start" event for the same container ID, which dispatch turns
// into stopStream(cid) followed by startStream(cid, ...) with no guarantee the
// goroutine cancelled by the first call has run far enough to reach its own
// deferred map cleanup.
//
// The stale entry is seeded by hand (a real cancel func, but not one backed by
// a running streamStats goroutine) rather than raced for real, so this test is
// deterministic: against the pre-fix stopStream (cancel() only, no delete) it
// fails every run, not just under unlucky scheduling.
func TestStreamRestartsAfterStopEvenBeforeOldGoroutineCleansUp(t *testing.T) {
	rt := &fakeRuntime{}
	m := newTestMonitor(t, rt, mustService(t))

	const cid = runtime.ContainerID("c1")
	_, staleCancel := context.WithCancel(context.Background())
	m.streams[cid] = &streamHandle{cancel: staleCancel} // stands in for "old goroutine hasn't cleaned up yet"

	m.stopStream(cid)
	m.startStream(cid, "comp-1")

	pollUntil(t, time.Second,
		"startStream after stop did not start a new stats stream — the stale registration silently blocked it",
		func() bool { return rt.statsCallCount() > 0 })
}

// TestHealthCheckRestartsAfterStopEvenBeforeOldGoroutineCleansUp is the same
// scenario for the health checker: seeded stale entry, stop, start, and the
// new checker must actually run (proved by it reaching its first exec probe).
func TestHealthCheckRestartsAfterStopEvenBeforeOldGoroutineCleansUp(t *testing.T) {
	rt := &fakeRuntime{}
	svc, st := newTestServiceAndStore(t, rt)
	comp := &core.Component{
		ID: "comp-1",
		Spec: core.ComponentSpec{
			Name: "comp-1",
			HealthCheck: &core.HealthCheck{
				Type:                core.HealthCheckExec,
				ExecCommand:         []string{"true"},
				IntervalSeconds:     1,
				TimeoutSeconds:      1,
				FailureThreshold:    3,
				InitialDelaySeconds: 0,
			},
		},
	}
	if err := st.SaveComponent(comp); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	m := newTestMonitor(t, rt, svc)

	const cid = runtime.ContainerID("c1")
	_, staleCancel := context.WithCancel(context.Background())
	m.healthChecks[cid] = &healthHandle{cancel: staleCancel} // stands in for "old checker hasn't cleaned up yet"

	m.stopHealthCheck(cid)
	m.startHealthCheck(cid, comp.ID)

	pollUntil(t, time.Second,
		"startHealthCheck after stop did not start a new checker — the stale registration silently blocked it",
		func() bool { return rt.execCallCount() > 0 })
}

// mustService is a throwaway core.Service for tests that only exercise the
// stream/health map bookkeeping and never actually call into it.
func mustService(t *testing.T) *core.Service {
	t.Helper()
	svc, _ := newTestServiceAndStore(t, &fakeRuntime{})
	return svc
}

// ---------------------------------------------------------------------------
// Problem 2: the events dispatcher must recover a panic in one event's
// handling, log it, record it, and keep processing the events after it.
// ---------------------------------------------------------------------------

// TestRecoverDispatchPanicRecordsAndLetsProcessingContinue drives
// recoverDispatchPanic the way runEvents' loop body does: one call per event,
// in sequence. A panic handling the first event must not propagate (proving
// the loop that calls this in production would reach its next iteration) and
// must leave a panic_recovered event behind on the affected component.
func TestRecoverDispatchPanicRecordsAndLetsProcessingContinue(t *testing.T) {
	rt := &fakeRuntime{}
	svc, st := newTestServiceAndStore(t, rt)
	if err := st.SaveComponent(&core.Component{ID: "comp-1", Spec: core.ComponentSpec{Name: "comp-1"}}); err != nil {
		t.Fatalf("seed component: %v", err)
	}
	m := newTestMonitor(t, rt, svc)

	events := []runtime.Event{
		{ContainerID: "c1", Action: "start", Labels: map[string]string{core.LabelComponentID: "comp-1"}},
		{ContainerID: "c2", Action: "start", Labels: map[string]string{core.LabelComponentID: "comp-2"}},
	}
	var processed []string
	for _, ev := range events {
		ev := ev
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escaped recoverDispatchPanic: %v", r)
				}
			}()
			m.recoverDispatchPanic(ev, func() {
				if ev.ContainerID == "c1" {
					panic("boom")
				}
				processed = append(processed, string(ev.ContainerID))
			})
		}()
	}

	if len(processed) != 1 || processed[0] != "c2" {
		t.Fatalf("processing did not continue past the panicking event: processed=%v", processed)
	}

	evList, err := svc.ListEvents(context.Background(), "comp-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range evList {
		if e.Type == "panic_recovered" {
			found = true
			if e.Message == "" {
				t.Error("panic_recovered event has an empty message")
			}
		}
	}
	if !found {
		t.Error("no panic_recovered event was recorded for comp-1")
	}
}
