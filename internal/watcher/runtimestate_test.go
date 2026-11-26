package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// A stand-in daemon, so the whole path runs — including the HTTP client and the
// JSON shape. A stub of ListComponents would test neither.
func fakeDaemon(t *testing.T, comps []client.ComponentResponse, status int) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"the daemon is having a bad day"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(comps)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

func TestRuntimeStateFillsEndpointsAndPhase(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{
			Name: "api", Namespace: "acme--prod", Phase: "Running",
			Endpoints: []string{"http://192.168.1.9:19000"},
		},
	}, http.StatusOK)

	targets := []TargetResponse{{ComponentName: "api", Namespace: "acme--prod"}}
	w.attachRuntimeState(context.Background(), targets)

	if len(targets[0].Endpoints) != 1 || targets[0].Endpoints[0] != "http://192.168.1.9:19000" {
		t.Fatalf("endpoints = %v", targets[0].Endpoints)
	}
	if targets[0].ComponentPhase != "Running" {
		t.Errorf("phase = %q", targets[0].ComponentPhase)
	}
}

// The match is on (namespace, name). Matching on name alone would show one
// account the address of another account's component — two namespaces may each
// hold an "api", and that is the normal case, not an edge one.
func TestRuntimeStateMatchesNamespaceAndNameNotNameAlone(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "api", Namespace: "bob--prod", Phase: "Running",
			Endpoints: []string{"http://10.0.0.1:19000"}},
	}, http.StatusOK)

	// Alice's target, same component name, different namespace.
	targets := []TargetResponse{{ComponentName: "api", Namespace: "alice--prod"}}
	w.attachRuntimeState(context.Background(), targets)

	if len(targets[0].Endpoints) != 0 || targets[0].ComponentPhase != "" {
		t.Fatalf("another namespace's component was matched: %+v", targets[0])
	}
}

// A component with no namespace on the target is the offline default, and the
// daemon stores that as "default" — so the two have to be compared in the same
// spelling or nothing ever matches on a single-user install.
func TestRuntimeStateTreatsAnEmptyNamespaceAsDefault(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "web", Namespace: "default", Phase: "Running",
			Endpoints: []string{"http://127.0.0.1:8080"}},
	}, http.StatusOK)

	targets := []TargetResponse{{ComponentName: "web", Namespace: ""}}
	w.attachRuntimeState(context.Background(), targets)

	if len(targets[0].Endpoints) != 1 {
		t.Fatalf("a target with no namespace did not match the daemon's \"default\": %+v", targets[0])
	}
}

// A removed component is not where anything is running. Reporting its old
// address would send somebody to a container that no longer exists.
func TestRuntimeStateIgnoresRemovedComponents(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "api", Namespace: "acme--prod", Phase: "Removed",
			Endpoints: []string{"http://10.0.0.1:19000"}},
	}, http.StatusOK)

	targets := []TargetResponse{{ComponentName: "api", Namespace: "acme--prod"}}
	w.attachRuntimeState(context.Background(), targets)

	if len(targets[0].Endpoints) != 0 {
		t.Fatalf("a removed component was reported as running at %v", targets[0].Endpoints)
	}
}

// An unreachable daemon must not break the listing.
//
// What was built is still true and still worth showing, and failing the whole
// request would make a daemon that is merely restarting look like lost data.
func TestRuntimeStateLeavesTheListingIntactWhenTheDaemonIsDown(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, nil, http.StatusInternalServerError)

	targets := []TargetResponse{{
		Name: "acme--prod_api", ComponentName: "api", Namespace: "acme--prod",
		LastBuildStatus: "success",
	}}
	w.attachRuntimeState(context.Background(), targets)

	if targets[0].LastBuildStatus != "success" || targets[0].Name != "acme--prod_api" {
		t.Fatalf("the listing was damaged by an unreachable daemon: %+v", targets[0])
	}
	if targets[0].ComponentPhase != "" || len(targets[0].Endpoints) != 0 {
		t.Errorf("state was invented despite the daemon being down: %+v", targets[0])
	}
}

// A watcher with no daemon client configured is the shape several tests use;
// it must not panic.
func TestRuntimeStateWithNoClientIsANoOp(t *testing.T) {
	w := testWatcher(t, "open")
	targets := []TargetResponse{{ComponentName: "api", Namespace: "acme--prod"}}
	w.attachRuntimeState(context.Background(), targets)
	if targets[0].ComponentPhase != "" {
		t.Fatalf("state appeared with no client: %+v", targets[0])
	}
}
