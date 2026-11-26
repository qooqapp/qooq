package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

func writeData(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// Removing a project deletes the data qooq kept for it — its database's data
// directory included. Left behind, a database recreated under the same name
// found the old data, ignored the newly generated password, and the app could
// never log in; a shared-node user had no way to reach that directory.
func TestRemovingAProjectDeletesItsData(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "app", Name: "api", Namespace: "acme--prod", Phase: "Running"},
		{ID: "db", Name: "api-db", Namespace: "acme--prod", Phase: "Running"},
	}}
	mounts := t.TempDir()
	w := &Watcher{client: d.start(t), log: zap.NewNop(), mountsDir: mounts}
	tg := lifecycleTarget()
	tg.Topology = encodeTopology([]watcherclient.TopologyService{
		{Name: "db", Component: "api-db", Type: "postgres"},
	}, nil)

	dbData := w.volumePath(componentRef{namespace: "acme--prod", name: "api-db"}, "api-db-data")
	appData := w.volumePath(componentRef{namespace: "acme--prod", name: "api"}, "uploads")
	otherNS := w.volumePath(componentRef{namespace: "acme--staging", name: "api-db"}, "api-db-data")
	otherApp := w.volumePath(componentRef{namespace: "acme--prod", name: "api-gateway"}, "data")
	for _, dir := range []string{dbData, appData, otherNS, otherApp} {
		writeData(t, dir)
	}

	if err := w.removeComponentFor(context.Background(), tg); err != nil {
		t.Fatalf("removeComponentFor: %v", err)
	}
	for _, dir := range []string{dbData, appData} {
		if exists(dir) {
			t.Errorf("data of a removed component is still on disk: %s", dir)
		}
	}
	for _, dir := range []string{otherNS, otherApp} {
		if !exists(dir) {
			t.Errorf("deleted data that is not this project's: %s", dir)
		}
	}
}

// A database whose data directory already has data, and whose password is no
// longer known, fails with the reason instead of deploying and leaving the app
// with "password authentication failed".
func TestOldDataWithANewPasswordIsRefusedWithTheReason(t *testing.T) {
	mounts := t.TempDir()
	w := &Watcher{log: zap.NewNop(), mountsDir: mounts}
	tg := lifecycleTarget()
	decl := manifestService{Type: "postgres"}
	writeData(t, w.volumePath(componentRef{namespace: "acme--prod", name: "api-db"}, "api-db-data"))

	svc, _, err := w.resolveService(tg, "acme--prod", "db", decl, map[string]serviceState{}, nil)
	if err == nil || !strings.Contains(err.Error(), "earlier deployment") || !strings.Contains(err.Error(), "remove this service") {
		t.Fatalf("err = %v, want the old-data reason", err)
	}
	if svc.component != "api-db" {
		t.Errorf("component not reported with the error (%q): removal needs it to find the data", svc.component)
	}
	if strings.Contains(err.Error(), mounts) {
		t.Errorf("the error shows a host path to the user: %v", err)
	}

	// The same data with its password still known is the normal redeploy.
	if _, _, err := w.resolveService(tg, "acme--prod", "db", decl,
		map[string]serviceState{"db": {Password: "kept"}}, nil); err != nil {
		t.Fatalf("redeploy with the kept password: %v", err)
	}
	// And an empty directory is a first deploy.
	w2 := &Watcher{log: zap.NewNop(), mountsDir: t.TempDir()}
	if _, _, err := w2.resolveService(tg, "acme--prod", "db", decl, map[string]serviceState{}, nil); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
}

// The services list reaches the row while the build is still running, so a
// first build that fails still leaves removal something to read; references
// already on the row are kept.
func TestServicesAreRecordedBeforeTheBuildFinishes(t *testing.T) {
	db := newTestDB(t)
	w := &Watcher{db: db, log: zap.NewNop()}
	tg := &Target{Name: "svctest", RepoURL: "file:///x", ComponentName: "svctest",
		Topology: encodeTopology(nil, []string{"other"})}
	if err := db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}

	w.persistServices(tg, []watcherclient.TopologyService{{Name: "db", Component: "svctest-db", Type: "postgres"}})

	var row Target
	if err := db.First(&row, tg.ID).Error; err != nil {
		t.Fatal(err)
	}
	topo := row.topologyForResponse()
	if topo == nil || len(topo.Services) != 1 || topo.Services[0].Component != "svctest-db" {
		t.Fatalf("services not recorded: %+v", topo)
	}
	if len(topo.References) != 1 || topo.References[0] != "other" {
		t.Errorf("references on the row were dropped: %+v", topo.References)
	}
}
