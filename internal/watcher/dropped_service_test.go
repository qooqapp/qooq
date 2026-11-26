package watcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

func twoServiceTarget() *Target {
	tg := lifecycleTarget()
	tg.Topology = encodeTopology([]watcherclient.TopologyService{
		{Name: "alpha", Component: "api-alpha", Type: "postgres"},
		{Name: "beta", Component: "api-beta", Type: "postgres"},
	}, nil)
	return tg
}

// Taking one of two services out of the manifest used to drop it from the
// only list that remembered it: the database kept running with its data on
// disk, nothing pointed at it, and removing the project left it behind.
func TestAServiceTakenOutOfTheManifestIsRemembered(t *testing.T) {
	got := droppedServices(twoServiceTarget().topologyForResponse(), &manifest{Services: map[string]manifestService{"alpha": {Type: "postgres"}}})
	if len(got) != 1 || got[0].Name != "beta" || !got[0].Dropped {
		t.Fatalf("dropped = %+v, want beta marked dropped", got)
	}
}

// A dropped service is stopped, not deleted, and stays on the topology so
// removal still finds it. With no services left at all this used to return
// before looking, which is the same bug in its one-service form.
func TestADroppedServiceIsStoppedAndKeptOnTheList(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "beta1", Name: "api-beta", Namespace: "acme--prod", Phase: "Running", DesiredState: "Running"},
	}}
	w := testWatcherWithDB(t)
	w.client = d.start(t)
	tg := twoServiceTarget()
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}
	// As the pipeline does before every deploy (pipeline.go): the previous
	// list must come from the stored row. The first version read t and
	// passed this test while finding nothing on a real deploy.
	tg.Topology = ""

	_, topo, err := w.applyServicesTopology(context.Background(), tg, &manifest{}, "acme--prod", "",
		newInterpolationScope(w.newSecretResolver("acme--prod")))
	if err != nil {
		t.Fatalf("applyServicesTopology: %v", err)
	}
	if len(topo) != 2 || !topo[0].Dropped || !topo[1].Dropped {
		t.Fatalf("topology = %+v, want both services kept and marked dropped", topo)
	}
	if !d.asked(http.MethodPost, "/v1/components/beta1/stop") {
		t.Errorf("the dropped service was not stopped; asked: %v", d.requests)
	}
	if d.asked(http.MethodDelete, "/v1/components/beta1") {
		t.Error("a deploy deleted a dropped service; only a confirmed delete may")
	}
}

func deleteService(t *testing.T, w *Watcher, target, service string) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.DELETE("/v1/targets/:name/services/:service", w.handleDeleteDroppedService)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/targets/"+target+"/services/"+service, nil))
	return rec.Code
}

// The confirmed delete takes the container, the data and the password, and
// refuses a service the manifest still declares (the next deploy would only
// create it again, empty).
func TestDeletingADroppedServiceTakesItsData(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "beta1", Name: "api-beta", Namespace: "acme--prod", Phase: "Exited"},
	}}
	w := testWatcherWithDB(t)
	w.client, w.log, w.mountsDir = d.start(t), zap.NewNop(), t.TempDir()
	tg := lifecycleTarget()
	tg.Topology = encodeTopology([]watcherclient.TopologyService{
		{Name: "alpha", Component: "api-alpha", Type: "postgres"},
		{Name: "beta", Component: "api-beta", Type: "postgres", Dropped: true},
	}, nil)
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.saveServiceState(tg, map[string]serviceState{"alpha": {Password: "a"}, "beta": {Password: "b"}}); err != nil {
		t.Fatal(err)
	}
	data := w.volumePath(componentRef{namespace: "acme--prod", name: "api-beta"}, "api-beta-data")
	writeData(t, data)

	if code := deleteService(t, w, tg.Name, "alpha"); code != http.StatusConflict {
		t.Errorf("deleting a declared service = %d, want 409", code)
	}
	if code := deleteService(t, w, tg.Name, "nope"); code != http.StatusNotFound {
		t.Errorf("deleting an unknown service = %d, want 404", code)
	}
	if code := deleteService(t, w, tg.Name, "beta"); code != http.StatusNoContent {
		t.Fatalf("deleting the dropped service = %d", code)
	}
	if !d.asked(http.MethodDelete, "/v1/components/beta1") {
		t.Errorf("component not removed; asked: %v", d.requests)
	}
	if exists(data) {
		t.Error("its data is still on disk")
	}
	row := reload(t, w, tg.ID)
	if topo := row.topologyForResponse(); topo == nil || len(topo.Services) != 1 || topo.Services[0].Name != "alpha" {
		t.Errorf("topology after delete = %+v", topo)
	}
	state, _ := w.loadServiceState(&row)
	if _, ok := state["beta"]; ok || state["alpha"].Password != "a" {
		t.Errorf("service state after delete = %+v", state)
	}
}

// A deploy that fails partway must not shrink the recorded services list.
// The row used to be rewritten as "resolved so far + dropped", so db1 failing
// left only db1: db2 and db3 kept running, with their data on disk, on no
// list that removal or the next dropped calculation reads.
func TestAFailedServiceDeployKeepsEveryRecordedService(t *testing.T) {
	d := &daemonStub{status: http.StatusInternalServerError}
	w := testWatcherWithDB(t)
	w.client = d.start(t)
	tg := lifecycleTarget()
	tg.Topology = encodeTopology([]watcherclient.TopologyService{
		{Name: "db1", Component: "api-db1", Type: "postgres"},
		{Name: "db2", Component: "api-db2", Type: "postgres"},
		{Name: "db3", Component: "api-db3", Type: "postgres"},
	}, nil)
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}
	tg.Topology = "" // as the pipeline clears it before a deploy

	mf := &manifest{Services: map[string]manifestService{
		"db1": {Type: "postgres"}, "db2": {Type: "postgres"}, "db3": {Type: "postgres"},
	}}
	_, _, err := w.applyServicesTopology(context.Background(), tg, mf, "acme--prod", "",
		newInterpolationScope(w.newSecretResolver("acme--prod")))
	if err == nil {
		t.Fatal("deploy against a failing daemon succeeded")
	}
	var row Target
	if err := w.db.First(&row, tg.ID).Error; err != nil {
		t.Fatal(err)
	}
	topo := row.topologyForResponse()
	if topo == nil || len(topo.Services) != 3 {
		t.Fatalf("stored services = %+v, want db1, db2 and db3 all still recorded", topo)
	}
}
