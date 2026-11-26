package watcher

import (
	"testing"
	"time"
)

// With a per-namespace build cap, the sweep offers due targets in the order
// dueTargets returns them and the first takes the slot. Found by make
// smoke-multi with builds_per_namespace=1: api and web, polled every ten
// seconds, were due again each tick and always listed first, so admin — due
// since it was created — never got a build. The one waiting longest goes first.
func TestDueTargetsComeOldestFirst(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	for i, name := range []string{"api", "web", "admin"} {
		// Created in this order (ascending ids); admin has waited the longest.
		at := now.Add(-time.Duration(i+1) * time.Second)
		if name == "admin" {
			at = now.Add(-time.Hour)
		}
		if err := createTarget(db, &Target{Name: name, RepoURL: "file:///" + name, ComponentName: name,
			PollIntervalSeconds: 10, NextCheckAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	due, err := dueTargets(db, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 3 || due[0].Name != "admin" {
		names := []string{}
		for _, d := range due {
			names = append(names, d.Name)
		}
		t.Fatalf("due = %v, want admin first", names)
	}
}
