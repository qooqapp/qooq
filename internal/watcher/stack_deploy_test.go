package watcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/qooqapp/qooq/pkg/client"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// newLocalCommittedRepo creates a throwaway git repository on disk with a
// single commit and returns its file:// URL, default branch name, and HEAD
// hash. Pointing a Target at it lets checkAndMaybeBuild run its real,
// unmocked ResolveRemoteHead/cloneBranch logic against something that
// actually exists on disk instead of the network — fast (tens of
// milliseconds) and hermetic (no docker, no qooqd).
func newLocalCommittedRepo(t *testing.T) (repoURL, branch, headHash string) {
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
	if err := os.WriteFile(dir+"/f.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	hash, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return "file://" + dir, head.Name().Short(), hash.String()
}

// newFakeQooqd stands up an HTTP server backing a *client.Client that answers
// GET /v1/components and GET /v1/components/{id} out of comps. It exists so
// waitForComponentReady/findComponent (both hit by the depends_on: wait:
// ready path) can be driven for real, without a live qooqd.
func newFakeQooqd(t *testing.T, comps []client.ComponentResponse) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/components" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(comps)
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/components/"); ok {
			for _, c := range comps {
				if c.ID == id {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(c)
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL)
}

// seedProjectTarget creates a project target row directly (bypassing
// reconcileProjects/desiredProjectTarget, which are exercised elsewhere) so
// walkProjects/deployProject have something real to lock, re-read and update.
func seedProjectTarget(t *testing.T, db *gorm.DB, stackName, project string, mutate func(*Target)) *Target {
	t.Helper()
	tg := &Target{
		Name:          projectTargetName(stackName, project),
		RepoURL:       "file:///does-not-matter",
		Branch:        "main",
		ComponentName: project,
		StackName:     stackName,
		ProjectName:   project,
	}
	if mutate != nil {
		mutate(tg)
	}
	if err := db.Create(tg).Error; err != nil {
		t.Fatalf("seed project target %s/%s: %v", stackName, project, err)
	}
	return tg
}

func reloadTarget(t *testing.T, db *gorm.DB, id uint) Target {
	t.Helper()
	var tg Target
	if err := db.First(&tg, id).Error; err != nil {
		t.Fatalf("reload target %d: %v", id, err)
	}
	return tg
}

// ---------------------------------------------------------------------------
// 1. A failed project's downstream is skipped, and left alone.
// ---------------------------------------------------------------------------

// TestFailedProjectSkipsDownstreamAndLeavesItAlone drives walkProjects over
// the real shrimpStack dependency graph (shell-shrimp -> whip-mayo -> plate-it,
// pineapple independent) with shell-shrimp's build genuinely failing (an
// unreachable repo). The point under test: a broken upstream must not pull
// its dependents' *containers* down with it. walkProjects itself never
// touches a container directly — that only happens inside deployProject's
// checkAndMaybeBuild/upsert — so "left alone" is proven by showing
// deployProject was never even called for whip-mayo/plate-it: their target
// rows come out of the walk byte-for-byte what they were seeded as.
func TestFailedProjectSkipsDownstreamAndLeavesItAlone(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatal(err)
	}
	stack := &Stack{Name: "acme-prod"}

	seedProjectTarget(t, w.db, stack.Name, "shell-shrimp", func(tg *Target) {
		tg.RepoURL = "file:///nonexistent-repo-shell-shrimp"
	})
	whipMayo := seedProjectTarget(t, w.db, stack.Name, "whip-mayo", func(tg *Target) {
		tg.LastBuildStatus = BuildStatusSuccess
		tg.LastCommit = "whip-was-here"
	})
	plateIt := seedProjectTarget(t, w.db, stack.Name, "plate-it", func(tg *Target) {
		tg.LastBuildStatus = BuildStatusSuccess
		tg.LastCommit = "plate-was-here"
	})
	pineappleURL, pineappleBranch, pineappleHead := newLocalCommittedRepo(t)
	seedProjectTarget(t, w.db, stack.Name, "pineapple", func(tg *Target) {
		tg.RepoURL = pineappleURL
		tg.Branch = pineappleBranch
		tg.LastCommit = pineappleHead
		tg.LastBuildStatus = BuildStatusSuccess
	})

	targets := map[string]*Target{}
	outcomes := w.walkProjects(t.Context(), stack, r, targets)

	if outcomes["shell-shrimp"] != projectFailed {
		t.Errorf("shell-shrimp = %q, want failed", outcomes["shell-shrimp"])
	}
	if outcomes["whip-mayo"] != projectSkipped {
		t.Errorf("whip-mayo = %q, want skipped", outcomes["whip-mayo"])
	}
	if outcomes["plate-it"] != projectSkipped {
		t.Errorf("plate-it = %q, want skipped (its own upstream, whip-mayo, was skipped too)", outcomes["plate-it"])
	}
	if outcomes["pineapple"] != projectDeployed {
		t.Errorf("pineapple = %q, want deployed (it has no dependency on shell-shrimp)", outcomes["pineapple"])
	}

	// The proof that "skipped" did not touch anything: deployProject re-reads
	// and re-persists the row it acts on, so an untouched row comes back
	// exactly as seeded.
	gotWhip := reloadTarget(t, w.db, whipMayo.ID)
	if gotWhip.LastBuildStatus != BuildStatusSuccess || gotWhip.LastCommit != "whip-was-here" {
		t.Errorf("whip-mayo target was touched while skipped: %+v", gotWhip)
	}
	gotPlate := reloadTarget(t, w.db, plateIt.ID)
	if gotPlate.LastBuildStatus != BuildStatusSuccess || gotPlate.LastCommit != "plate-was-here" {
		t.Errorf("plate-it target was touched while skipped: %+v", gotPlate)
	}
}

// ---------------------------------------------------------------------------
// 2. A project that never becomes ready is blamed, not its waiter.
// ---------------------------------------------------------------------------

const dbThenAPIStack = `
version: 1
stack: acme
projects:
  db:
    repo: https://example.test/db.git
  api:
    repo: https://example.test/api.git
    depends_on:
      - db
`

// TestUpstreamNeverReadyIsBlamedNotItsWaiter builds "db" successfully (via
// the matching-commit short-circuit in checkAndMaybeBuild, so no docker/qooqd
// build is needed) but points the fake qooqd at an empty component list, so
// db's depends_on: wait: ready barrier can never be satisfied. The failure
// must land on db — the thing that did not come up — never on api, which
// could not be attempted at all.
func TestUpstreamNeverReadyIsBlamedNotItsWaiter(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	w.client = newFakeQooqd(t, nil) // no components exist anywhere
	r, err := parseStackManifest([]byte(dbThenAPIStack))
	if err != nil {
		t.Fatal(err)
	}
	stack := &Stack{Name: "acme"}

	dbURL, dbBranch, dbHead := newLocalCommittedRepo(t)
	seedProjectTarget(t, w.db, stack.Name, "db", func(tg *Target) {
		tg.RepoURL = dbURL
		tg.Branch = dbBranch
		tg.LastCommit = dbHead
		tg.LastBuildStatus = BuildStatusSuccess
	})
	api := seedProjectTarget(t, w.db, stack.Name, "api", func(tg *Target) {
		tg.LastBuildStatus = BuildStatusNever
	})

	targets := map[string]*Target{}
	outcomes := w.walkProjects(t.Context(), stack, r, targets)

	if outcomes["db"] != projectFailed {
		t.Errorf("db = %q, want failed (it never became ready)", outcomes["db"])
	}
	if outcomes["api"] != projectSkipped {
		t.Errorf("api = %q, want skipped, not blamed for db's readiness", outcomes["api"])
	}

	gotDB := reloadTarget(t, w.db, targets["db"].ID)
	if gotDB.LastBuildStatus != BuildStatusFailed {
		t.Errorf("db LastBuildStatus = %q, want failed", gotDB.LastBuildStatus)
	}
	if !strings.Contains(gotDB.LastBuildError, "never became ready") {
		t.Errorf("db LastBuildError = %q, want it to explain it never became ready", gotDB.LastBuildError)
	}

	// api was never attempted at all: deployProject/checkAndMaybeBuild for it
	// never ran, so its row is exactly what it was seeded as.
	gotAPI := reloadTarget(t, w.db, api.ID)
	if gotAPI.LastBuildStatus != BuildStatusNever {
		t.Errorf("api was touched even though it was never attempted: %+v", gotAPI)
	}
}

// ---------------------------------------------------------------------------
// 4. depends_on order is honored: a dependent is not deployed until its
//    upstream is actually ready.
// ---------------------------------------------------------------------------

// TestDependentDeploysOnlyAfterUpstreamReady is
// TestUpstreamNeverReadyIsBlamedNotItsWaiter's mirror image: this time the
// fake qooqd reports db as Running, so the wait: ready barrier clears and api
// must actually be attempted. api is pointed at an unreachable repo so a real
// attempt is unambiguous: its LastBuildStatus flips away from the seeded
// "never" (proving deployProject really ran for it), which api's row in the
// companion test above proved does NOT happen when the barrier never clears.
func TestDependentDeploysOnlyAfterUpstreamReady(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	w.client = newFakeQooqd(t, []client.ComponentResponse{
		{ID: "c-db", Namespace: "acme", Name: "db", Phase: "Running"},
	})
	r, err := parseStackManifest([]byte(dbThenAPIStack))
	if err != nil {
		t.Fatal(err)
	}
	stack := &Stack{Name: "acme"}

	dbURL, dbBranch, dbHead := newLocalCommittedRepo(t)
	seedProjectTarget(t, w.db, stack.Name, "db", func(tg *Target) {
		tg.RepoURL = dbURL
		tg.Branch = dbBranch
		tg.LastCommit = dbHead
		tg.LastBuildStatus = BuildStatusSuccess
	})
	api := seedProjectTarget(t, w.db, stack.Name, "api", func(tg *Target) {
		tg.RepoURL = "file:///nonexistent-repo-api"
		tg.LastBuildStatus = BuildStatusNever
	})

	targets := map[string]*Target{}
	outcomes := w.walkProjects(t.Context(), stack, r, targets)

	if outcomes["db"] != projectDeployed {
		t.Errorf("db = %q, want deployed", outcomes["db"])
	}
	// api's own build fails (bad repo) once it is actually attempted — that
	// failure is expected and is not what this test is about.
	if outcomes["api"] != projectFailed {
		t.Errorf("api = %q, want failed (its own pipeline ran and hit a bad repo)", outcomes["api"])
	}

	gotAPI := reloadTarget(t, w.db, api.ID)
	if gotAPI.LastBuildStatus == BuildStatusNever {
		t.Fatal("api's row never changed: deployProject was not attempted even though db became ready")
	}
	if !strings.Contains(gotAPI.LastBuildError, "resolve remote head") {
		t.Errorf("api LastBuildError = %q, want it to show a real attempt was made", gotAPI.LastBuildError)
	}
}

// ---------------------------------------------------------------------------
// 3. paused freezes deployment, not containers; only resume lifts it.
// ---------------------------------------------------------------------------

// TestPausedStackDeploysNothing proves deployStack's very first check: a
// paused stack must return before touching the network at all. The stack's
// repo is deliberately unreachable — if the pause check were bypassed,
// syncStackManifest would fail loudly and touchStack(..., StackStatusFailed,
// ...) would overwrite the status this test checks was left alone.
func TestPausedStackDeploysNothing(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	stack := &Stack{
		Name:         "acme",
		RepoURL:      "file:///nonexistent-stack-repo",
		Branch:       "main",
		Status:       StackStatusPaused,
		PausedReason: "project \"api\" failed 5 times in a row",
	}
	if err := createStack(w.db, stack); err != nil {
		t.Fatalf("createStack: %v", err)
	}

	w.deployStack(t.Context(), stack)

	got, err := getStackByName(w.db, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StackStatusPaused {
		t.Errorf("status = %q, want paused (a paused stack must not attempt a deploy at all)", got.Status)
	}
	if got.PausedReason != "project \"api\" failed 5 times in a row" {
		t.Errorf("paused reason was overwritten: %q", got.PausedReason)
	}
}

// TestResumeStackClearsPauseAndFailureBudget proves the only documented way
// out of a pause: resumeStack lifts the stack's own status, gives every one
// of its projects a fresh failure budget (so an old streak doesn't
// immediately re-trip maxFailures on the very next pass), and schedules an
// immediate re-deploy.
func TestResumeStackClearsPauseAndFailureBudget(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	stack := &Stack{
		Name:         "acme",
		RepoURL:      "file:///stackrepo",
		Status:       StackStatusPaused,
		PausedReason: "too many failures",
		NextCheckAt:  time.Now().Add(time.Hour),
	}
	if err := createStack(w.db, stack); err != nil {
		t.Fatalf("createStack: %v", err)
	}
	failing := seedProjectTarget(t, w.db, "acme", "api", func(tg *Target) {
		tg.ConsecutiveFailures = 5
	})

	if err := w.resumeStack(stack); err != nil {
		t.Fatalf("resumeStack: %v", err)
	}

	got, err := getStackByName(w.db, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == StackStatusPaused {
		t.Error("status still paused after resume")
	}
	if got.PausedReason != "" {
		t.Errorf("paused reason survived resume: %q", got.PausedReason)
	}
	if !got.NextCheckAt.Before(time.Now().Add(time.Minute)) {
		t.Errorf("next_check_at = %v, want it moved up so the next sweep tick deploys it", got.NextCheckAt)
	}

	gotProject := reloadTarget(t, w.db, failing.ID)
	if gotProject.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want reset to 0 by resume", gotProject.ConsecutiveFailures)
	}
}

// ---------------------------------------------------------------------------
// 8 (end to end). A build/deploy failure must not move LastCommit — proven
// through the real entry point, checkAndMaybeBuild, rather than only against
// the recordFailure helper it calls (see TestFailureKeepsTheRevisionItFailedAt
// in build_status_test.go for that unit-level guard).
// ---------------------------------------------------------------------------

func TestCheckAndMaybeBuildResolveFailureLeavesLastCommitAlone(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	tg := &Target{
		Name:            "web",
		ComponentName:   "web",
		RepoURL:         "file:///nonexistent-repo-web",
		Branch:          "main",
		LastCommit:      "old-known-good",
		LastBuildStatus: BuildStatusSuccess,
	}
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatalf("create target: %v", err)
	}

	w.checkAndMaybeBuild(t.Context(), tg)

	got := getTargetByID(t, w.db, tg.ID)
	if got.LastCommit != "old-known-good" {
		t.Errorf("last_commit moved on a resolve failure: %q", got.LastCommit)
	}
	if got.LastBuildStatus != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.LastBuildStatus)
	}
	if got.LastBuildError == "" {
		t.Error("no error recorded for the failed resolve")
	}
}

// TestSweepTargetReschedulesAfterAFailure drives sweep.go's sweepTarget end
// to end (its own coverage was 0% before this test): it must re-read the row
// under the per-target lock, run the pipeline (here, a fast deterministic
// failure), and still reschedule the next check even though the pipeline
// failed — a build failure must not stop a target from ever being looked at
// again.
func TestSweepTargetReschedulesAfterAFailure(t *testing.T) {
	w := &Watcher{db: newTestDB(t), log: zap.NewNop()}
	past := time.Now().Add(-time.Hour)
	tg := &Target{
		Name:                "web",
		ComponentName:       "web",
		RepoURL:             "file:///nonexistent-repo-web",
		Branch:              "main",
		PollIntervalSeconds: 30,
		NextCheckAt:         past,
	}
	if err := createTarget(w.db, tg); err != nil {
		t.Fatalf("createTarget: %v", err)
	}

	w.sweepTarget(t.Context(), "web")

	got, err := getTargetByName(w.db, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastBuildStatus != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.LastBuildStatus)
	}
	if !got.NextCheckAt.After(past) {
		t.Errorf("next_check_at = %v, want it advanced past %v even after a failed build", got.NextCheckAt, past)
	}
}
