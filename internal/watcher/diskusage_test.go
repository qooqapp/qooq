package watcher

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One total per namespace directory, links not followed: a link to a big file
// elsewhere must not count, or one user could make another's usage anything.
func TestMountsAreMeasuredPerNamespace(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o644)
	}
	write("acme--prod/api-db/data/base", 3000)
	write("acme--prod/api/files/conf", 500)
	write("bob--prod/web-db/data/x", 7)
	outside := filepath.Join(t.TempDir(), "huge")
	os.WriteFile(outside, bytes.Repeat([]byte("y"), 1<<20), 0o644)
	os.Symlink(outside, filepath.Join(root, "bob--prod", "link"))

	got, byComp, err := measureMounts(root)
	if err != nil {
		t.Fatal(err)
	}
	if got["acme--prod"] != 3500 || got["bob--prod"] != 7 {
		t.Errorf("measured %v, want acme--prod 3500 and bob--prod 7", got)
	}
	if byComp["acme--prod/api-db"] != 3000 || byComp["acme--prod/api"] != 500 || byComp["bob--prod/web-db"] != 7 {
		t.Errorf("per component %v, want api-db 3000, api 500, web-db 7", byComp)
	}
	if _, ok := byComp["bob--prod/link"]; ok {
		t.Errorf("the link was counted as a component: %v", byComp)
	}

	if m, _, err := measureMounts(filepath.Join(root, "missing")); err != nil || len(m) != 0 {
		t.Errorf("a missing mounts dir: %v, %v", m, err)
	}
}

// A held target is not built by polling and not by a forced deploy either;
// trigger and upload refuse instead of accepting work that would never run.
func TestAQuotaHoldStopsEveryWayOfDeploying(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "app")
	w.db.Model(tg).Updates(map[string]any{"quota_hold": "over the 5 GB data limit", "force_rebuild": true})

	due, err := dueTargets(w.db, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.Name == "app" {
			t.Error("a held target is still due, even forced")
		}
	}

	r := lifecycleRouter(w)
	r.POST("/v1/targets/:name/trigger", w.handleTriggerTarget)
	r.PUT("/v1/targets/:name/quota-hold", w.handleQuotaHold)
	if rec := call(r, http.MethodPost, "/v1/targets/app/trigger?force=1"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "5 GB") {
		t.Errorf("trigger on a held target: %d %s", rec.Code, rec.Body.String())
	}

	// Released: due again.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/targets/app/quota-hold", strings.NewReader(`{"reason":""}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
	}
	due, _ = dueTargets(w.db, time.Now().Add(time.Hour), "")
	found := false
	for _, d := range due {
		found = found || d.Name == "app"
	}
	if !found {
		t.Error("a released target is still not due")
	}
}
