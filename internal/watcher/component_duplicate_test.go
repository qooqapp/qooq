package watcher

import (
	"errors"
	"strings"
	"testing"
)

// A project moved into a stack without removing the standalone target it used
// to have deploys twice: the target names differ (stack targets are prefixed)
// and the namespaces differ, so neither existing check fires, and both copies
// poll the same repo and report success. Nothing downstream ever notices.
func TestCreateTarget_RejectsDuplicateFromSameRepo(t *testing.T) {
	db := newTestDB(t)
	standalone := &Target{Name: "blog", RepoURL: "file:///repos/blog.git", ComponentName: "blog"}
	if err := createTarget(db, standalone); err != nil {
		t.Fatalf("createTarget standalone: %v", err)
	}
	inStack := &Target{
		Name: "news_blog", RepoURL: "file:///repos/blog.git", ComponentName: "blog",
		DeploySpec: `{"namespace":"news"}`,
	}
	err := createTarget(db, inStack)
	if !errors.Is(err, ErrComponentDuplicate) {
		t.Fatalf("second deployment of one repo: got %v, want ErrComponentDuplicate", err)
	}
	// The message has to name the target holding it and both ways out, or the
	// reader is left to guess which of the two situations they are in.
	for _, want := range []string{`"blog"`, "qooq watch rm blog", "--allow-duplicate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err)
		}
	}
	ts, err := listTargets(db)
	if err != nil {
		t.Fatalf("listTargets: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("targets after rejected create: got %d, want 1", len(ts))
	}
}

// Staging beside production is the same shape and genuinely wanted, so the flag
// lets it through rather than the check being dropped.
func TestCreateTarget_AllowDuplicateLetsItThrough(t *testing.T) {
	db := newTestDB(t)
	staging := &Target{
		Name: "api-staging", RepoURL: "file:///repos/api.git", ComponentName: "api",
		DeploySpec: `{"namespace":"staging"}`,
	}
	if err := createTarget(db, staging); err != nil {
		t.Fatalf("createTarget staging: %v", err)
	}
	prod := &Target{
		Name: "api-prod", RepoURL: "file:///repos/api.git", ComponentName: "api",
		DeploySpec: `{"namespace":"prod"}`, AllowDuplicate: true,
	}
	if err := createTarget(db, prod); err != nil {
		t.Fatalf("createTarget prod with AllowDuplicate: %v", err)
	}
}

// One repository producing several components is the monorepo case: the names
// differ, so it is not the shape this check is looking for.
func TestCreateTarget_MonorepoComponentsAreNotDuplicates(t *testing.T) {
	db := newTestDB(t)
	api := &Target{
		Name: "todo_api", RepoURL: "file:///repos/todo.git", ComponentName: "api",
		ContextSubdir: "api", DeploySpec: `{"namespace":"todo"}`,
	}
	if err := createTarget(db, api); err != nil {
		t.Fatalf("createTarget api: %v", err)
	}
	web := &Target{
		Name: "todo_web", RepoURL: "file:///repos/todo.git", ComponentName: "web",
		ContextSubdir: "web", DeploySpec: `{"namespace":"todo"}`,
	}
	if err := createTarget(db, web); err != nil {
		t.Fatalf("createTarget web: %v", err)
	}
}

// Same repo, same name, same namespace is the older collision and keeps its own
// error: it says the component is claimed, which is more specific than saying
// it is deployed twice.
func TestCreateTarget_SameNamespaceStillReportsClaimed(t *testing.T) {
	db := newTestDB(t)
	first := &Target{Name: "blog", RepoURL: "file:///repos/blog.git", ComponentName: "blog"}
	if err := createTarget(db, first); err != nil {
		t.Fatalf("createTarget first: %v", err)
	}
	second := &Target{Name: "blog2", RepoURL: "file:///repos/blog.git", ComponentName: "blog"}
	if err := createTarget(db, second); !errors.Is(err, ErrComponentClaimed) {
		t.Fatalf("same namespace: got %v, want ErrComponentClaimed", err)
	}
}
