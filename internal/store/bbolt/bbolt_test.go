package bbolt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

// This is the only persistence implementation there is, and everything above
// it is tested against fakes. What these tests are really guarding is the
// round trip: a component written by one process and read back by the next one
// has to come back whole. A field that silently stops surviving JSON, or an
// event log that stops truncating, would otherwise show up as a daemon that
// "forgot" something after a restart.

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "qooq.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleComponent(id, name string) *core.Component {
	tls := true
	return &core.Component{
		ID: id,
		Spec: core.ComponentSpec{
			Namespace: "acme",
			Name:      name,
			Image:     "nginx:alpine",
			Command:   []string{"nginx", "-g", "daemon off;"},
			Env:       map[string]string{"PORT": "8080"},
			Ports: []runtime.PortMapping{
				{HostPort: 19000, ContainerPort: 80, Protocol: "tcp", HostIP: "127.0.0.1"},
			},
			Mounts:        []runtime.Mount{{Source: "/srv/data", Target: "/app/data"}},
			Network:       "qooq-acme",
			Labels:        map[string]string{core.LabelManaged: "true"},
			GroupID:       "grp-1",
			RestartPolicy: "on-failure",
			Domain:        "app.example.com",
			Ingress:       &core.IngressSpec{Hosts: []string{"a.example.com"}, Port: 80, Path: "/api", StripPath: true, TLS: &tls},
			CPULimit:      0.5,
			MemLimitBytes: 67108864,
			HealthCheck:   &core.HealthCheck{Type: "http", HTTPPath: "/health", HTTPPort: 19000},
		},
		Status: core.ComponentStatus{
			ContainerID:  "abc123",
			Phase:        runtime.PhaseRunning,
			DesiredState: core.DesiredRunning,
			CrashCount:   2,
			RestartCount: 1,
			StartedAt:    time.Now().UTC().Truncate(time.Second),
			LastError:    "previous start failed",
		},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
}

func TestComponentSurvivesTheRoundTrip(t *testing.T) {
	s := openTemp(t)
	want := sampleComponent("01ABC", "web")
	if err := s.SaveComponent(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.GetComponent("01ABC")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Spot-check the nested structures rather than only the top level: those
	// are where a serialization change actually loses data.
	if got.Spec.Namespace != want.Spec.Namespace || got.Spec.Name != want.Spec.Name || got.Spec.Image != want.Spec.Image {
		t.Errorf("identity fields changed: %+v", got.Spec)
	}
	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].HostPort != 19000 || got.Spec.Ports[0].HostIP != "127.0.0.1" {
		t.Errorf("ports did not survive: %+v", got.Spec.Ports)
	}
	if got.Spec.Env["PORT"] != "8080" || len(got.Spec.Command) != 3 {
		t.Errorf("env/command did not survive: %+v %+v", got.Spec.Env, got.Spec.Command)
	}
	if got.Spec.HealthCheck == nil || got.Spec.HealthCheck.HTTPPath != "/health" {
		t.Errorf("health check did not survive: %+v", got.Spec.HealthCheck)
	}
	if got.Spec.Ingress == nil || got.Spec.Ingress.Path != "/api" || !got.Spec.Ingress.StripPath {
		t.Errorf("ingress spec did not survive: %+v", got.Spec.Ingress)
	}
	if got.Spec.Ingress.TLS == nil || !*got.Spec.Ingress.TLS {
		t.Errorf("ingress tls pointer did not survive: %+v", got.Spec.Ingress.TLS)
	}
	if len(got.Spec.Mounts) != 1 || got.Spec.Mounts[0].Target != "/app/data" {
		t.Errorf("mounts did not survive: %+v", got.Spec.Mounts)
	}
	if got.Spec.CPULimit != 0.5 || got.Spec.MemLimitBytes != 67108864 {
		t.Errorf("resource limits did not survive: %v %v", got.Spec.CPULimit, got.Spec.MemLimitBytes)
	}
	if got.Status.Phase != runtime.PhaseRunning || got.Status.CrashCount != 2 || got.Status.LastError == "" {
		t.Errorf("status did not survive: %+v", got.Status)
	}
	if !got.Status.StartedAt.Equal(want.Status.StartedAt) {
		t.Errorf("started_at: got %v, want %v", got.Status.StartedAt, want.Status.StartedAt)
	}
}

func TestComponentSurvivesReopeningTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qooq.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.SaveComponent(sampleComponent("01ABC", "web")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.AppendEvent("01ABC", core.Event{ComponentID: "01ABC", Type: "started"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// This is the restart the daemon does every time it is upgraded.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if _, err := reopened.GetComponent("01ABC"); err != nil {
		t.Fatalf("component did not survive a reopen: %v", err)
	}
	events, err := reopened.ListEvents("01ABC")
	if err != nil || len(events) != 1 {
		t.Fatalf("events did not survive a reopen: %d, %v", len(events), err)
	}
}

func TestGetComponentMissingIsErrNotFound(t *testing.T) {
	s := openTemp(t)
	// Callers branch on this with errors.Is; a bare error here turns "no such
	// component" into a 500.
	if _, err := s.GetComponent("nope"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("got %v, want core.ErrNotFound", err)
	}
	if _, err := s.GetGroup("nope"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("group: got %v, want core.ErrNotFound", err)
	}
}

func TestListOnAnEmptyStoreIsEmptyNotAnError(t *testing.T) {
	s := openTemp(t)
	comps, err := s.ListComponents()
	if err != nil || len(comps) != 0 {
		t.Fatalf("components: %d, %v", len(comps), err)
	}
	groups, err := s.ListGroups()
	if err != nil || len(groups) != 0 {
		t.Fatalf("groups: %d, %v", len(groups), err)
	}
	events, err := s.ListEvents("never-existed")
	if err != nil || len(events) != 0 {
		t.Fatalf("events for an unknown component: %d, %v", len(events), err)
	}
}

func TestListComponentsReturnsEveryone(t *testing.T) {
	s := openTemp(t)
	for _, id := range []string{"01A", "01B", "01C"} {
		if err := s.SaveComponent(sampleComponent(id, "c-"+id)); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	got, err := s.ListComponents()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d components, want 3", len(got))
	}
}

func TestSaveComponentOverwritesInPlace(t *testing.T) {
	s := openTemp(t)
	c := sampleComponent("01ABC", "web")
	if err := s.SaveComponent(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	c.Spec.Image = "nginx:1.27"
	c.Status.RestartCount = 9
	if err := s.SaveComponent(c); err != nil {
		t.Fatalf("re-save: %v", err)
	}

	got, err := s.GetComponent("01ABC")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Image != "nginx:1.27" || got.Status.RestartCount != 9 {
		t.Errorf("update was not persisted: %s %d", got.Spec.Image, got.Status.RestartCount)
	}
	all, _ := s.ListComponents()
	if len(all) != 1 {
		t.Errorf("saving the same id twice made %d components", len(all))
	}
}

func TestDeleteComponentTakesItsEventsWithIt(t *testing.T) {
	s := openTemp(t)
	if err := s.SaveComponent(sampleComponent("01ABC", "web")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.AppendEvent("01ABC", core.Event{ComponentID: "01ABC", Type: "started"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.DeleteComponent("01ABC"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetComponent("01ABC"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("component still readable after delete: %v", err)
	}
	// The nested event bucket has to go too, or a recycled id inherits a
	// stranger's history.
	events, err := s.ListEvents("01ABC")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events outlived their component: %d", len(events))
	}
}

func TestDeleteComponentWithoutEventsIsFine(t *testing.T) {
	s := openTemp(t)
	if err := s.SaveComponent(sampleComponent("01ABC", "web")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.DeleteComponent("01ABC"); err != nil {
		t.Fatalf("delete with no event bucket should succeed: %v", err)
	}
	// Deleting something that was never there is how reconcile cleans up.
	if err := s.DeleteComponent("never-existed"); err != nil {
		t.Fatalf("delete of an unknown id should be a no-op: %v", err)
	}
}

func TestGroupRoundTripAndDelete(t *testing.T) {
	s := openTemp(t)
	g := &core.Group{ID: "grp-1", Name: "frontend", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.SaveGroup(g); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.GetGroup("grp-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "frontend" || !got.CreatedAt.Equal(g.CreatedAt) {
		t.Errorf("group did not survive: %+v", got)
	}

	groups, err := s.ListGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("list: %d, %v", len(groups), err)
	}
	if err := s.DeleteGroup("grp-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetGroup("grp-1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("group still readable after delete: %v", err)
	}
}

func TestEventsAreCappedAndKeepTheNewest(t *testing.T) {
	s := openTemp(t)
	total := maxEventsPerComponent + 50
	for i := 0; i < total; i++ {
		e := core.Event{ComponentID: "01ABC", Type: "tick", Message: fmt.Sprintf("event-%d", i)}
		if err := s.AppendEvent("01ABC", e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := s.ListEvents("01ABC")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The cap is what stops one crash-looping component from growing the file
	// without bound.
	if len(got) != maxEventsPerComponent {
		t.Fatalf("got %d events, want the cap of %d", len(got), maxEventsPerComponent)
	}
	// Truncation must drop the oldest, not the newest: the recent ones are the
	// ones anybody debugging is looking for. Order is oldest-first.
	if want := fmt.Sprintf("event-%d", total-maxEventsPerComponent); got[0].Message != want {
		t.Errorf("first retained event is %q, want %q", got[0].Message, want)
	}
	if want := fmt.Sprintf("event-%d", total-1); got[len(got)-1].Message != want {
		t.Errorf("last event is %q, want %q", got[len(got)-1].Message, want)
	}
}

func TestEventsAreScopedToTheirComponent(t *testing.T) {
	s := openTemp(t)
	if err := s.AppendEvent("01A", core.Event{ComponentID: "01A", Type: "started"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.AppendEvent("01B", core.Event{ComponentID: "01B", Type: "died", ExitCode: 137}); err != nil {
		t.Fatalf("append: %v", err)
	}

	a, err := s.ListEvents("01A")
	if err != nil || len(a) != 1 || a[0].Type != "started" {
		t.Fatalf("component A events: %+v, %v", a, err)
	}
	b, err := s.ListEvents("01B")
	if err != nil || len(b) != 1 || b[0].ExitCode != 137 {
		t.Fatalf("component B events: %+v, %v", b, err)
	}
}

func TestOpenCreatesTheParentDirectory(t *testing.T) {
	// `-data /some/new/dir/qooq.db` has to work without the operator creating
	// the path first; bolt.Open only creates the file.
	path := filepath.Join(t.TempDir(), "nested", "deeper", "qooq.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open into a missing directory: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file was not created: %v", err)
	}
}

func TestBackupProducesAReadableDatabase(t *testing.T) {
	s := openTemp(t)
	if err := s.SaveComponent(sampleComponent("01ABC", "web")); err != nil {
		t.Fatalf("save: %v", err)
	}

	var buf bytes.Buffer
	var hinted int64
	if err := s.Backup(&buf, func(n int64) { hinted = n }); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if hinted <= 0 {
		t.Errorf("size hint was %d; it becomes the Content-Length of the download", hinted)
	}
	if int64(buf.Len()) != hinted {
		t.Errorf("wrote %d bytes but announced %d — a truncated download would look complete", buf.Len(), hinted)
	}

	// A backup nobody can open is not a backup: restore is "copy this file
	// over the data path", so it has to be a valid database on its own.
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restored, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write restored file: %v", err)
	}
	rs, err := Open(restored)
	if err != nil {
		t.Fatalf("open the backup: %v", err)
	}
	defer rs.Close()
	if _, err := rs.GetComponent("01ABC"); err != nil {
		t.Fatalf("component missing from the backup: %v", err)
	}
}
