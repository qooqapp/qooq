package watcher

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const twoProjectStack = `
version: 1
stack: acme-prod
projects:
  db-migrate:
    path: ./migrator
  api:
    repo: https://github.com/acme/api.git
    branch: release
    depends_on:
      - db-migrate
`

func testStack() *Stack {
	return &Stack{
		Name:                "acme-prod",
		RepoURL:             "https://github.com/acme/prod.git",
		Branch:              "main",
		AuthType:            AuthTypeToken,
		AuthCredential:      "enc:v1:whatever",
		PollIntervalSeconds: 60,
	}
}

// A path: project builds out of the stack's own repository, narrowed to that
// subdirectory — including the manifest, which lives there too.
func TestDesiredStepTarget_PathStep(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(twoProjectStack))
	if err != nil {
		t.Fatal(err)
	}

	stack := testStack()
	got, err := w.desiredProjectTarget(stack, "db-migrate", r.Projects["db-migrate"], w.stackScope(stack, r))
	if err != nil {
		t.Fatalf("desiredProjectTarget: %v", err)
	}

	if got.Name != "acme-prod_db-migrate" {
		t.Errorf("target name = %q", got.Name)
	}
	if got.ComponentName != "db-migrate" {
		t.Errorf("component name = %q, want the short name (it is the DNS alias)", got.ComponentName)
	}
	if got.RepoURL != stack.RepoURL || got.Branch != "main" {
		t.Errorf("path project should track the stack's repo, got %s@%s", got.RepoURL, got.Branch)
	}
	if got.ContextSubdir != "migrator" || got.ManifestPath != "migrator/qooq.yaml" {
		t.Errorf("subdir = %q, manifest = %q", got.ContextSubdir, got.ManifestPath)
	}
	if got.AuthCredential != stack.AuthCredential {
		t.Error("the stack's credential should reach a project in its own repo, still encrypted")
	}
	if got.StackName != stack.Name || got.ProjectName != "db-migrate" {
		t.Errorf("ownership not recorded: stack=%q project=%q", got.StackName, got.ProjectName)
	}
}

// A repo: project is someone else's repository, and the stack's credential must
// not follow it there.
func TestDesiredStepTarget_RepoStep(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(twoProjectStack))
	if err != nil {
		t.Fatal(err)
	}

	stack := testStack()
	got, err := w.desiredProjectTarget(stack, "api", r.Projects["api"], w.stackScope(stack, r))
	if err != nil {
		t.Fatalf("desiredProjectTarget: %v", err)
	}

	if got.RepoURL != "https://github.com/acme/api.git" || got.Branch != "release" {
		t.Errorf("got %s@%s", got.RepoURL, got.Branch)
	}
	if got.ContextSubdir != "" || got.ManifestPath != "qooq.yaml" {
		t.Errorf("an external repo is used whole: subdir=%q manifest=%q", got.ContextSubdir, got.ManifestPath)
	}
	if got.AuthType != AuthTypeNone || got.AuthCredential != "" {
		t.Errorf("the stack's credential leaked to an external repo: type=%q cred=%q", got.AuthType, got.AuthCredential)
	}
}

// The stack decides the namespace and the network for every project, which is what
// lets the same repo be deployed into several stacks without editing its
// manifest.
func TestDesiredStepTarget_DeploySpec(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(twoProjectStack))
	if err != nil {
		t.Fatal(err)
	}

	stack := testStack()
	got, err := w.desiredProjectTarget(stack, "api", r.Projects["api"], w.stackScope(stack, r))
	if err != nil {
		t.Fatal(err)
	}
	var spec deploySpecFields
	if err := json.Unmarshal([]byte(got.DeploySpec), &spec); err != nil {
		t.Fatalf("deploy_spec is not valid JSON: %v", err)
	}
	if spec.Namespace != "acme-prod" {
		t.Errorf("namespace = %q", spec.Namespace)
	}
	if spec.Network != stackNetwork("acme-prod") {
		t.Errorf("network = %q, want the stack's own network", spec.Network)
	}
}

func TestStepTargetUpdates(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(twoProjectStack))
	if err != nil {
		t.Fatal(err)
	}
	stack := testStack()
	desired, err := w.desiredProjectTarget(stack, "api", r.Projects["api"], w.stackScope(stack, r))
	if err != nil {
		t.Fatal(err)
	}

	// An identical target needs no update at all — otherwise every sweep would
	// set force_rebuild and rebuild the whole stack forever.
	same := *desired
	if updates := projectTargetUpdates(&same, desired); len(updates) != 0 {
		t.Errorf("unchanged project reported updates: %v", updates)
	}

	// Pipeline state is the target's own and must survive a manifest edit.
	stale := *desired
	stale.Branch = "main"
	stale.LastCommit = "abc1234"
	stale.GeneratedManifest = "version: 1\n"
	updates := projectTargetUpdates(&stale, desired)
	if updates["branch"] != "release" {
		t.Errorf("branch change not detected: %v", updates)
	}
	for _, k := range []string{"last_commit", "generated_manifest", "last_build_status"} {
		if _, ok := updates[k]; ok {
			t.Errorf("%s belongs to the target, not the manifest", k)
		}
	}
}

// The stack manifest layer's env is where one project is told where another one lives.
func TestStackScopeExpandsProjectHost(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(`
version: 1
stack: acme-prod
projects:
  api:
    repo: https://github.com/acme/api.git
  web:
    repo: https://github.com/acme/web.git
    env:
      API_URL: "http://${project.api.host}:8080"
      STACK: "${stack}"
`))
	if err != nil {
		t.Fatal(err)
	}
	stack := testStack()

	got, err := w.desiredProjectTarget(stack, "web", r.Projects["web"], w.stackScope(stack, r))
	if err != nil {
		t.Fatalf("desiredProjectTarget: %v", err)
	}
	var spec deploySpecFields
	if err := json.Unmarshal([]byte(got.DeploySpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Env["API_URL"] != "http://api:8080" {
		t.Errorf("API_URL = %q, want the sibling's short name", spec.Env["API_URL"])
	}
	if spec.Env["STACK"] != "acme-prod" {
		t.Errorf("STACK = %q", spec.Env["STACK"])
	}
}

// A typo in a project name has to be an error here, not a connection failure
// inside a container later — that is the entire reason to write the reference
// rather than the bare hostname.
func TestStackScopeRejectsUnknownProject(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(`
version: 1
stack: acme-prod
projects:
  api:
    repo: https://github.com/acme/api.git
  web:
    repo: https://github.com/acme/web.git
    env:
      API_URL: "http://${project.ap.host}:8080"
`))
	if err != nil {
		t.Fatal(err)
	}
	stack := testStack()

	_, err = w.desiredProjectTarget(stack, "web", r.Projects["web"], w.stackScope(stack, r))
	if err == nil {
		t.Fatal("a misspelled project name was accepted")
	}
	// The message has to show what does resolve, or a typo is a guessing game.
	if !strings.Contains(err.Error(), "project.api.host") {
		t.Errorf("error %q does not list the available references", err)
	}
}

func TestBlockedDeps(t *testing.T) {
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatal(err)
	}
	project := r.Projects["plate-it"] // depends_on: whip-mayo (ready), pineapple (started)

	cases := map[string]struct {
		outcomes map[string]projectOutcome
		want     []string
	}{
		"all deployed": {
			outcomes: map[string]projectOutcome{"whip-mayo": projectDeployed, "pineapple": projectDeployed},
		},
		"one failed": {
			outcomes: map[string]projectOutcome{"whip-mayo": projectFailed, "pineapple": projectDeployed},
			want:     []string{"whip-mayo"},
		},
		"a skipped upstream blocks too": {
			outcomes: map[string]projectOutcome{"whip-mayo": projectDeployed, "pineapple": projectSkipped},
			want:     []string{"pineapple"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := blockedDeps(project, tc.outcomes)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("blockedDeps = %v, want %v", got, tc.want)
			}
		})
	}
}

// wait: started must not block at all. The walk is sequential, so by the time
// a dependent is reached its upstream deploy has already returned — and a
// nil qooqd client here proves nothing was asked of qooqd.
func TestAwaitDeps_StartedDoesNotWait(t *testing.T) {
	w := &Watcher{}
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatal(err)
	}
	project := r.Projects["plate-it"]

	// Only pineapple (wait: started) counts as deployed; whip-mayo (wait:
	// ready) is left un-deployed so it is not waited on either.
	outcomes := map[string]projectOutcome{"pineapple": projectDeployed}
	if dep, err := w.awaitDeps(t.Context(), testStack(), project, outcomes, map[string]error{}); err != nil {
		t.Fatalf("awaitDeps(%s) = %v, want no wait at all", dep, err)
	}
}

func TestStackStatusFor(t *testing.T) {
	cases := map[string]struct {
		outcomes map[string]projectOutcome
		want     string
	}{
		"nothing deployed": {map[string]projectOutcome{}, StackStatusNever},
		"all deployed":     {map[string]projectOutcome{"a": projectDeployed, "b": projectDeployed}, StackStatusSuccess},
		"all failed":       {map[string]projectOutcome{"a": projectFailed, "b": projectSkipped}, StackStatusFailed},
		"partly deployed":  {map[string]projectOutcome{"a": projectDeployed, "b": projectFailed}, StackStatusDegraded},
		"partly skipped":   {map[string]projectOutcome{"a": projectDeployed, "b": projectSkipped}, StackStatusDegraded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := stackStatusFor(tc.outcomes); got != tc.want {
				t.Errorf("stackStatusFor(%v) = %q, want %q", tc.outcomes, got, tc.want)
			}
		})
	}
}

// A stack-owned target must never be swept on its own: its stack deploys it in
// the manifest order, and a second sweeper would deploy it out of order.
func TestDueTargetsExcludesStackProjects(t *testing.T) {
	db := newTestDB(t)
	past := time.Now().Add(-time.Hour)

	standalone := &Target{Name: "web", RepoURL: "file:///repo", ComponentName: "web", NextCheckAt: past}
	project := &Target{
		Name: "acme_api", RepoURL: "file:///repo", ComponentName: "api",
		StackName: "acme", ProjectName: "api", NextCheckAt: past,
	}
	for _, tg := range []*Target{standalone, project} {
		if err := createTarget(db, tg); err != nil {
			t.Fatalf("createTarget %s: %v", tg.Name, err)
		}
	}

	due, err := dueTargets(db, time.Now(), "")
	if err != nil {
		t.Fatalf("dueTargets: %v", err)
	}
	if len(due) != 1 || due[0].Name != "web" {
		names := make([]string, 0, len(due))
		for _, stack := range due {
			names = append(names, stack.Name)
		}
		t.Fatalf("due = %v, want only the standalone target", names)
	}
}

// Triggering a project has to reach its stack, or a webhook on a project repo would
// do nothing at all.
func TestTriggerNowOnProjectPokesItsStack(t *testing.T) {
	db := newTestDB(t)

	stack := &Stack{Name: "acme", RepoURL: "file:///stackrepo", NextCheckAt: time.Now().Add(time.Hour)}
	if err := createStack(db, stack); err != nil {
		t.Fatalf("createStack: %v", err)
	}
	project := &Target{
		Name: "acme_api", RepoURL: "file:///repo", ComponentName: "api",
		StackName: "acme", ProjectName: "api", NextCheckAt: time.Now().Add(time.Hour),
	}
	if err := createTarget(db, project); err != nil {
		t.Fatalf("createTarget: %v", err)
	}

	if err := triggerNow(db, "acme_api"); err != nil {
		t.Fatalf("triggerNow: %v", err)
	}

	due, err := dueStacks(db, time.Now())
	if err != nil {
		t.Fatalf("dueStacks: %v", err)
	}
	if len(due) != 1 || due[0].Name != "acme" {
		t.Fatalf("poking a project did not make its stack due: %+v", due)
	}
}

func TestCreateStackDefaults(t *testing.T) {
	db := newTestDB(t)
	stack := &Stack{Name: "acme", RepoURL: "file:///stackrepo"}
	if err := createStack(db, stack); err != nil {
		t.Fatalf("createStack: %v", err)
	}
	if stack.Branch != "main" || stack.ManifestPath != defaultStackManifestPath || stack.AuthType != AuthTypeNone {
		t.Errorf("defaults not applied: %+v", stack)
	}
	if stack.PollIntervalSeconds != defaultPollIntervalSeconds {
		t.Errorf("poll interval = %d", stack.PollIntervalSeconds)
	}
	if stack.Status != StackStatusNever {
		t.Errorf("status = %q", stack.Status)
	}
	// Zero NextCheckAt means the first sweep tick deploys it.
	if !stack.NextCheckAt.IsZero() {
		t.Errorf("next check = %v, want zero so it is deployed immediately", stack.NextCheckAt)
	}

	if err := createStack(db, &Stack{Name: "acme", RepoURL: "file:///other"}); err != ErrStackNameConflict {
		t.Errorf("duplicate name error = %v, want ErrStackNameConflict", err)
	}
}
