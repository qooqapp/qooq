package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// imageDaemon is a qooqd for image deploys: the component runs whatever image
// was last upserted, and an image listed in broken exits instead of running.
// Rollback puts the previous image back, as qooqd does.
type imageDaemon struct {
	mu       sync.Mutex
	image    string
	previous string
	digests  map[string]string
	broken   map[string]bool
	upserts  []string
	rollback bool
}

func (d *imageDaemon) start(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		comp := func() client.ComponentResponse {
			phase := "Running"
			if d.broken[d.image] {
				phase = "Exited"
			}
			return client.ComponentResponse{ID: "c1", Name: "img", Image: d.image, PreviousImage: d.previous,
				Phase: phase, ContainerID: d.image, ExitCode: map[bool]int{true: 1}[phase == "Exited"]}
		}
		switch {
		case r.URL.Path == "/v1/images/digest":
			var req client.ImageDigestRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(client.ImageDigestResponse{Digest: d.digests[req.Ref]})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/components":
			var req client.DeployRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			d.upserts = append(d.upserts, req.Image)
			d.previous, d.image = d.image, req.Image
			_ = json.NewEncoder(w).Encode(comp())
		case strings.HasSuffix(r.URL.Path, "/rollback"):
			d.rollback = true
			d.previous, d.image = d.image, d.previous
			_ = json.NewEncoder(w).Encode(comp())
		case strings.HasPrefix(r.URL.Path, "/v1/components/c1"):
			if strings.HasSuffix(r.URL.Path, "/logs") {
				_, _ = w.Write([]byte("panic: boom\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(comp())
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

func imageTarget(t *testing.T, w *Watcher) *Target {
	t.Helper()
	tg := &Target{Name: "img", ComponentName: "img", DeployMode: DeployModeImage,
		ImageRef: "registry.local/app:1.0", LastImageDigest: "sha256:aaaaaaaaaa",
		LastBuildRevision: "sha256:aaaaaaaaaa", BuildGeneration: 2, LastBuildStatus: BuildStatusSuccess}
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}
	return tg
}

func ciDeploy(t *testing.T, w *Watcher, image string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/targets/:name/deploy", w.handleDeployTarget)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/targets/img/deploy", strings.NewReader(`{"image":"`+image+`"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

func reload(t *testing.T, w *Watcher, id uint) Target {
	t.Helper()
	var row Target
	if err := w.db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

// A deploy CI asks for by image is a deploy like any other: the generation
// moves and the revision names what went out. It used to leave both alone, so
// `-o json` kept describing the previous version and anything waiting on
// build_generation (the CLI's --wait) never saw the deploy happen.
func TestAnImageDeployFromCIMovesTheGenerationAndRevision(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	quickStable(t, 20*time.Millisecond)
	d := &imageDaemon{image: "registry.local/app:1.0",
		digests: map[string]string{"registry.local/app:pinned": "sha256:bbbbbbbbbb"}}
	w := &Watcher{db: newTestDB(t), client: d.start(t), log: zap.NewNop()}
	tg := imageTarget(t, w)

	if rec := ciDeploy(t, w, "registry.local/app:pinned"); rec.Code != http.StatusOK {
		t.Fatalf("deploy = %d %s", rec.Code, rec.Body.String())
	}
	row := reload(t, w, tg.ID)
	if row.BuildGeneration != 3 {
		t.Errorf("build_generation = %d, want 3", row.BuildGeneration)
	}
	if row.LastBuildRevision != "sha256:bbbbbbbbbb" || row.LastImageDigest != "sha256:bbbbbbbbbb" {
		t.Errorf("revision=%q digest=%q, want the pinned image's digest", row.LastBuildRevision, row.LastImageDigest)
	}
	if row.LastBuildStatus != BuildStatusSuccess || row.ImageRef != "registry.local/app:pinned" {
		t.Errorf("status=%q ref=%q", row.LastBuildStatus, row.ImageRef)
	}
}

// An image CI picks that does not start is rolled back, like a git build. The
// answer says failed with the reason, and the target keeps following what runs.
func TestABrokenImageFromCIIsRolledBack(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	quickStable(t, 20*time.Millisecond)
	d := &imageDaemon{image: "registry.local/app:1.0", previous: "registry.local/app:0.9",
		digests: map[string]string{"registry.local/app:bad": "sha256:cccccccccc"},
		broken:  map[string]bool{"registry.local/app:bad": true}}
	w := &Watcher{db: newTestDB(t), client: d.start(t), log: zap.NewNop()}
	tg := imageTarget(t, w)

	rec := ciDeploy(t, w, "registry.local/app:bad")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"last_build_status":"failed"`) {
		t.Fatalf("deploy = %d %s", rec.Code, rec.Body.String())
	}
	if !d.rollback || d.image != "registry.local/app:1.0" {
		t.Fatalf("not rolled back: running %q", d.image)
	}
	row := reload(t, w, tg.ID)
	if !strings.Contains(row.LastBuildError, "was put back") || !strings.Contains(row.LastBuildError, "panic: boom") {
		t.Errorf("error does not say what happened: %s", row.LastBuildError)
	}
	if row.ImageRef != "registry.local/app:1.0" || row.LastImageDigest != "sha256:aaaaaaaaaa" {
		t.Errorf("the target no longer follows what runs: ref=%q digest=%q", row.ImageRef, row.LastImageDigest)
	}
}

// On a moving tag the new image has the same name as the old one, so "the
// previous image" is the broken one. The previous version is put back by its
// digest instead, and polling does not deploy the broken digest again.
func TestAMovingTagIsRolledBackByDigest(t *testing.T) {
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	quickStable(t, 20*time.Millisecond)
	d := &imageDaemon{image: "registry.local/app:latest", previous: "registry.local/app:latest",
		broken: map[string]bool{"registry.local/app:latest": true}}
	w := &Watcher{db: newTestDB(t), client: d.start(t), log: zap.NewNop()}
	tg := imageTarget(t, w)
	tg.ImageRef = "registry.local/app:latest"

	err := w.runImageDeploy(context.Background(), tg, "", "", "sha256:dddddddddd")
	if err == nil || !strings.Contains(err.Error(), "sha256:aaaaaaa") || !strings.Contains(err.Error(), "was put back") {
		t.Fatalf("err = %v", err)
	}
	want := "registry.local/app@sha256:aaaaaaaaaa"
	if d.image != want {
		t.Fatalf("running %q after the rollback, want %q (upserts: %v)", d.image, want, d.upserts)
	}
	if tg.RejectedCommit != "sha256:dddddddddd" {
		t.Errorf("broken digest not rejected: %q", tg.RejectedCommit)
	}
}

func TestImageRepoKeepsTheRegistryPort(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx:1.27":                    "nginx",
		"reg.local:5000/team/app:1.2":   "reg.local:5000/team/app",
		"reg.local:5000/team/app":       "reg.local:5000/team/app",
		"reg.local/app@sha256:abc":      "reg.local/app",
		"reg.local:5000/app:v@sha256:a": "reg.local:5000/app",
	} {
		if got := imageRepo0(ref); got != want {
			t.Errorf("imageRepo0(%q) = %q, want %q", ref, got, want)
		}
	}
}
