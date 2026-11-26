package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qooqapp/qooq/pkg/client"
)

// rollbackDaemon answers the three calls verifyOrRollBack makes: the component
// (from a sequence, before and after a rollback), its logs, and the rollback.
type rollbackDaemon struct {
	mu         sync.Mutex
	before     []client.ComponentResponse
	after      []client.ComponentResponse
	rolledBack bool
	calls      int
}

func (f *rollbackDaemon) serve(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			_, _ = w.Write([]byte("server listening on 3000\nReferenceError: true1029 is not defined\n"))
		case strings.HasSuffix(r.URL.Path, "/rollback"):
			f.rolledBack, f.calls = true, 0
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(client.ComponentResponse{ID: "id"})
		default:
			seq := f.before
			if f.rolledBack {
				seq = f.after
			}
			i := f.calls
			f.calls++
			if i >= len(seq) {
				i = len(seq) - 1
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(seq[i])
		}
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

func quickStable(t *testing.T, d time.Duration) {
	t.Helper()
	o := stableWindow
	stableWindow = d
	t.Cleanup(func() { stableWindow = o })
}

var httpProbe = &client.HealthCheckSpec{Type: "http", HTTPPath: "/api/health", HTTPPort: 3000}

func newlyUpserted(prev string) *client.ComponentResponse {
	return &client.ComponentResponse{ID: "id", Image: "app:06eb0b0", PreviousImage: prev}
}

func rollbackTarget() *Target {
	return &Target{Name: "app", ComponentName: "app", ForceRebuild: true}
}

// The report this exists for: the new version passes the build, its first
// health probe makes it exit 1, and the service must not stay down.
func TestANewVersionThatExitsIsRolledBack(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	quickStable(t, 20*time.Millisecond)
	f := &rollbackDaemon{
		before: []client.ComponentResponse{
			{Phase: "Running", ContainerID: "new", HealthCheck: httpProbe},
			{Phase: "Exited", ContainerID: "new", ExitCode: 1, HealthCheck: httpProbe},
		},
		after: []client.ComponentResponse{
			{Phase: "Running", ContainerID: "old", HealthCheck: httpProbe, HealthyAt: time.Now()},
		},
	}
	w := readyTestWatcher(f.serve(t))
	tg := rollbackTarget()

	err := w.verifyOrRollBack(context.Background(), tg, newlyUpserted("app:973bee4"), "06eb0b0aaaaaaa")
	if err == nil {
		t.Fatal("a version that exited was reported as deployed")
	}
	if !f.rolledBack {
		t.Fatal("the previous version was not put back")
	}
	for _, want := range []string{"did not start", "exit code 1", "(973bee4) was put back", "is running", "ReferenceError"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure message is missing %q:\n%s", want, err)
		}
	}
	if tg.RejectedCommit != "06eb0b0aaaaaaa" || tg.ForceRebuild {
		t.Errorf("commit not marked rejected (rejected=%q force=%v): polling would deploy it again",
			tg.RejectedCommit, tg.ForceRebuild)
	}
}

// A version that comes up is left alone.
func TestAHealthyNewVersionIsKept(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	f := &rollbackDaemon{before: []client.ComponentResponse{
		{Phase: "Running", ContainerID: "new", HealthCheck: httpProbe},
		{Phase: "Running", ContainerID: "new", HealthCheck: httpProbe, HealthyAt: time.Now()},
	}}
	w := readyTestWatcher(f.serve(t))
	tg := rollbackTarget()
	if err := w.verifyOrRollBack(context.Background(), tg, newlyUpserted("app:973bee4"), "06eb0b0"); err != nil {
		t.Fatalf("healthy version rejected: %v", err)
	}
	if f.rolledBack || tg.RejectedCommit != "" {
		t.Fatal("a healthy version was rolled back")
	}
}

// With no probe, Running for a moment is not proof: a process that dies on its
// first timer is Running first. It must hold for the stable window.
func TestWithoutAProbeAVersionMustStayUp(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	quickStable(t, 50*time.Millisecond)
	f := &rollbackDaemon{
		before: []client.ComponentResponse{
			{Phase: "Running", ContainerID: "new"},
			{Phase: "Running", ContainerID: "new"},
			{Phase: "Running", ContainerID: "restarted", RestartCount: 1, ExitCode: 2},
		},
		after: []client.ComponentResponse{{Phase: "Running", ContainerID: "old"}},
	}
	w := readyTestWatcher(f.serve(t))
	err := w.verifyOrRollBack(context.Background(), rollbackTarget(), newlyUpserted("app:973bee4"), "06eb0b0")
	if err == nil || !f.rolledBack || !strings.Contains(err.Error(), "restarted") {
		t.Fatalf("a version that crash-restarted was kept (rolledBack=%v): %v", f.rolledBack, err)
	}
}

// A first deploy has nothing to go back to: it fails, says so, and does not
// pretend to roll back.
func TestAFirstDeployThatExitsFailsWithoutARollback(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	f := &rollbackDaemon{before: []client.ComponentResponse{{Phase: "Exited", ContainerID: "new", ExitCode: 1}}}
	w := readyTestWatcher(f.serve(t))
	err := w.verifyOrRollBack(context.Background(), rollbackTarget(), newlyUpserted(""), "06eb0b0")
	if err == nil || f.rolledBack || !strings.Contains(err.Error(), "no earlier version") {
		t.Fatalf("rolledBack=%v err=%v", f.rolledBack, err)
	}
}

// An http probe that can never see the app (Docker Desktop, wrong port) must
// not roll back a working version — the same grace waitForComponentReady gives.
func TestAnHTTPProbeThatNeverAnswersDoesNotRollBack(t *testing.T) {
	quickTimeouts(t, time.Second, 30*time.Millisecond, 5*time.Millisecond)
	f := &rollbackDaemon{before: []client.ComponentResponse{{Phase: "Running", ContainerID: "new", HealthCheck: httpProbe}}}
	w := readyTestWatcher(f.serve(t))
	if err := w.verifyOrRollBack(context.Background(), rollbackTarget(), newlyUpserted("app:973bee4"), "06eb0b0"); err != nil || f.rolledBack {
		t.Fatalf("rolledBack=%v err=%v", f.rolledBack, err)
	}
}
