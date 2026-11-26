package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// newCrossRef stands up a qooqd that reports comps, and returns a resolver
// pointed at it plus a counter of how many times the component list was asked
// for.
func newCrossRef(t *testing.T, namespace string, comps []client.ComponentResponse) (*componentResolver, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/components" {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(comps)
	}))
	t.Cleanup(srv.Close)

	return &componentResolver{
		ctx:       context.Background(),
		client:    client.New(srv.URL),
		namespace: namespace,
	}, &calls
}

func apiComponent() client.ComponentResponse {
	return client.ComponentResponse{
		Namespace: "default",
		Name:      "api",
		Phase:     "Running",
		Ports:     []client.PortSpec{{ContainerPort: 8080, HostPort: 19000}},
		Endpoints: []string{"http://192.168.1.50:19000"},
	}
}

// The five fields exist because "the address of api" is two different answers
// depending on who is asking, and both are needed in the same manifest.
func TestCrossRefFields(t *testing.T) {
	r, _ := newCrossRef(t, "default", []client.ComponentResponse{apiComponent()})

	for _, tc := range []struct{ ref, want string }{
		{"api.host", "api"},
		{"api.port", "8080"},
		{"api.address", "api:8080"},
		{"api.host_port", "19000"},
		{"api.endpoint", "http://192.168.1.50:19000"},
	} {
		got, ok := r.resolve(tc.ref)
		if !ok {
			t.Errorf("%s did not resolve", tc.ref)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.ref, got, tc.want)
		}
	}
	if w := r.waitingFor(); w != "" {
		t.Errorf("nothing should be pending, got %q", w)
	}
}

// A component that is not deployed yet is an ordering problem that fixes
// itself, so it has to be distinguishable from a typo: the deploy waits
// instead of failing.
func TestCrossRefRecordsWhatItIsWaitingFor(t *testing.T) {
	r, _ := newCrossRef(t, "default", []client.ComponentResponse{apiComponent()})

	if _, ok := r.resolve("worker.host"); ok {
		t.Fatal("a component that does not exist resolved")
	}
	w := r.waitingFor()
	if !strings.Contains(w, "worker") {
		t.Errorf("waitingFor = %q, want it to name worker", w)
	}
	if !strings.Contains(w, "default") {
		t.Errorf("waitingFor = %q, want it to name the namespace", w)
	}

	// Referencing the same missing component twice names it once.
	_, _ = r.resolve("worker.port")
	if got := strings.Count(r.waitingFor(), "worker"); got != 1 {
		t.Errorf("worker named %d times, want 1", got)
	}
}

// Identity is (namespace, name): two stacks each having an "api" is two
// components, and a manifest must never reach the other one's address.
func TestCrossRefIsScopedToItsNamespace(t *testing.T) {
	other := apiComponent()
	other.Namespace = "other"
	other.Ports = []client.PortSpec{{ContainerPort: 8080, HostPort: 29000}}

	r, _ := newCrossRef(t, "default", []client.ComponentResponse{other})
	if got, ok := r.resolve("api.host_port"); ok {
		t.Errorf("resolved another namespace's api to %q", got)
	}
}

func TestCrossRefIgnoresRemoved(t *testing.T) {
	gone := apiComponent()
	gone.Phase = "Removed"

	r, _ := newCrossRef(t, "default", []client.ComponentResponse{gone})
	if _, ok := r.resolve("api.host"); ok {
		t.Error("a removed component still answered")
	}
}

// A manifest naming several fields of several components costs one round trip.
func TestCrossRefAsksOnce(t *testing.T) {
	r, calls := newCrossRef(t, "default", []client.ComponentResponse{apiComponent()})
	for _, ref := range []string{"api.host", "api.port", "api.endpoint", "missing.host"} {
		_, _ = r.resolve(ref)
	}
	if *calls != 1 {
		t.Errorf("asked qooqd %d times, want 1", *calls)
	}
}

// A component with no published host port has no host_port and no endpoint,
// and must say so rather than expanding to something plausible like 0.
func TestCrossRefWithoutAPublishedPort(t *testing.T) {
	private := apiComponent()
	private.Ports = []client.PortSpec{{ContainerPort: 8080}}
	private.Endpoints = nil

	r, _ := newCrossRef(t, "default", []client.ComponentResponse{private})
	if got, ok := r.resolve("api.host_port"); ok {
		t.Errorf("host_port resolved to %q for an unpublished port", got)
	}
	if got, ok := r.resolve("api.endpoint"); ok {
		t.Errorf("endpoint resolved to %q with no published port", got)
	}
	// The in-network address still works — that is the whole point of it.
	if got, ok := r.resolve("api.address"); !ok || got != "api:8080" {
		t.Errorf("address = %q (%v), want api:8080", got, ok)
	}
}

// ${component} is this component's own name and predates the prefix; the two
// must not shadow each other.
func TestCrossRefDoesNotShadowTheBareComponentKey(t *testing.T) {
	r, _ := newCrossRef(t, "default", []client.ComponentResponse{apiComponent()})
	scope := newInterpolationScope(processEnvOnlyResolver())
	scope.set("component", "web")
	scope.setPrefix("component.", r.resolve)

	got, err := scope.expand("${component} talks to ${component.api.address}")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != "web talks to api:8080" {
		t.Errorf("got %q", got)
	}
}

// A name spelled right for something not deployed yet and a name spelled wrong
// look identical from outside the resolver, and the generic
// "unknown reference (available: ...)" is actively misleading for the first —
// it sends someone hunting for a typo in a line that is correct.
func TestCrossRefExplainsWhichKindOfFailureItIs(t *testing.T) {
	r, _ := newCrossRef(t, "default", []client.ComponentResponse{apiComponent()})
	_, _ = r.resolve("worker.host") // load the cache the way expand would

	for _, tc := range []struct{ ref, want string }{
		{"component.worker.host", "not deployed"},
		{"component.api.hst", "not a field"},
		{"component.api", "expected component.<name>.<field>"},
	} {
		got, ok := r.explain(tc.ref)
		if !ok {
			t.Errorf("%s was not explained", tc.ref)
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s explained as %q, want it to mention %q", tc.ref, got, tc.want)
		}
	}

	// References this resolver does not own are left to the generic message.
	if _, ok := r.explain("secret.TOKEN"); ok {
		t.Error("explained a reference it does not own")
	}
}

// A deployed component whose port is not published has no host_port: the
// explanation must say that, not claim the component is missing.
func TestCrossRefExplainsAnUnpublishedPort(t *testing.T) {
	private := apiComponent()
	private.Ports = []client.PortSpec{{ContainerPort: 8080}}
	private.Endpoints = nil

	r, _ := newCrossRef(t, "default", []client.ComponentResponse{private})
	_, _ = r.resolve("api.host_port")

	got, ok := r.explain("component.api.host_port")
	if !ok {
		t.Fatal("not explained")
	}
	if strings.Contains(got, "not deployed") {
		t.Errorf("a running component was described as not deployed: %q", got)
	}
	if !strings.Contains(got, "no host port") {
		t.Errorf("explanation = %q, want it to name the real cause", got)
	}
}

// explain runs after resolve has already recorded what is missing; it must not
// add to that list, or a deploy would report waiting for something merely
// because the error message was built.
func TestCrossRefExplainDoesNotAddToPending(t *testing.T) {
	r, _ := newCrossRef(t, "default", []client.ComponentResponse{apiComponent()})
	_, _ = r.resolve("api.host") // prime the cache without a miss
	before := r.waitingFor()

	_, _ = r.explain("component.ghost.host")

	if r.waitingFor() != before {
		t.Errorf("explain changed what the deploy waits for: %q -> %q", before, r.waitingFor())
	}
}
