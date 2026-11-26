package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
	"go.uber.org/zap"
)

// recordingDaemon is a qooqd that answers everything blandly and remembers
// which paths it was asked for. The point of these tests is never the reply —
// it is which of two machines received the call.
type recordingDaemon struct {
	mu   sync.Mutex
	hits []string
}

func (d *recordingDaemon) start(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.hits = append(d.hits, r.Method+" "+r.URL.Path)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/images":
			_ = json.NewEncoder(w).Encode([]client.ImageResponse{
				{Tags: []string{"acme-api:oldoldo"}},
			})
		case "/v1/components":
			_ = json.NewEncoder(w).Encode([]client.ComponentResponse{})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

func (d *recordingDaemon) saw(method, path string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range d.hits {
		if h == method+" "+path {
			return true
		}
	}
	return false
}

func (d *recordingDaemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.hits)
}

func splitWatcher(t *testing.T) (*Watcher, *recordingDaemon, *recordingDaemon) {
	t.Helper()
	deploy, build := &recordingDaemon{}, &recordingDaemon{}
	w := &Watcher{log: zap.NewNop()}
	w.client = deploy.start(t)
	w.buildClient = build.start(t)
	return w, deploy, build
}

// The regression guard for every offline install: no build address means the
// code path is not merely equivalent to the old one, it is the same object.
func TestWithoutABuildAddressThereIsOnlyOneNode(t *testing.T) {
	deploy := &recordingDaemon{}
	w := &Watcher{log: zap.NewNop()}
	w.client = deploy.start(t)

	if w.buildNode() != w.client {
		t.Fatal("buildNode() must be the deploy client when no build address is set")
	}
	if w.buildNodeIsSeparate() {
		t.Fatal("buildNodeIsSeparate() must be false when no build address is set")
	}
	if hosts := w.imageHosts(); len(hosts) != 1 {
		t.Fatalf("imageHosts() = %d hosts, want 1 — a single machine must be walked once, not twice", len(hosts))
	}
}

func TestWithABuildAddressTheBuildNodeIsTheOtherOne(t *testing.T) {
	w, _, _ := splitWatcher(t)
	if w.buildNode() == w.client {
		t.Fatal("buildNode() must not be the deploy client once a build address is configured")
	}
	if !w.buildNodeIsSeparate() {
		t.Fatal("buildNodeIsSeparate() must be true once a build address is configured")
	}
}

// Pushing from the deploy node cannot work: at this point the image exists
// only where it was built. Getting this wrong fails every build in registry
// mode, and only in registry mode.
func TestThePushHappensOnTheBuildNode(t *testing.T) {
	w, deploy, build := splitWatcher(t)
	w.registry = registryConfig{Prefix: "reg.example:5000/qooq"}

	ref, err := w.pushBuiltImage(context.Background(), "acme-api:abc1234")
	if err != nil {
		t.Fatalf("pushBuiltImage: %v", err)
	}
	if ref != "reg.example:5000/qooq/acme-api:abc1234" {
		t.Fatalf("ref = %q, want the remote ref", ref)
	}
	if !build.saw("POST", "/v1/images/push") {
		t.Error("the build node never received the push")
	}
	if deploy.count() != 0 {
		t.Errorf("the deploy node was called during a push: %d request(s)", deploy.count())
	}
}

// The registry tag is a second name on the build node and the name the deploy
// node pulled, so both have to be swept. Pruning only where the build ran
// leaves the deploy node growing by one image per build — a disk leak created
// by the change meant to contain one.
func TestImageRetentionWalksBothNodes(t *testing.T) {
	w, _, _ := splitWatcher(t)
	hosts := w.imageHosts()
	if len(hosts) != 2 {
		t.Fatalf("imageHosts() = %d hosts, want 2 (build and deploy)", len(hosts))
	}
	labels := []string{hosts[0].label, hosts[1].label}
	if labels[0] != "build" || labels[1] != "deploy" {
		t.Fatalf("labels = %v, want [build deploy] — the labels end up in prune logs", labels)
	}
	if hosts[0].c != w.buildNode() || hosts[1].c != w.client {
		t.Fatal("imageHosts() must pair each label with the matching client")
	}
}

// pruneOldImages must act on the host it is handed and no other, or the two
// passes above collapse into two passes over the same machine.
func TestPruneActsOnlyOnTheHostItIsGiven(t *testing.T) {
	w, deploy, build := splitWatcher(t)

	w.pruneOldImages(context.Background(),
		imageHost{c: w.buildNode(), label: "build"},
		"acme-api", "acme-api:new1234", "")

	if !build.saw("GET", "/v1/images") {
		t.Error("the build node was never listed")
	}
	if !build.saw("DELETE", "/v1/images") {
		t.Error("the stale tag was never removed on the build node")
	}
	if deploy.count() != 0 {
		t.Errorf("the deploy node was touched by a build-node prune: %d request(s)", deploy.count())
	}
}

// The inverse mistake, and the expensive one: routing deploy-side reads at the
// build node would have the external manager reporting the state of a machine
// that runs nothing.
func TestRuntimeStateReadsTheDeployNode(t *testing.T) {
	w, deploy, build := splitWatcher(t)

	w.attachRuntimeState(context.Background(), []TargetResponse{
		{ComponentName: "api", Namespace: "acme--prod"},
	})

	if !deploy.saw("GET", "/v1/components") {
		t.Error("runtime state must come from the deploy node")
	}
	if build.count() != 0 {
		t.Errorf("the build node was asked about running components: %d request(s)", build.count())
	}
}

// Removing a project takes the images its builds left (the two every redeploy
// keeps), and only for a target qooq built: an image-mode target deploys
// somebody else's image.
func TestRemovingATargetRemovesItsBuiltImages(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		delete bool
	}{{DeployModeUpload, true}, {"source", true}, {DeployModeImage, false}} {
		deploy := &recordingDaemon{}
		w := &Watcher{log: zap.NewNop()}
		w.client = deploy.start(t)
		w.removeBuiltImages(context.Background(), &Target{
			DeployMode: tc.mode, ComponentName: "api", DeploySpec: `{"namespace":"acme"}`,
		})
		if got := deploy.saw("DELETE", "/v1/images"); got != tc.delete {
			t.Errorf("%s: removed an image = %v, want %v", tc.mode, got, tc.delete)
		}
	}
}
