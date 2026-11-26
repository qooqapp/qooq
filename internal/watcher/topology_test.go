package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qooqapp/qooq/pkg/client"
)

// topologyDaemon answers just enough of qooqd for a service deploy: the list
// (so references resolve), and any single-component read or upsert as a
// Running, already-healthy component.
func topologyDaemon(t *testing.T, list []client.ComponentResponse) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/components" {
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		_ = json.NewEncoder(w).Encode(client.ComponentResponse{ID: "x", Phase: "Running", HealthyAt: time.Now()})
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

func TestApplyRunConfigRecordsServicesAndReferences(t *testing.T) {
	w := testWatcherWithDB(t)
	w.client = topologyDaemon(t, []client.ComponentResponse{
		{Name: "auth", Namespace: "default", Phase: "Running", Ports: []client.PortSpec{{ContainerPort: 9000}}},
		{Name: "payments", Namespace: "default", Phase: "Running", Ports: []client.PortSpec{{ContainerPort: 9001}}},
	})
	target := &Target{Name: "web", ComponentName: "web"}
	if err := w.db.Create(target).Error; err != nil {
		t.Fatal(err)
	}
	mf := &manifest{
		App:      "web",
		Services: map[string]manifestService{"db": {Type: "postgres", Version: "16"}},
		Run: manifestRun{Env: map[string]string{
			"AUTH":     "${component.auth.address}",
			"AUTH2":    "${component.auth.host}",
			"PAYMENTS": "${component.payments.address}",
		}},
	}
	if _, err := w.applyRunConfig(context.Background(), target, mf, deploySpecFields{}, t.TempDir(), "abc1234"); err != nil {
		t.Fatalf("applyRunConfig: %v", err)
	}
	if err := w.recordSuccess(target, "abc1234"); err != nil {
		t.Fatal(err)
	}

	var got Target
	if err := w.db.First(&got, target.ID).Error; err != nil {
		t.Fatal(err)
	}
	topo := got.topologyForResponse()
	if topo == nil || len(topo.Services) != 1 {
		t.Fatalf("topology = %q", got.Topology)
	}
	s := topo.Services[0]
	if s.Name != "db" || s.Component != "web-db" || s.Type != "postgres" || s.Version != "16" {
		t.Errorf("service = %+v", s)
	}
	if strings.Join(topo.References, ",") != "auth,payments" {
		t.Errorf("references = %v, want sorted and de-duplicated [auth payments]", topo.References)
	}
}

// A rebuild that no longer has services or references must clear the old
// record rather than keep describing a deploy that is gone.
func TestRecordSuccessClearsStaleTopology(t *testing.T) {
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web", Topology: `{"references":["api"]}`}
	saveTarget(t, w, tg)

	tg.Topology = encodeTopology(nil, nil)
	if err := w.recordSuccess(tg, "n1"); err != nil {
		t.Fatal(err)
	}
	got := getTargetByID(t, w.db, tg.ID)
	if got.Topology != "" || got.topologyForResponse() != nil {
		t.Errorf("topology = %q, want empty", got.Topology)
	}
}

// Service phase is read from the same listing, by (namespace, name). Another
// namespace's component of the same name must not lend its phase.
func TestRuntimeStateFillsServicePhaseByNamespace(t *testing.T) {
	w := testWatcher(t, "open")
	w.client = fakeDaemon(t, []client.ComponentResponse{
		{Name: "web-db", Namespace: "bob--prod", Phase: "Failed"},
		{Name: "web-db", Namespace: "acme--prod", Phase: "Running"},
		{Name: "web-cache", Namespace: "bob--prod", Phase: "Running"},
	}, http.StatusOK)

	tgt := Target{Name: "acme--prod_web", ComponentName: "web", DeploySpec: `{"namespace":"acme--prod"}`,
		Topology: encodeTopology(nil, nil)}
	tgt.Topology = `{"services":[{"name":"db","component":"web-db","type":"postgres"},{"name":"cache","component":"web-cache","type":"redis"}]}`
	resp := tgt.toResponse()
	targets := []TargetResponse{resp}
	w.attachRuntimeState(context.Background(), targets)

	svcs := targets[0].Topology.Services
	if svcs[0].Phase != "Running" {
		t.Errorf("db phase = %q, want Running (acme's own)", svcs[0].Phase)
	}
	if svcs[1].Phase != "" {
		t.Errorf("cache phase = %q, want empty: only bob has that component", svcs[1].Phase)
	}
}
