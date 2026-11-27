package watcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// Two standalone targets that reference each other's endpoint wait for each
// other forever on a first deploy. Each one alone reads "not deployed yet",
// which promises it will fix itself; it never does. Once both have recorded
// what they wait for, the message has to name the loop.
func TestCircularComponentReferencesAreNamed(t *testing.T) {
	qooqd := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte("[]")) // nothing is deployed yet
	}))
	defer qooqd.Close()
	w := newStatusWatcher(t)
	w.client = client.New(qooqd.URL)

	// deploy runs the same steps the pipeline does: resolve, then record.
	deploy := func(tg *Target, ref string) string {
		t.Helper()
		mf := &manifest{App: tg.ComponentName}
		mf.Run.Env = map[string]string{"PEER_URL": "${component." + ref + ".endpoint}"}
		_, err := w.applyRunConfig(context.Background(), tg, mf, deploySpecFields{Namespace: "default"}, t.TempDir(), "abc1234")
		var needs errNeedsInput
		if !errors.As(err, &needs) {
			t.Fatalf("%s: err = %v, want needs_input", tg.Name, err)
		}
		w.recordNeedsInput(tg, err.Error(), needs.waitingOn)
		return getTargetByID(t, w.db, tg.ID).LastBuildError
	}

	api := &Target{Name: "api", ComponentName: "api", DeploySpec: `{"namespace":"default"}`}
	web := &Target{Name: "web", ComponentName: "web", DeploySpec: `{"namespace":"default"}`}
	saveTarget(t, w, api)
	saveTarget(t, w, web)

	// The first one tried cannot know yet: the other has not said what it waits for.
	if msg := deploy(api, "web"); strings.Contains(msg, "circular") || !strings.Contains(msg, `"web" is not deployed`) {
		t.Errorf("api, first: %q", msg)
	}
	if got := getTargetByID(t, w.db, api.ID).WaitingOn; got != "web" {
		t.Errorf("api waiting_on = %q, want web", got)
	}
	// The second closes the loop, and the first sees it on its next sweep.
	if msg := deploy(web, "api"); !strings.Contains(msg, "circular reference") || !strings.Contains(msg, "web -> api -> web") {
		t.Errorf("web: %q", msg)
	}
	if msg := deploy(api, "web"); !strings.Contains(msg, "api -> web -> api") {
		t.Errorf("api, second: %q", msg)
	}

	// Waiting on a member of the loop without being in it is still just waiting.
	worker := &Target{Name: "worker", ComponentName: "worker", DeploySpec: `{"namespace":"default"}`}
	saveTarget(t, w, worker)
	if msg := deploy(worker, "api"); strings.Contains(msg, "circular") {
		t.Errorf("worker is not in the loop: %q", msg)
	}

	// The same names in another namespace are another deployment.
	other := &Target{Name: "other_web", ComponentName: "web", DeploySpec: `{"namespace":"other"}`}
	saveTarget(t, w, other)
	mf := &manifest{App: "web"}
	mf.Run.Env = map[string]string{"PEER_URL": "${component.api.endpoint}"}
	_, err := w.applyRunConfig(context.Background(), other, mf, deploySpecFields{Namespace: "other"}, t.TempDir(), "abc1234")
	if err == nil || strings.Contains(err.Error(), "circular") {
		t.Errorf("other namespace: %v", err)
	}
}

func TestFindCycle(t *testing.T) {
	edges := map[string][]string{"a": {"b"}, "b": {"c", "x"}, "c": {"a"}, "x": {"x"}}
	if got := strings.Join(findCycle(edges, "a"), ","); got != "a,b,c,a" {
		t.Errorf("got %q", got)
	}
	// A loop elsewhere that never returns to start is not start's cycle.
	if got := findCycle(map[string][]string{"a": {"x"}, "x": {"x"}}, "a"); got != nil {
		t.Errorf("got %v", got)
	}
}

// An app whose address is reserved but which is not deployed yet resolves
// like a deployed one: that is what breaks the loop above on its own.
func TestAReservedAddressResolvesBeforeTheDeploy(t *testing.T) {
	qooqd := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/reservations" {
			rw.Write([]byte(`[{"namespace":"default","name":"web","phase":"Reserved",
				"ports":[{"container_port":3000,"host_port":19001}],
				"endpoints":["http://web.localhost","http://192.0.2.10:19001"]}]`))
			return
		}
		rw.Write([]byte("[]"))
	}))
	defer qooqd.Close()
	w := newStatusWatcher(t)
	w.client = client.New(qooqd.URL)

	api := &Target{Name: "api", ComponentName: "api", DeploySpec: `{"namespace":"default"}`}
	saveTarget(t, w, api)
	mf := &manifest{App: "api"}
	mf.Run.Env = map[string]string{
		"ALLOW_ORIGIN": "${component.web.endpoint}",
		"WEB_PORT":     "${component.web.host_port}",
		"WEB_ADDR":     "${component.web.address}",
	}
	spec, err := w.applyRunConfig(context.Background(), api, mf, deploySpecFields{Namespace: "default"}, t.TempDir(), "abc1234")
	if err != nil {
		t.Fatalf("a reserved address did not resolve: %v", err)
	}
	for k, want := range map[string]string{"ALLOW_ORIGIN": "http://web.localhost", "WEB_PORT": "19001", "WEB_ADDR": "web:3000"} {
		if spec.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, spec.Env[k], want)
		}
	}
}
