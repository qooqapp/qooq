package watcher

import (
	"errors"
	"testing"
)

// Two targets aimed at one component do not share it, they fight over it: each
// build upserts the same component, so the deployment flips to whichever built
// last with nothing in either target's status to explain it.
func TestCreateTarget_RejectsClaimedComponent(t *testing.T) {
	db := newTestDB(t)
	first := &Target{Name: "demo", RepoURL: "file:///a", ComponentName: "demo-web"}
	if err := createTarget(db, first); err != nil {
		t.Fatalf("createTarget first: %v", err)
	}
	second := &Target{Name: "demo-copy", RepoURL: "file:///b", ComponentName: "demo-web"}
	err := createTarget(db, second)
	if !errors.Is(err, ErrComponentClaimed) {
		t.Fatalf("second target on the same component: got %v, want ErrComponentClaimed", err)
	}
	// The rejected target must not have been written: a half-created target
	// would be swept and would deploy exactly what the check refused.
	ts, err := listTargets(db)
	if err != nil {
		t.Fatalf("listTargets: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("targets after rejected create: got %d, want 1", len(ts))
	}
}

// Namespace is part of a component's identity, so two stacks that each contain
// an "api" project are two components and both targets are legal.
func TestCreateTarget_AllowsSameNameInOtherNamespace(t *testing.T) {
	db := newTestDB(t)
	a := &Target{
		Name: "alpha-api", RepoURL: "file:///a", ComponentName: "api",
		DeploySpec: `{"namespace":"alpha"}`,
	}
	if err := createTarget(db, a); err != nil {
		t.Fatalf("createTarget alpha: %v", err)
	}
	b := &Target{
		Name: "beta-api", RepoURL: "file:///b", ComponentName: "api",
		DeploySpec: `{"namespace":"beta"}`,
	}
	if err := createTarget(db, b); err != nil {
		t.Fatalf("createTarget beta: %v", err)
	}
}

// Deleting a target releases its component: reusing the name after a rm is the
// normal way to repoint a deployment at a different repo.
func TestCreateTarget_ComponentFreedAfterDelete(t *testing.T) {
	db := newTestDB(t)
	first := &Target{Name: "demo", RepoURL: "file:///a", ComponentName: "demo-web"}
	if err := createTarget(db, first); err != nil {
		t.Fatalf("createTarget first: %v", err)
	}
	if err := deleteTargetByName(db, "demo"); err != nil {
		t.Fatalf("deleteTargetByName: %v", err)
	}
	second := &Target{Name: "demo2", RepoURL: "file:///b", ComponentName: "demo-web"}
	if err := createTarget(db, second); err != nil {
		t.Fatalf("createTarget after delete: %v", err)
	}
}
