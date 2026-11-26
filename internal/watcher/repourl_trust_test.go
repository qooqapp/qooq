package watcher

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Under untrusted_repos a repository on the watcher's own disk is refused;
// with it off, the offline product's local bare repos keep working.
func TestUntrustedRepositoriesCannotCloneFromThisMachine(t *testing.T) {
	local := []string{
		"file:///var/lib/qooq/other-user.git",
		"FILE:///srv/git/api.git",
		"/srv/git/api.git",
		"./api.git",
		"../qooq",
	}
	remote := []string{
		"https://github.com/alice/api.git",
		"ssh://git@github.com/alice/api.git",
		"git@github.com:alice/api.git",
	}

	trusted := &Watcher{untrustedRepos: false}
	untrusted := &Watcher{untrustedRepos: true}
	for _, raw := range local {
		if err := trusted.checkRepoURLAllowed(raw); err != nil {
			t.Errorf("trusted watcher refused local %s: %v", raw, err)
		}
		if err := untrusted.checkRepoURLAllowed(raw); err == nil {
			t.Errorf("untrusted watcher accepted local %s", raw)
		}
	}
	for _, raw := range remote {
		if err := untrusted.checkRepoURLAllowed(raw); err != nil {
			t.Errorf("untrusted watcher refused remote %s: %v", raw, err)
		}
	}
}

// Through the real handler, so the check is known to be on the create path and
// before anything is stored (this Watcher has no database to store into).
func TestCreateTargetRefusesALocalRepoWhenUntrusted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := &Watcher{untrustedRepos: true}
	r := gin.New()
	r.POST("/v1/targets", w.handleCreateTarget)
	r.POST("/v1/stacks", w.handleCreateStack)

	for path, body := range map[string]string{
		"/v1/targets": `{"name":"x","component_name":"x","repo_url":"file:///srv/git/other.git"}`,
		"/v1/stacks":  `{"name":"x","repo_url":"/srv/git/other.git"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "disk") {
			t.Errorf("POST %s: %d %s, want 400 naming the disk", path, rec.Code, rec.Body.String())
		}
	}
}

// A stack manifest comes from the repository, so its repo: projects are as
// untrusted as the repository itself.
func TestUntrustedStackCannotNameALocalRepoProject(t *testing.T) {
	r, err := parseStackManifest([]byte(twoProjectStack))
	if err != nil {
		t.Fatal(err)
	}
	stack := testStack()
	api := r.Projects["api"]
	api.Repo = "file:///srv/git/other-user.git"

	w := &Watcher{untrustedRepos: true}
	if _, err := w.desiredProjectTarget(stack, "api", api, w.stackScope(stack, r)); err == nil {
		t.Error("untrusted watcher accepted a stack project on its own disk")
	}
	w = &Watcher{}
	if _, err := w.desiredProjectTarget(stack, "api", api, w.stackScope(stack, r)); err != nil {
		t.Errorf("trusted watcher refused a local stack project: %v", err)
	}
}
