package watcher

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// newTwoBranchRepo makes a repository whose main and feature branches point at
// different commits. Two branches at the same commit cannot show which one a
// build used, which is the only thing a branch switch is for.
func newTwoBranchRepo(t *testing.T, feature string) (repoURL, mainHead, featureHead string) {
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
	commit := func(content string) string {
		if err := os.WriteFile(dir+"/f.txt", []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add("f.txt"); err != nil {
			t.Fatalf("add: %v", err)
		}
		h, err := wt.Commit(content, &git.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		return h.String()
	}
	mainHead = commit("on main")
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(feature), Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	featureHead = commit("on feature")
	if head.Name().Short() != "main" && head.Name().Short() != "master" {
		t.Fatalf("unexpected default branch %q", head.Name().Short())
	}
	return "file://" + dir, mainHead, featureHead
}

func TestSetBranchSwitchesAndTheNextCheckReadsTheNewBranch(t *testing.T) {
	w := giveUpWatcher(t)
	url, mainHead, featureHead := newTwoBranchRepo(t, "feat/x")
	defaultBranch := "master"
	tg := &Target{Name: "app", ComponentName: "app", RepoURL: url, Branch: defaultBranch,
		LastCommit: mainHead, NextCheckAt: time.Now().Add(time.Hour)}
	if err := createTarget(w.db, tg); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, changed, err := w.setTargetBranch("app", "feat/x")
	if err != nil || !changed {
		t.Fatalf("setTargetBranch: changed=%v err=%v", changed, err)
	}
	if got.Branch != "feat/x" {
		t.Fatalf("returned branch %q, want feat/x", got.Branch)
	}

	fresh, err := getTargetByName(w.db, "app")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Branch != "feat/x" {
		t.Errorf("stored branch %q, want feat/x", fresh.Branch)
	}
	if !fresh.ForceRebuild {
		t.Error("force_rebuild not set: a switch to a branch whose head equals LastCommit would never build")
	}
	if fresh.NextCheckAt.After(time.Now()) {
		t.Errorf("not scheduled now: next_check_at=%v", fresh.NextCheckAt)
	}
	if fresh.LastCommit != mainHead {
		t.Errorf("last_commit changed to %q; it is the previous image's tag for pruning", fresh.LastCommit)
	}
	// What the sweep does with the row: resolve its branch on the remote.
	head, err := ResolveRemoteHead(fresh, gitAuth{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if head != featureHead {
		t.Errorf("next check resolves %s, want feature head %s (main is %s)", head, featureHead, mainHead)
	}
}

func TestSetBranchToTheSameBranchRebuildsNothing(t *testing.T) {
	w := giveUpWatcher(t)
	mkTarget(t, w, "app")
	_, changed, err := w.setTargetBranch("app", "main")
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want no-op", changed, err)
	}
	fresh, _ := getTargetByName(w.db, "app")
	if fresh.ForceRebuild {
		t.Error("an unchanged branch forced a rebuild")
	}
}

// The stack sync derives a project's branch from the stack manifest and would
// write it back on its next pass: accepting the change would be a success that
// silently does not stick.
func TestSetBranchRefusesAStackOwnedTarget(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "todo_api")
	if err := w.db.Model(tg).Updates(map[string]any{"stack_name": "todo", "project_name": "api"}).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err := w.setTargetBranch("todo_api", "dev")
	if !errors.Is(err, ErrTargetStackOwned) {
		t.Fatalf("err = %v, want ErrTargetStackOwned", err)
	}
	if !strings.Contains(err.Error(), "stack manifest") {
		t.Errorf("error does not say where to change it: %v", err)
	}
	fresh, _ := getTargetByName(w.db, "todo_api")
	if fresh.Branch != "main" || fresh.ForceRebuild {
		t.Errorf("stack-owned row was modified: branch=%q force=%v", fresh.Branch, fresh.ForceRebuild)
	}
}

func TestSetBranchRefusesAnImageModeTarget(t *testing.T) {
	w := giveUpWatcher(t)
	tg := &Target{Name: "img", ComponentName: "img", DeployMode: DeployModeImage, ImageRef: "nginx:alpine"}
	if err := createTarget(w.db, tg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.setTargetBranch("img", "dev"); !errors.Is(err, ErrTargetHasNoBranch) {
		t.Fatalf("err = %v, want ErrTargetHasNoBranch", err)
	}
}

// A build of the old branch that is running when the switch arrives finishes
// with recordSuccess, which clears force_rebuild. If the switch does not wait
// for it, that write lands after the switch and the rebuild never happens.
func TestSetBranchWaitsForAnInFlightBuild(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "app")

	unlock := w.lockTarget("app") // the sweep, mid-build
	done := make(chan error, 1)
	go func() {
		_, _, err := w.setTargetBranch("app", "dev")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// The build of main finishes: recordSuccess's write.
	if err := w.recordSuccess(tg, "abc1234"); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	fresh, _ := getTargetByName(w.db, "app")
	if fresh.Branch != "dev" {
		t.Errorf("branch %q, want dev", fresh.Branch)
	}
	if !fresh.ForceRebuild {
		t.Error("the in-flight build's success erased the switch's force_rebuild")
	}
}

// Decisions are made on the row as it is once the lock is held, not as it was
// when the request arrived: a stack that adopted the target meanwhile owns it.
func TestSetBranchRereadsTheRowInsideTheLock(t *testing.T) {
	w := giveUpWatcher(t)
	mkTarget(t, w, "app")

	unlock := w.lockTarget("app")
	done := make(chan error, 1)
	go func() {
		_, _, err := w.setTargetBranch("app", "dev")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := w.db.Model(&Target{}).Where("name = ?", "app").Update("stack_name", "todo").Error; err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; !errors.Is(err, ErrTargetStackOwned) {
		t.Fatalf("err = %v, want ErrTargetStackOwned from the re-read row", err)
	}
}

// Switching to a branch that builds is one of the ways a paused target gets
// fixed, so it resumes it — the same stance as `qooq watch trigger`.
func TestSetBranchResumesAPausedTarget(t *testing.T) {
	w := giveUpWatcher(t)
	tg := mkTarget(t, w, "app")
	for i := 0; i < maxConsecutiveBuildFailures; i++ {
		w.noteBuildFailure(tg)
	}
	if tg.PausedReason == "" {
		t.Fatal("setup: target not paused")
	}
	if _, _, err := w.setTargetBranch("app", "dev"); err != nil {
		t.Fatal(err)
	}
	fresh, _ := getTargetByName(w.db, "app")
	if fresh.PausedReason != "" {
		t.Errorf("still paused: %q", fresh.PausedReason)
	}
	due, err := dueTargets(w.db, time.Now().Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Errorf("paused target not back in the work queue (due=%d)", len(due))
	}
}

func TestValidateBranch(t *testing.T) {
	for _, ok := range []string{"main", "feat/qooq-test", "release-1.2", "a_b"} {
		if err := validateBranch(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "has space", "a..b", "a:b", "x~1", "end.", "/lead", "trail/",
		"a//b", "refs/heads/main", "refs/tags/v1", "x.lock", "@", "a@{b", "-flag"} {
		if err := validateBranch(bad); !errors.Is(err, ErrInvalidBranch) {
			t.Errorf("%q accepted (err=%v)", bad, err)
		}
	}
}

func TestHandleUpdateTargetStatusCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := giveUpWatcher(t)
	mkTarget(t, w, "app")
	owned := mkTarget(t, w, "todo_api")
	w.db.Model(owned).Update("stack_name", "todo")

	r := gin.New()
	r.PUT("/v1/targets/:name", w.handleUpdateTarget)
	put := func(name, body, ct string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/v1/targets/"+name, strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		name, target, body, ct string
		want                   int
	}{
		{"switch", "app", `{"branch":"dev"}`, "application/json", http.StatusOK},
		{"not json", "app", `{"branch":"dev"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"no field", "app", `{}`, "application/json", http.StatusBadRequest},
		{"bad name", "app", `{"branch":"a b"}`, "application/json", http.StatusBadRequest},
		{"missing", "nope", `{"branch":"dev"}`, "application/json", http.StatusNotFound},
		{"stack", "todo_api", `{"branch":"dev"}`, "application/json", http.StatusConflict},
	}
	for _, c := range cases {
		rec := put(c.target, c.body, c.ct)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

// auth_type can be moved from none to github-app, and to nothing else: the
// one change a target can need without anybody having made a mistake (a
// project created before its account installed the App). Nothing carrying a
// credential, no downgrade, not without a external manager, not on a stack.
func TestUpdateTargetMovesOnlyNoneToAppAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := giveUpWatcher(t)
	plain := mkTarget(t, w, "plain")
	w.db.Model(plain).Updates(map[string]any{"auth_type": AuthTypeNone, "repo_url": "https://github.com/o/r.git"})
	tok := mkTarget(t, w, "tok")
	w.db.Model(tok).Updates(map[string]any{"auth_type": AuthTypeToken, "auth_credential": "x"})
	owned := mkTarget(t, w, "todo_api")
	w.db.Model(owned).Update("stack_name", "todo")

	r := gin.New()
	r.PUT("/v1/targets/:name", w.handleUpdateTarget)
	put := func(name, body string) int {
		req := httptest.NewRequest(http.MethodPut, "/v1/targets/"+name, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := put("plain", `{"auth_type":"github-app"}`); got != http.StatusBadRequest {
		t.Fatalf("without a control plane: %d, want 400", got)
	}
	w.control = &controlLink{}
	for _, c := range []struct {
		target, body string
		want         int
	}{
		{"plain", `{"auth_type":"token"}`, http.StatusBadRequest},
		{"tok", `{"auth_type":"github-app"}`, http.StatusBadRequest},
		{"todo_api", `{"auth_type":"github-app"}`, http.StatusConflict},
		{"plain", `{"auth_type":"github-app"}`, http.StatusOK},
		{"plain", `{"auth_type":"github-app"}`, http.StatusOK}, // idempotent
	} {
		if got := put(c.target, c.body); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.target, c.body, got, c.want)
		}
	}
	after, _ := getTargetByName(w.db, "plain")
	if after.AuthType != AuthTypeGitHubApp {
		t.Fatalf("auth_type = %q", after.AuthType)
	}
	if again, _ := getTargetByName(w.db, "tok"); again.AuthType != AuthTypeToken {
		t.Fatalf("a token target was changed to %q", again.AuthType)
	}
}
