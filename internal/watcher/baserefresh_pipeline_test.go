package watcher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qooqapp/qooq/pkg/client"
	"go.uber.org/zap"
)

// The call site, not only the helper: a real clone of a repo whose
// Dockerfile starts FROM nginx:alpine must make the build node pull that
// image before it builds — and only the first time inside the interval.
func TestThePipelinePullsTheBaseBeforeBuilding(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM nginx:alpine\nCOPY . /usr/share/nginx/html\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("Dockerfile"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Head()

	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/images/build" {
			// Stop the pipeline here; what came before is the point.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"stop"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	w := &Watcher{db: newTestDB(t), log: zap.NewNop(), client: client.New(srv.URL),
		baseRefresh: newBaseRefresher(time.Hour)}
	tg := &Target{Name: "web", ComponentName: "web", RepoURL: "file://" + dir,
		Branch: head.Name().Short(), Dockerfile: "Dockerfile", ManifestPath: "-"}
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}

	_ = w.runPipeline(t.Context(), tg)
	_ = w.runPipeline(t.Context(), tg)

	mu.Lock()
	defer mu.Unlock()
	var pulls, builds int
	for _, h := range hits {
		switch h {
		case "POST /v1/images/pull":
			pulls++
			if builds > 0 {
				t.Errorf("pulled after building: %v", hits)
			}
		case "POST /v1/images/build":
			builds++
		}
	}
	if builds != 2 || pulls != 1 {
		t.Errorf("pulls=%d builds=%d, want 1 and 2 (hits %v)", pulls, builds, hits)
	}
}
