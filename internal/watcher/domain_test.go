package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qooqapp/qooq/pkg/client"
)

// newRepoWith makes a one-commit repository holding the given files.
func newRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if _, err := wt.Commit("c", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return "file://" + dir
}

// runUntilRefused runs the real pipeline up to the first thing that can refuse
// the manifest. The watcher has no qooqd client, so a build that was allowed to
// start fails somewhere else — which is exactly what the assertions tell apart.
func runUntilRefused(t *testing.T, manifestYAML, generated, deploySpec string) error {
	t.Helper()
	w := giveUpWatcher(t)
	w.client = client.New("http://127.0.0.1:1") // nothing listens
	files := map[string]string{"f.txt": "x"}
	if manifestYAML != "" {
		files["qooq.yaml"] = manifestYAML
	}
	tg := &Target{Name: "app", ComponentName: "app", RepoURL: newRepoWith(t, files), Branch: "master",
		DeploySpec: deploySpec, GeneratedManifest: generated}
	if err := createTarget(w.db, tg); err != nil {
		t.Fatalf("create: %v", err)
	}
	return w.runPipeline(context.Background(), tg)
}

const (
	sharedSpec = `{"namespace":"acme--prod"}`
	addressMsg = "domain and ingress.hosts cannot be set in qooq.yaml here"
)

func TestASharedNodeManifestCannotPickItsOwnDomain(t *testing.T) {
	err := runUntilRefused(t, "version: 1\napp: app\ndomain: blog.example.com\n", "", sharedSpec)
	if err == nil || !strings.Contains(err.Error(), addressMsg) {
		t.Fatalf("err = %v, want the address refusal", err)
	}
	if isNotTheRepository(err) {
		t.Errorf("the refusal must count against the repository: %v", err)
	}
}

func TestASharedNodeManifestCannotPickItsOwnHosts(t *testing.T) {
	err := runUntilRefused(t, "version: 1\napp: app\ningress:\n  hosts: [blog.example.com]\n", "", sharedSpec)
	if err == nil || !strings.Contains(err.Error(), addressMsg) {
		t.Fatalf("err = %v, want the address refusal", err)
	}
}

// The stored manifest is the one an operator can PUT, and the generated one:
// both resolve through the same call, so the refusal covers them too.
func TestAStoredManifestIsHeldToTheSameRule(t *testing.T) {
	err := runUntilRefused(t, "", "version: 1\napp: app\ndomain: blog.example.com\n", sharedSpec)
	if err == nil || !strings.Contains(err.Error(), addressMsg) {
		t.Fatalf("err = %v, want the address refusal", err)
	}
}

// Outside a shared-node namespace nothing changes: a self-hosted install names its
// own domain.
func TestANonSharedNodeManifestMayStillSetItsDomain(t *testing.T) {
	for _, spec := range []string{"", `{"namespace":"staging"}`} {
		err := runUntilRefused(t, "version: 1\napp: app\ndomain: blog.example.com\ningress:\n  hosts: [a.example.com]\n", "", spec)
		if err != nil && strings.Contains(err.Error(), addressMsg) {
			t.Errorf("spec %q: refused a non-shared manifest: %v", spec, err)
		}
	}
}

// What the platform assigns arrives in the operator's deploy_spec, and that is
// not the manifest: a user with no address in its qooq.yaml is not refused.
func TestTheOperatorsDomainInASharedNodeNamespaceIsNotRefused(t *testing.T) {
	err := runUntilRefused(t, "version: 1\napp: app\n", "",
		`{"namespace":"acme--prod","domain":"blog-k3x9q.apps.example.net"}`)
	if err != nil && strings.Contains(err.Error(), addressMsg) {
		t.Fatalf("refused the platform's own domain: %v", err)
	}
	if err := checkTenantManifestAddress(&Target{DeploySpec: sharedSpec}, nil); err != nil {
		t.Errorf("no manifest at all was refused: %v", err)
	}
}

func TestValidateDomain(t *testing.T) {
	for _, ok := range []string{"", "a", "blog-k3x9q.apps.example.net", "A1.b-2.example.com", strings.Repeat("a", 63) + ".com"} {
		if err := validateDomain(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	long := strings.Repeat(strings.Repeat("a", 61)+".", 5) + "com"
	for _, bad := range []string{"a b.com", "-a.com", "a-.com", "a..com", ".a.com", "a.com.", "a_b.com",
		"http://a.com", "a.com/x", "a:80", strings.Repeat("a", 64) + ".com", long, "é.com"} {
		if err := validateDomain(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestUpdateTargetDomain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := giveUpWatcher(t)
	const orig = `{"namespace":"acme--prod","env":{"A":"b c"},"ports":[{"container":80,"host":8080}],"cpu_limit":0.5}`
	tg := mkTarget(t, w, "app")
	w.db.Model(tg).Update("deploy_spec", orig)
	owned := mkTarget(t, w, "todo_api")
	w.db.Model(owned).Update("stack_name", "todo")

	r := gin.New()
	r.PUT("/v1/targets/:name", w.handleUpdateTarget)
	put := func(name, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/v1/targets/"+name, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	spec := func() map[string]json.RawMessage {
		got, err := getTargetByName(w.db, "app")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(got.DeploySpec), &m); err != nil {
			t.Fatalf("deploy_spec %q: %v", got.DeploySpec, err)
		}
		return m
	}
	// Every field but domain, byte for byte.
	sameAsBefore := func(m map[string]json.RawMessage) {
		t.Helper()
		var want map[string]json.RawMessage
		_ = json.Unmarshal([]byte(orig), &want)
		for k, v := range want {
			if string(m[k]) != string(v) {
				t.Errorf("field %s = %s, want %s", k, m[k], v)
			}
		}
		if _, ok := m["domain"]; ok && len(m) != len(want)+1 {
			t.Errorf("unexpected fields: %v", m)
		}
	}

	if rec := put("app", `{"domain":"blog-k3x9q.apps.example.net"}`); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	m := spec()
	if string(m["domain"]) != `"blog-k3x9q.apps.example.net"` {
		t.Errorf("domain = %s", m["domain"])
	}
	sameAsBefore(m)
	got, _ := getTargetByName(w.db, "app")
	if !got.ForceRebuild {
		t.Error("the change did not schedule a rebuild")
	}

	// Setting the same value again is not a request to restart the app.
	w.db.Model(got).Update("force_rebuild", false)
	if rec := put("app", `{"domain":"blog-k3x9q.apps.example.net"}`); rec.Code != http.StatusOK {
		t.Fatalf("repeat: %d", rec.Code)
	}
	if again, _ := getTargetByName(w.db, "app"); again.ForceRebuild {
		t.Error("an unchanged domain triggered a rebuild")
	}

	before := spec()
	for _, bad := range []string{`{"domain":"a b"}`, `{"domain":"-x.com"}`, `{"domain":"https://x.com"}`,
		`{"domain":"` + strings.Repeat("a", 64) + `.com"}`} {
		if rec := put("app", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, rec.Code)
		}
	}
	if after := spec(); string(after["domain"]) != string(before["domain"]) {
		t.Error("a refused domain was stored anyway")
	}

	if rec := put("app", `{"domain":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d", rec.Code)
	}
	m = spec()
	if _, ok := m["domain"]; ok {
		t.Errorf("domain not cleared: %s", m["domain"])
	}
	sameAsBefore(m)

	if rec := put("nope", `{"domain":"a.com"}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
	if rec := put("todo_api", `{"domain":"a.com"}`); rec.Code != http.StatusConflict {
		t.Errorf("stack-owned: %d", rec.Code)
	}
}
