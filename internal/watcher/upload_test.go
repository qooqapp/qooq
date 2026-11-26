package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func uploadTestWatcher(t *testing.T) (*Watcher, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := testWatcherWithDB(t)
	w.uploadsDir = t.TempDir()
	r := gin.New()
	r.POST("/v1/targets", w.handleCreateTarget)
	r.PUT("/v1/targets/:name/source", w.handleUploadSource)
	return w, r
}

func createUploadTarget(t *testing.T, r *gin.Engine, name string) {
	t.Helper()
	body := `{"name":"` + name + `","deploy_mode":"upload","component_name":"` + name + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/targets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create upload target: %d %s", rec.Code, rec.Body.String())
	}
}

func putSource(t *testing.T, r *gin.Engine, name string, archive []byte, query string) (*httptest.ResponseRecorder, UploadSourceResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/targets/"+name+"/source"+query, bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out UploadSourceResponse
	if rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

func storedArchives(t *testing.T, w *Watcher, name string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(w.uploadsDir, name))
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestCreateUploadTargetNeedsNoRepository(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	got, err := getTargetByName(w.db, "site")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployMode != DeployModeUpload || got.RepoURL != "" {
		t.Fatalf("stored %q / repo %q", got.DeployMode, got.RepoURL)
	}
	// The column defaults to "main"; an upload has no branch to show.
	if got.Branch != "" {
		t.Errorf("branch = %q, want empty", got.Branch)
	}

	for _, extra := range []string{`"repo_url":"https://x/y.git"`, `"branch":"main"`, `"image_ref":"nginx"`, `"auth_type":"token"`} {
		body := `{"name":"bad","deploy_mode":"upload","component_name":"bad",` + extra + `}`
		req := httptest.NewRequest(http.MethodPost, "/v1/targets", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("upload target with %s: %d, want 400", extra, rec.Code)
		}
	}
	// The name becomes a directory, so it is held to a directory's rules.
	body := `{"name":"a/b","deploy_mode":"upload","component_name":"ab"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/targets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("upload target named a/b: %d, want 400", rec.Code)
	}
}

func TestStackProjectCannotBeAnUploadTarget(t *testing.T) {
	db := newTestDB(t)
	err := createTarget(db, &Target{Name: "s_p", DeployMode: DeployModeUpload, ComponentName: "p", StackName: "s", ProjectName: "p"})
	if !errors.Is(err, ErrUploadInStack) {
		t.Fatalf("err = %v, want ErrUploadInStack", err)
	}
}

func TestUploadSourceOnlyForUploadTargets(t *testing.T) {
	w, r := uploadTestWatcher(t)
	if err := createTarget(w.db, &Target{Name: "git", RepoURL: "https://x/y.git", ComponentName: "git"}); err != nil {
		t.Fatal(err)
	}
	rec, _ := putSource(t, r, "git", makeTarGz(t, file("a", "1")), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("upload to a source target: %d, want 409", rec.Code)
	}
	rec, _ = putSource(t, r, "nope", makeTarGz(t, file("a", "1")), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upload to a missing target: %d, want 404", rec.Code)
	}
}

func TestUploadSourceStoresAndQueues(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	archive := makeTarGz(t, file("index.html", "v1"))

	before := time.Now()
	rec, out := putSource(t, r, "site", archive, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if !out.BuildQueued || len(out.Revision) != 64 {
		t.Fatalf("response %+v", out)
	}
	if got := storedArchives(t, w, "site"); len(got) != 1 || got[0] != out.Revision+".tar.gz" {
		t.Fatalf("stored %v, want [%s.tar.gz]", got, out.Revision)
	}
	tg, _ := getTargetByName(w.db, "site")
	if tg.UploadRevision != out.Revision || tg.LastUploadAt.Before(before) {
		t.Fatalf("target upload_revision=%q last_upload_at=%v", tg.UploadRevision, tg.LastUploadAt)
	}
	if tg.NextCheckAt.Before(before) {
		t.Fatalf("upload did not make the target due (next_check_at %v)", tg.NextCheckAt)
	}
	if tg.ForceRebuild {
		t.Fatal("a plain upload set force_rebuild")
	}

	// The archive the pipeline reads back is the one that was sent.
	dir := t.TempDir()
	rev, err := w.extractStoredUpload(tg, dir)
	if err != nil || rev != out.Revision {
		t.Fatalf("extractStoredUpload = %q, %v", rev, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "index.html")); string(b) != "v1" {
		t.Fatalf("index.html = %q", b)
	}
}

// Uploading what is already deployed is "nothing changed", exactly like a
// poll that finds the same HEAD — unless the caller insists.
func TestUploadSameContentDoesNotQueueABuild(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	_, first := putSource(t, r, "site", makeTarGz(t, file("index.html", "v1")), "")
	// As if that upload had built and deployed.
	w.db.Model(&Target{}).Where("name = ?", "site").Updates(map[string]any{
		"last_commit": first.Revision, "last_build_status": BuildStatusSuccess, "build_generation": 3})

	// Same files, packed again later.
	again := makeTarGz(t, tarEntry{name: "index.html", typ: '0', body: "v1", mode: 0o600, mtime: time.Now()})
	rec, out := putSource(t, r, "site", again, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("re-upload: %d %s", rec.Code, rec.Body.String())
	}
	if out.Revision != first.Revision || out.BuildQueued {
		t.Fatalf("re-upload of the same files: %+v (first revision %s)", out, first.Revision)
	}
	if out.BuildGeneration != 3 {
		t.Fatalf("build_generation baseline = %d, want 3", out.BuildGeneration)
	}

	// The sweep agrees: nothing is built.
	tg, _ := getTargetByName(w.db, "site")
	w.checkAndMaybeBuild(context.Background(), tg)
	if tg.BuildGeneration != 3 || tg.LastBuildStatus != BuildStatusSuccess {
		t.Fatalf("sweep rebuilt an unchanged upload: generation %d status %s", tg.BuildGeneration, tg.LastBuildStatus)
	}

	rec, out = putSource(t, r, "site", again, "?force=1")
	if rec.Code != http.StatusAccepted || !out.BuildQueued {
		t.Fatalf("forced re-upload: %d %+v", rec.Code, out)
	}
	if tg, _ := getTargetByName(w.db, "site"); !tg.ForceRebuild {
		t.Fatal("?force=1 did not set force_rebuild")
	}
}

func TestSweepIgnoresUploadTargetWithNothingUploaded(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	tg, _ := getTargetByName(w.db, "site")
	w.checkAndMaybeBuild(context.Background(), tg)
	if tg.BuildGeneration != 0 || tg.LastBuildStatus != BuildStatusNever {
		t.Fatalf("generation %d status %s, want untouched", tg.BuildGeneration, tg.LastBuildStatus)
	}
}

func TestUploadRejectsUnsafeArchiveAndStoresNothing(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	rec, _ := putSource(t, r, "site", makeTarGz(t, file("../evil", "x")), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsafe archive: %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if got := storedArchives(t, w, "site"); len(got) != 0 {
		t.Fatalf("a refused upload was stored: %v", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(w.uploadsDir, "site")); len(entries) != 0 {
		t.Fatalf("a refused upload left files behind: %v", entries)
	}
	if tg, _ := getTargetByName(w.db, "site"); tg.UploadRevision != "" {
		t.Fatalf("a refused upload moved upload_revision to %q", tg.UploadRevision)
	}
}

func TestUploadTooLargeIs413(t *testing.T) {
	_, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	withUploadLimits(t, 1<<20, 100, 100)
	rec, _ := putSource(t, r, "site", makeTarGz(t, file("big", strings.Repeat("x", 1000))), "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the unpacked limit: %d, want 413 (%s)", rec.Code, rec.Body.String())
	}

	withUploadLimits(t, 64, 1<<20, 100)
	rec, _ = putSource(t, r, "site", makeTarGz(t, file("a", strings.Repeat("y", 10))), "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the compressed limit: %d, want 413 (%s)", rec.Code, rec.Body.String())
	}
}

func TestUploadKeepsCurrentAndPrevious(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	var revs []string
	for _, v := range []string{"v1", "v2", "v3"} {
		rec, out := putSource(t, r, "site", makeTarGz(t, file("index.html", v)), "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("upload %s: %d", v, rec.Code)
		}
		revs = append(revs, out.Revision)
	}
	got := storedArchives(t, w, "site")
	want := map[string]bool{revs[1] + ".tar.gz": true, revs[2] + ".tar.gz": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("stored %v, want the last two (%s, %s)", got, shortSHA(revs[1]), shortSHA(revs[2]))
	}
}

// A stored archive that no longer matches its name has changed on disk;
// building it would deploy something nobody sent.
func TestExtractStoredUploadDetectsTampering(t *testing.T) {
	w, r := uploadTestWatcher(t)
	createUploadTarget(t, r, "site")
	_, out := putSource(t, r, "site", makeTarGz(t, file("index.html", "v1")), "")
	path := filepath.Join(w.uploadsDir, "site", out.Revision+".tar.gz")
	if err := os.WriteFile(path, makeTarGz(t, file("index.html", "evil")), 0o600); err != nil {
		t.Fatal(err)
	}
	tg, _ := getTargetByName(w.db, "site")
	if _, err := w.extractStoredUpload(tg, t.TempDir()); err == nil {
		t.Fatal("built an archive whose contents no longer match its revision")
	}
}
