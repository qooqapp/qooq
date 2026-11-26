package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// componentServer serves GET /v1/components/{id} from a sequence of responses,
// repeating the last one once the sequence runs out. That shape is the point:
// readiness is a thing that changes between polls, and a test that can only
// describe one fixed answer cannot tell "ready" from "ready eventually".
func componentServer(t *testing.T, seq ...client.ComponentResponse) (*client.Client, func() int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := calls
		calls++
		if i >= len(seq) {
			i = len(seq) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seq[i])
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL), func() int { return calls }
}

func readyTestWatcher(c *client.Client) *Watcher {
	return &Watcher{client: c, log: zap.NewNop()}
}

// Fast timings so a test measures the decision, not the clock.
func quickTimeouts(t *testing.T, ready, grace, poll time.Duration) {
	t.Helper()
	or, og, op := serviceReadyTimeout, healthGraceTimeout, servicePollInterval
	serviceReadyTimeout, healthGraceTimeout, servicePollInterval = ready, grace, poll
	t.Cleanup(func() {
		serviceReadyTimeout, healthGraceTimeout, servicePollInterval = or, og, op
	})
}

func running(hc *client.HealthCheckSpec, healthyAt time.Time) client.ComponentResponse {
	return client.ComponentResponse{Phase: "Running", HealthCheck: hc, HealthyAt: healthyAt}
}

// A component with no health check makes no readiness claim, so Running is the
// whole answer. This is the pre-existing behaviour and must not change.
func TestReadyWithoutAHealthCheckIsJustRunning(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 10*time.Millisecond)
	c, _ := componentServer(t, running(nil, time.Time{}))
	if err := readyTestWatcher(c).waitForComponentReady(context.Background(), "id", "svc"); err != nil {
		t.Fatalf("want ready, got %v", err)
	}
}

// The bug this whole change exists for: a database container is Running
// several seconds before it accepts connections, and deploying the app in
// that window makes it fail on startup. Running alone must not end the wait.
func TestRunningIsNotReadyWhileTheProbeHasNotPassed(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 10*time.Millisecond)
	exec := &client.HealthCheckSpec{Type: "exec", ExecCommand: []string{"pg_isready"}}

	notYet := running(exec, time.Time{})
	ready := running(exec, time.Now().UTC())

	// Three polls that say "running, probe has not passed", then one that says
	// it has — the shape of a real postgres start.
	c, calls := componentServer(t, notYet, notYet, notYet, ready)
	if err := readyTestWatcher(c).waitForComponentReady(context.Background(), "id", "db"); err != nil {
		t.Fatalf("want ready, got %v", err)
	}
	if calls() < 4 {
		t.Fatalf("returned after %d polls: it stopped waiting before the probe passed", calls())
	}
}

// An exec probe reaches the container regardless of network topology, so one
// that never passes is evidence about the service, not the probe. The deploy
// must fail rather than hand the app a database that is not answering.
func TestExecProbeThatNeverPassesFailsTheWait(t *testing.T) {
	quickTimeouts(t, 150*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond)
	exec := &client.HealthCheckSpec{Type: "exec", ExecCommand: []string{"pg_isready"}}
	c, _ := componentServer(t, running(exec, time.Time{}))

	err := readyTestWatcher(c).waitForComponentReady(context.Background(), "id", "db")
	if err == nil {
		t.Fatal("want failure, got ready")
	}
	if !strings.Contains(err.Error(), "health probe never passed") {
		t.Fatalf("error does not say what it was waiting for: %v", err)
	}
}

// An http probe can be permanently unable to see a healthy app (a
// container-only port on Docker Desktop). Blocking deploys forever on that
// would turn one misconfiguration into a dead pipeline, so the wait gives up
// and continues.
func TestHTTPProbeThatNeverPassesGivesUpAndContinues(t *testing.T) {
	quickTimeouts(t, 5*time.Second, 50*time.Millisecond, 10*time.Millisecond)
	httpHC := &client.HealthCheckSpec{Type: "http", HTTPPath: "/", HTTPPort: 8080}
	c, _ := componentServer(t, running(httpHC, time.Time{}))

	start := time.Now()
	if err := readyTestWatcher(c).waitForComponentReady(context.Background(), "id", "app"); err != nil {
		t.Fatalf("want it to continue, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("it waited out the full ready timeout instead of the grace period")
	}
}

// RestartExhausted stays terminal: a component that has stopped trying is an
// answer, not something to keep polling.
func TestRestartExhaustedEndsTheWait(t *testing.T) {
	quickTimeouts(t, 5*time.Second, 5*time.Second, 10*time.Millisecond)
	exec := &client.HealthCheckSpec{Type: "exec", ExecCommand: []string{"pg_isready"}}
	dead := client.ComponentResponse{Phase: "Exited", HealthCheck: exec, RestartExhausted: true}
	c, _ := componentServer(t, dead)

	err := readyTestWatcher(c).waitForComponentReady(context.Background(), "id", "db")
	if err == nil || !strings.Contains(err.Error(), "gave up restarting") {
		t.Fatalf("want the gave-up error, got %v", err)
	}
}
