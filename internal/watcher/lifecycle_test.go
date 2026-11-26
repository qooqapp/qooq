package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// daemonStub answers the two calls the lifecycle operations make — list the
// components, then act on one — and records what was asked of it. What the
// tests below check is mostly which requests were made, because the defect
// being fixed was a request that was never made at all.
type daemonStub struct {
	comps    []client.ComponentResponse
	requests []string
	status   int // when non-zero, the answer to the action (not the listing)
}

func (d *daemonStub) start(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.requests = append(d.requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/components" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(d.comps)
			return
		}
		if d.status != 0 {
			w.WriteHeader(d.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.ComponentResponse{ID: "c1"})
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

func (d *daemonStub) asked(method, path string) bool {
	for _, r := range d.requests {
		if r == method+" "+path {
			return true
		}
	}
	return false
}

func lifecycleTarget() *Target {
	return &Target{
		Name: "acme--prod_api", ComponentName: "api",
		DeploySpec: `{"namespace":"acme--prod"}`,
	}
}

// Removing a project takes down what it deployed.
//
// Before this, deleting a target deleted the row and nothing else: the
// container kept running, the address kept answering, and the project was gone
// from every listing — so the deployment nobody wanted was also the one nobody
// could find again. Verified against a real daemon before it was fixed, and
// pinned here.
func TestRemovingAProjectAlsoRemovesWhatItDeployed(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "c1", Name: "api", Namespace: "acme--prod", Phase: "Running"},
	}}
	w := &Watcher{client: d.start(t), log: zap.NewNop()}

	if err := w.removeComponentFor(context.Background(), lifecycleTarget()); err != nil {
		t.Fatalf("removeComponentFor: %v", err)
	}
	if !d.asked(http.MethodDelete, "/v1/components/c1") {
		t.Errorf("the component was never removed; asked: %v", d.requests)
	}
}

// Its services go with it, and only its own.
//
// A project's services: are components of their own, and removing the project
// used to leave its database running where nobody could see it. Same-named
// services in another namespace, and components the project never declared,
// are not its to remove.
func TestRemovingAProjectAlsoRemovesItsServices(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "app", Name: "api", Namespace: "acme--prod", Phase: "Running"},
		{ID: "db", Name: "api-db", Namespace: "acme--prod", Phase: "Running"},
		{ID: "cache", Name: "api-cache", Namespace: "acme--prod", Phase: "Running"},
		{ID: "elsewhere", Name: "api-db", Namespace: "acme--staging", Phase: "Running"},
		{ID: "gateway", Name: "api-gateway", Namespace: "acme--prod", Phase: "Running"},
	}}
	w := &Watcher{client: d.start(t), log: zap.NewNop()}
	tg := lifecycleTarget()
	tg.Topology = encodeTopology([]watcherclient.TopologyService{
		{Name: "db", Component: "api-db", Type: "postgres"},
		{Name: "cache", Component: "api-cache", Type: "redis"},
	}, nil)

	if err := w.removeComponentFor(context.Background(), tg); err != nil {
		t.Fatalf("removeComponentFor: %v", err)
	}
	for _, id := range []string{"app", "db", "cache"} {
		if !d.asked(http.MethodDelete, "/v1/components/"+id) {
			t.Errorf("%s was left running; asked: %v", id, d.requests)
		}
	}
	for _, id := range []string{"elsewhere", "gateway"} {
		if d.asked(http.MethodDelete, "/v1/components/"+id) {
			t.Errorf("removed %s, which is not this project's", id)
		}
	}
}

// Another namespace's component with the same name is not this project's.
func TestRemovalMatchesTheNamespaceToo(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "other", Name: "api", Namespace: "acme--staging", Phase: "Running"},
	}}
	w := &Watcher{client: d.start(t), log: zap.NewNop()}

	if err := w.removeComponentFor(context.Background(), lifecycleTarget()); err != nil {
		t.Fatalf("removeComponentFor: %v", err)
	}
	if d.asked(http.MethodDelete, "/v1/components/other") {
		t.Error("removed a component belonging to a different namespace")
	}
}

// Nothing deployed is not a failure to remove: that is the state being asked
// for, and a target may never have built.
func TestRemovingAProjectThatNeverDeployedIsNotAnError(t *testing.T) {
	d := &daemonStub{}
	w := &Watcher{client: d.start(t), log: zap.NewNop()}

	if err := w.removeComponentFor(context.Background(), lifecycleTarget()); err != nil {
		t.Errorf("removing nothing reported an error: %v", err)
	}
}

// Stop and start do say so, because acting on a project with nothing deployed
// is a caller mistake rather than an idempotent no-op.
func TestStoppingSomethingThatWasNeverDeployedSaysSo(t *testing.T) {
	d := &daemonStub{}
	w := &Watcher{client: d.start(t), log: zap.NewNop()}

	if err := w.stopComponentFor(context.Background(), lifecycleTarget()); err == nil {
		t.Error("stopping a project with no component reported success")
	}
}

func TestStopAndStartActOnTheComponent(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(*Watcher, context.Context, *Target) error
		path string
	}{
		{"stop", (*Watcher).stopComponentFor, "/v1/components/c1/stop"},
		{"start", (*Watcher).startComponentFor, "/v1/components/c1/start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &daemonStub{comps: []client.ComponentResponse{
				{ID: "c1", Name: "api", Namespace: "acme--prod", Phase: "Running"},
			}}
			w := &Watcher{client: d.start(t), log: zap.NewNop()}
			if err := tc.do(w, context.Background(), lifecycleTarget()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !d.asked(http.MethodPost, tc.path) {
				t.Errorf("%s never reached the daemon; asked: %v", tc.name, d.requests)
			}
		})
	}
}

// A paused project stops being polled.
func TestAPausedProjectIsNotSweptUp(t *testing.T) {
	db := newTestDB(t)
	past := time.Now().Add(-time.Minute)
	for _, tg := range []*Target{
		{Name: "running", ComponentName: "a", NextCheckAt: past},
		{Name: "paused", ComponentName: "b", NextCheckAt: past, DeployPaused: true},
	} {
		if err := db.Create(tg).Error; err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	due, err := dueTargets(db, time.Now(), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	if len(due) != 1 || due[0].Name != "running" {
		t.Errorf("due = %v, want only the unpaused one", names(due))
	}
}

// But an explicit deploy still builds it.
//
// Pausing automatic deploys is not refusing to deploy. Without this the deploy
// button silently does nothing on exactly the projects somebody is most likely
// to be in the middle of fixing — and nothing on the screen would say why.
func TestAnExplicitDeployStillBuildsAPausedProject(t *testing.T) {
	db := newTestDB(t)
	past := time.Now().Add(-time.Minute)
	if err := db.Create(&Target{
		Name: "paused", ComponentName: "b", NextCheckAt: past,
		DeployPaused: true, ForceRebuild: true,
	}).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	due, err := dueTargets(db, time.Now(), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("a forced rebuild of a paused project was skipped: %v", names(due))
	}
}

func names(ts []Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
