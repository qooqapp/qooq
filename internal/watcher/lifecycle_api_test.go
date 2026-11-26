package watcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qooqapp/qooq/pkg/client"
)

// The lifecycle routes at the HTTP layer: the status codes a caller sees, and
// the one ordering guarantee that cannot be checked any other way.
//
// lifecycle_test.go covers the operations; this covers the handlers around
// them, which is where the failure modes live — a 404 that should be a 409, a
// row deleted after the removal it was supposed to follow had already failed.

// lifecycleRouter wires the four verbs and delete onto a bare engine, the same
// way registerRoutes does, without the auth middleware (which has its own
// tests) so each case is about the handler.
func lifecycleRouter(w *Watcher) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.DELETE("/v1/targets/:name", w.handleDeleteTarget)
	r.POST("/v1/targets/:name/stop", w.handleStopTarget)
	r.POST("/v1/targets/:name/start", w.handleStartTarget)
	r.POST("/v1/targets/:name/pause", w.handlePauseTarget)
	r.POST("/v1/targets/:name/resume", w.handleResumeTarget)
	return r
}

func call(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// lifecycleWatcher is a watcher with a target called "app" that deploys a
// component of the same name, and a stubbed daemon behind it.
func lifecycleWatcher(t *testing.T, d *daemonStub) (*Watcher, *Target) {
	t.Helper()
	w := giveUpWatcher(t)
	if d != nil {
		w.client = d.start(t)
	}
	tg := mkTarget(t, w, "app")
	return w, tg
}

func deployedStub(status int) *daemonStub {
	return &daemonStub{
		status: status,
		comps: []client.ComponentResponse{
			{ID: "c1", Name: "app", Namespace: client.DefaultNamespace, Phase: "Running"},
		},
	}
}

func targetExists(t *testing.T, w *Watcher, name string) bool {
	t.Helper()
	_, err := getTargetByName(w.db, name)
	return err == nil
}

// A removal that could not take the deployment down must not take the target
// either.
//
// This is the whole reason the handler removes the component FIRST. If the row
// went first, a daemon that is down for thirty seconds would leave a running
// container with its address live and nothing listing it — the exact orphan
// this change exists to stop creating. Keeping the row means the project is
// still there to retry.
func TestAFailedRemovalLeavesTheTargetInPlace(t *testing.T) {
	d := deployedStub(http.StatusInternalServerError)
	w, _ := lifecycleWatcher(t, d)

	rec := call(lifecycleRouter(w), http.MethodDelete, "/v1/targets/app")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !targetExists(t, w, "app") {
		t.Error("the target was deleted even though its deployment is still running — that is the orphan this ordering exists to prevent")
	}
}

func TestRemovalTakesTheComponentThenTheRow(t *testing.T) {
	d := deployedStub(0)
	w, _ := lifecycleWatcher(t, d)

	rec := call(lifecycleRouter(w), http.MethodDelete, "/v1/targets/app")

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !d.asked(http.MethodDelete, "/v1/components/c1") {
		t.Errorf("the component was never removed; asked: %v", d.requests)
	}
	if targetExists(t, w, "app") {
		t.Error("the target survived its own removal")
	}
}

// keep_component is the escape hatch for handing a deployment to something
// else. The daemon must not be asked to remove anything at all.
func TestKeepComponentRemovesOnlyTheWatching(t *testing.T) {
	d := deployedStub(0)
	w, _ := lifecycleWatcher(t, d)

	rec := call(lifecycleRouter(w), http.MethodDelete, "/v1/targets/app?keep_component=1")

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if d.asked(http.MethodDelete, "/v1/components/c1") {
		t.Error("keep_component=1 removed the component anyway")
	}
	if targetExists(t, w, "app") {
		t.Error("the target survived its own removal")
	}
}

// Anything other than exactly "1" is not the escape hatch. A caller who writes
// ?keep_component=true means to keep it, but guessing at that is how a flag
// ends up meaning different things in different places; the one spelling is
// the documented one, and the default is the safe-to-retry direction.
func TestKeepComponentOnlyAcceptsOne(t *testing.T) {
	d := deployedStub(0)
	w, _ := lifecycleWatcher(t, d)

	call(lifecycleRouter(w), http.MethodDelete, "/v1/targets/app?keep_component=true")

	if !d.asked(http.MethodDelete, "/v1/components/c1") {
		t.Error("?keep_component=true was read as the escape hatch; only \"1\" is")
	}
}

// A target that was never there is a 404, and the daemon is not called about
// it — there is nothing to remove, and a request on behalf of a target that
// does not exist is a request nobody can explain later.
func TestRemovingAnUnknownTargetTouchesNothing(t *testing.T) {
	d := deployedStub(0)
	w, _ := lifecycleWatcher(t, d)

	rec := call(lifecycleRouter(w), http.MethodDelete, "/v1/targets/nope")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if len(d.requests) != 0 {
		t.Errorf("the daemon was asked about a target that does not exist: %v", d.requests)
	}
}

// The status codes for stop and start, including the three failures that are
// easy to collapse into one.
func TestLifecycleStatusCodes(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		stub   *daemonStub // nil means no qooqd configured
		target string
		want   int
	}{
		{"stop works", "/stop", deployedStub(0), "app", http.StatusNoContent},
		{"start works", "/start", deployedStub(0), "app", http.StatusNoContent},
		{"no such target", "/stop", deployedStub(0), "nope", http.StatusNotFound},
		// 409 rather than 404: the target IS there. A 404 would read as "no
		// such project" to a caller that just listed it.
		{"nothing deployed", "/stop", &daemonStub{}, "app", http.StatusConflict},
		{"daemon refuses", "/stop", deployedStub(http.StatusInternalServerError), "app", http.StatusBadGateway},
		{"no daemon configured", "/start", nil, "app", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := lifecycleWatcher(t, tc.stub)
			rec := call(lifecycleRouter(w), http.MethodPost, "/v1/targets/"+tc.target+tc.path)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Pausing is about the watching, so it must not reach the daemon at all —
// anything running keeps running, and that is the promise the console repeats
// to the person pressing the button.
func TestPausingNeverTouchesTheDeployment(t *testing.T) {
	d := deployedStub(0)
	w, tg := lifecycleWatcher(t, d)
	r := lifecycleRouter(w)

	if rec := call(r, http.MethodPost, "/v1/targets/app/pause"); rec.Code != http.StatusNoContent {
		t.Fatalf("pause = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if len(d.requests) != 0 {
		t.Errorf("pausing talked to the daemon: %v", d.requests)
	}
	if got := getTargetByID(t, w.db, tg.ID); !got.DeployPaused {
		t.Error("deploy_paused was not set")
	}
}

// Resuming checks now rather than at the end of the poll interval: the reason
// somebody resumes is that they want what was pushed while it was paused, and
// waiting out five minutes looks like the resume did not work.
func TestResumingChecksImmediately(t *testing.T) {
	w, tg := lifecycleWatcher(t, deployedStub(0))
	later := time.Now().Add(time.Hour)
	if err := w.db.Model(tg).Updates(map[string]any{
		"deploy_paused": true, "next_check_at": later,
	}).Error; err != nil {
		t.Fatalf("set up: %v", err)
	}

	if rec := call(lifecycleRouter(w), http.MethodPost, "/v1/targets/app/resume"); rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d, want 204", rec.Code)
	}

	got := getTargetByID(t, w.db, tg.ID)
	if got.DeployPaused {
		t.Error("deploy_paused survived a resume")
	}
	if got.NextCheckAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("next_check_at = %v, still in the future: a resume that waits out the interval looks like it did nothing", got.NextCheckAt)
	}
}

func TestPauseAndResumeOnAnUnknownTargetAre404(t *testing.T) {
	w, _ := lifecycleWatcher(t, deployedStub(0))
	r := lifecycleRouter(w)
	for _, verb := range []string{"pause", "resume"} {
		if rec := call(r, http.MethodPost, "/v1/targets/nope/"+verb); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", verb, rec.Code)
		}
	}
}

// A component the daemon says is already gone is a removal that succeeded:
// the caller asked for it not to be there, and it is not.
func TestRemovingAComponentThatIsAlreadyGoneSucceeds(t *testing.T) {
	d := deployedStub(http.StatusNotFound)
	w, _ := lifecycleWatcher(t, d)

	rec := call(lifecycleRouter(w), http.MethodDelete, "/v1/targets/app")

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if targetExists(t, w, "app") {
		t.Error("the target was kept because of a component that was already gone")
	}
}

// A removed component is not this target's component any more, so a stop must
// not act on it. Phase "Removed" is how the daemon reports one.
func TestARemovedComponentIsNotFound(t *testing.T) {
	d := &daemonStub{comps: []client.ComponentResponse{
		{ID: "c1", Name: "app", Namespace: client.DefaultNamespace, Phase: "Removed"},
	}}
	w, _ := lifecycleWatcher(t, d)

	rec := call(lifecycleRouter(w), http.MethodPost, "/v1/targets/app/stop")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if d.asked(http.MethodPost, "/v1/components/c1/stop") {
		t.Error("stopped a component the daemon had already removed")
	}
}

// The error bodies stay JSON, like every other route: a caller that parses
// {"error":...} must not get HTML or an empty body on these.
func TestLifecycleErrorsAreJSON(t *testing.T) {
	w, _ := lifecycleWatcher(t, &daemonStub{})
	rec := call(lifecycleRouter(w), http.MethodPost, "/v1/targets/app/stop")

	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %q", rec.Body.String())
	}
	if body.Error == "" {
		t.Errorf("error body says nothing: %q", rec.Body.String())
	}
}
