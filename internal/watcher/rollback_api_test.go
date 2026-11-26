package watcher

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qooqapp/qooq/pkg/client"
)

func rollbackStub(image, previous string, status int) *daemonStub {
	return &daemonStub{
		status: status,
		comps: []client.ComponentResponse{{
			ID: "c1", Name: "app", Namespace: client.DefaultNamespace, Phase: "Running",
			Image: image, PreviousImage: previous,
		}},
	}
}

func rollbackRouter(w *Watcher) *gin.Engine {
	r := lifecycleRouter(w)
	r.POST("/v1/targets/:name/rollback", w.handleRollbackTarget)
	return r
}

func TestRollbackStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		d    *daemonStub
		want int
		asks bool
	}{
		{"rolls back", rollbackStub("app:bbbbbbb", "app:aaaaaaa", 0), http.StatusNoContent, true},
		{"never replaced", rollbackStub("app:aaaaaaa", "", 0), http.StatusConflict, false},
		// An image service on ":latest": the previous image is the same name,
		// and "rolling back" to it would restart what is running.
		{"same image", rollbackStub("nginx:latest", "nginx:latest", 0), http.StatusConflict, false},
		{"nothing deployed", &daemonStub{}, http.StatusConflict, false},
		{"daemon says nothing to roll back", rollbackStub("app:b", "app:a", http.StatusConflict), http.StatusConflict, true},
		{"daemon fails", rollbackStub("app:b", "app:a", http.StatusInternalServerError), http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := lifecycleWatcher(t, tc.d)
			rec := call(rollbackRouter(w), http.MethodPost, "/v1/targets/app/rollback")
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if got := tc.d.asked(http.MethodPost, "/v1/components/c1/rollback"); got != tc.asks {
				t.Errorf("asked qooqd to roll back = %v, want %v; asked: %v", got, tc.asks, tc.d.requests)
			}
		})
	}
}

// A deploy holds the target's lock through the check after it. A rollback
// underneath that check reads to it as the new version failing, and it rolls
// back again by itself — so the rollback is refused, not queued.
func TestRollbackIsRefusedWhileADeployHoldsTheTarget(t *testing.T) {
	d := rollbackStub("app:bbbbbbb", "app:aaaaaaa", 0)
	w, _ := lifecycleWatcher(t, d)
	unlock := w.lockTarget("app")
	defer unlock()

	rec := call(rollbackRouter(w), http.MethodPost, "/v1/targets/app/rollback")

	if rec.Code != http.StatusLocked {
		t.Errorf("status = %d, want 423; body=%s", rec.Code, rec.Body.String())
	}
	if d.asked(http.MethodPost, "/v1/components/c1/rollback") {
		t.Error("rolled back underneath a deploy in progress")
	}
}

func TestRuntimeStateCarriesBothVersions(t *testing.T) {
	d := rollbackStub("app:aaaaaaa", "app:bbbbbbb", 0)
	w, tg := lifecycleWatcher(t, d)
	targets := []TargetResponse{tg.toResponse()}

	w.attachRuntimeState(t.Context(), targets)

	if targets[0].ComponentVersion != "aaaaaaa" || targets[0].ComponentPreviousVersion != "bbbbbbb" {
		t.Errorf("versions = %q / %q, want aaaaaaa / bbbbbbb", targets[0].ComponentVersion, targets[0].ComponentPreviousVersion)
	}
}

// After a manual rollback the running version is not the last commit. The
// next deploy must keep the one that was running — it is what that deploy's
// own rollback puts back — and keeping the last commit instead deleted it.
func TestTheNextDeployKeepsTheVersionThatWasRunning(t *testing.T) {
	comp := &client.ComponentResponse{Image: "app:ccccccc", PreviousImage: "app:aaaaaaa"}
	if got := replacedVersion(comp, "bbbbbbbbbbbb"); got != "aaaaaaa" {
		t.Errorf("kept %q, want aaaaaaa (the version rolled back to)", got)
	}
	if got := replacedVersion(&client.ComponentResponse{Image: "app:ccccccc"}, "bbbbbbbbbbbb"); got != "bbbbbbbbbbbb" {
		t.Errorf("first deploy kept %q, want the last commit", got)
	}
}

func TestAppLogsAreReadFromTheTargetsComponent(t *testing.T) {
	d := rollbackStub("app:a", "", 0)
	w, _ := lifecycleWatcher(t, d)
	r := lifecycleRouter(w)
	r.GET("/v1/targets/:name/logs", w.handleAppLogs)

	rec := call(r, http.MethodGet, "/v1/targets/app/logs?tail=99999&follow=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	asked := ""
	for _, q := range d.requests {
		if strings.HasPrefix(q, "GET /v1/components/c1/logs") {
			asked = q
		}
	}
	if asked == "" {
		t.Fatalf("qooqd was not asked for the component's logs: %v", d.requests)
	}

	// Nothing deployed: 409, which the external manager turns into "not deployed".
	w2, _ := lifecycleWatcher(t, &daemonStub{})
	r2 := lifecycleRouter(w2)
	r2.GET("/v1/targets/:name/logs", w2.handleAppLogs)
	if rec := call(r2, http.MethodGet, "/v1/targets/app/logs"); rec.Code != http.StatusConflict {
		t.Errorf("nothing deployed: %d", rec.Code)
	}
}
