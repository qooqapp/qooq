package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

const sameDigest = "sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10"

func imageForceRig(t *testing.T, force bool) (*Watcher, *Target, *[]string) {
	t.Helper()
	quickTimeouts(t, time.Second, time.Second, 5*time.Millisecond)
	quickStable(t, 20*time.Millisecond)
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/images/digest":
			_ = json.NewEncoder(w).Encode(client.ImageDigestResponse{Digest: sameDigest})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/components":
			_ = json.NewEncoder(w).Encode([]client.ComponentResponse{
				{ID: "c1", Name: "web", Namespace: "default", Phase: "Running"}})
		default:
			// Running on one container: the deploy is followed until the
			// new version is shown to stay up (newversion.go).
			_ = json.NewEncoder(w).Encode(client.ComponentResponse{ID: "c1", Phase: "Running", ContainerID: "k1"})
		}
	}))
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	old := time.Now().Add(-17 * time.Hour)
	tg := &Target{
		Name: "web", ComponentName: "web", DeployMode: DeployModeImage,
		ImageRef: "nginx:1.27-alpine", LastImageDigest: sameDigest,
		LastBuildStatus: BuildStatusSuccess, LastBuildAt: old, ForceRebuild: force,
	}
	if err := db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}
	return &Watcher{client: client.New(srv.URL), db: db, log: zap.NewNop()}, tg, &asked
}

func upserted(asked []string) bool {
	for _, a := range asked {
		if a == "PUT /v1/components" {
			return true
		}
	}
	return false
}

// 重新部署 on an image service whose tag has not moved must still redeploy.
func TestForcedRedeployOfAnUnchangedImageDeploys(t *testing.T) {
	w, tg, asked := imageForceRig(t, true)
	w.checkAndMaybeDeployImage(context.Background(), tg)

	if !upserted(*asked) {
		t.Fatalf("a forced redeploy of the same digest did not deploy; asked: %v", *asked)
	}
	var row Target
	if err := w.db.First(&row, tg.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.ForceRebuild {
		t.Error("force_rebuild is still set after the redeploy it asked for")
	}
	if row.LastBuildStatus != BuildStatusSuccess {
		t.Errorf("status = %q (%s), want success", row.LastBuildStatus, row.LastBuildError)
	}
	if time.Since(row.LastBuildAt) > time.Minute {
		t.Errorf("last_build_at did not move: %s", row.LastBuildAt)
	}
}

// Without the request, the same digest is still nothing to do.
func TestUnchangedImageWithoutForceIsLeftAlone(t *testing.T) {
	w, tg, asked := imageForceRig(t, false)
	w.checkAndMaybeDeployImage(context.Background(), tg)
	if upserted(*asked) {
		t.Fatalf("redeployed an unchanged image nobody asked for; asked: %v", *asked)
	}
}
