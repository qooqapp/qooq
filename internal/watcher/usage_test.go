package watcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// The operator's usage list: every component with its sample, its data, and
// the target it belongs to — a service under its app's target, and each
// matched on (namespace, name), so bob's "api" is not filed under alice's.
func TestComponentUsageJoinsStatsDataAndOwner(t *testing.T) {
	w := testWatcher(t, "open")
	w.db = newTestDB(t)
	comps := []client.ComponentResponse{
		{ID: "c1", Name: "api", Namespace: "alice--prod", Phase: "Running", MemLimitBytes: 512 << 20},
		{ID: "c2", Name: "api-db", Namespace: "alice--prod", Phase: "Running"},
		{ID: "c3", Name: "api", Namespace: "bob--prod", Phase: "Exited", DesiredState: "Stopped"},
		{ID: "c4", Name: "gone", Namespace: "bob--prod", Phase: "Removed"},
	}
	var statsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/components":
			_ = json.NewEncoder(rw).Encode(comps)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			statsCalls.Add(1)
			id := strings.Split(r.URL.Path, "/")[3]
			_ = json.NewEncoder(rw).Encode(client.StatsResponse{ComponentID: id, CPUPercent: 12.5, MemUsage: 100 << 20, Time: time.Now()})
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	w.client = client.New(srv.URL)

	built := time.Now().Add(-48 * time.Hour)
	if err := w.db.Create(&Target{
		Name: "alice--prod_api", ComponentName: "api", DeploySpec: `{"namespace":"alice--prod"}`,
		Topology: `{"services":[{"name":"db","component":"api-db","type":"postgres"}]}`, LastBuildAt: built,
	}).Error; err != nil {
		t.Fatal(err)
	}
	w.disk.byNS = map[string]int64{"alice--prod": 3000}
	w.disk.byComp = map[string]int64{"alice--prod/api-db": 3000}
	w.disk.measuredAt = time.Now()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/u", w.handleComponentUsage)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/u", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got watcherclient.ComponentUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Components) != 3 {
		t.Fatalf("want 3 components (removed left out), got %+v", got.Components)
	}
	by := map[string]watcherclient.ComponentUsage{}
	for _, u := range got.Components {
		by[u.Namespace+"/"+u.Component] = u
	}
	app, db, bob := by["alice--prod/api"], by["alice--prod/api-db"], by["bob--prod/api"]
	if app.Target != "alice--prod_api" || app.Service || app.LastDeployAt == nil || !app.LastDeployAt.Equal(built) {
		t.Errorf("app row: %+v", app)
	}
	if db.Target != "alice--prod_api" || !db.Service || db.DataBytes == nil || *db.DataBytes != 3000 {
		t.Errorf("service row: %+v", db)
	}
	if app.CPUPercent == nil || *app.CPUPercent != 12.5 || app.MemUsage == nil || app.MemLimit != 512<<20 {
		t.Errorf("app sample: %+v", app)
	}
	if bob.Target != "" || bob.CPUPercent != nil || bob.DataBytes == nil || *bob.DataBytes != 0 {
		t.Errorf("bob's stopped api: %+v", bob)
	}
	if n := statsCalls.Load(); n != 2 {
		t.Errorf("stats asked %d times, want only the 2 running components", n)
	}
}
