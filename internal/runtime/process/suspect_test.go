//go:build unix

package process

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// TestCheckAdoptedRequiresTwoConsecutiveSuspectsBeforeDeath exercises the
// suspectThreshold=2 policy end to end via checkAdopted (the poller's
// per-tick sweep): a single liveSuspect probe (key mismatch on an otherwise
// alive pid) must not kill the instance, but two consecutive ones must.
func TestCheckAdoptedRequiresTwoConsecutiveSuspectsBeforeDeath(t *testing.T) {
	if suspectThreshold != 2 {
		t.Fatalf("test assumes suspectThreshold == 2, got %d", suspectThreshold)
	}

	dir := t.TempDir()
	a := &Adapter{
		dir:       dir,
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		instances: make(map[runtime.ContainerID]*instance),
		bus:       newEventBus(),
	}

	inst := &instance{
		id:  runtime.ContainerID("test-inst"),
		dir: t.TempDir(),
		meta: meta{
			ID:      "test-inst",
			PID:     os.Getpid(), // alive, but...
			ProcKey: "wrong-key-that-never-matches",
			Adopted: true,
		},
		done:     make(chan struct{}),
		doneOnce: &sync.Once{},
	}
	a.instances[inst.id] = inst

	// First tick: one liveSuspect strike, must not be marked exited.
	a.checkAdopted()
	inst.mu.Lock()
	exited := inst.meta.Exited
	strikes := inst.suspect
	inst.mu.Unlock()
	if exited {
		t.Fatal("single suspect strike must not kill the instance")
	}
	if strikes != 1 {
		t.Fatalf("expected 1 strike after first suspect tick, got %d", strikes)
	}

	// Second consecutive tick: second strike reaches suspectThreshold, must
	// now be marked exited (soft evidence accepted after two in a row).
	a.checkAdopted()
	inst.mu.Lock()
	exited = inst.meta.Exited
	exitCode := inst.meta.ExitCode
	inst.mu.Unlock()
	if !exited {
		t.Fatal("two consecutive suspect strikes should mark the instance exited")
	}
	if exitCode != -1 {
		t.Fatalf("adopted-death exit code should be -1 (unknown), got %d", exitCode)
	}
}

// TestCheckAdoptedAliveResetsSuspectCounter verifies a liveAlive probe
// resets the consecutive-strike counter, so an intermittent single mismatch
// followed by a clean match never accumulates toward death.
func TestCheckAdoptedAliveResetsSuspectCounter(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{
		dir:       dir,
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		instances: make(map[runtime.ContainerID]*instance),
		bus:       newEventBus(),
	}

	pid := os.Getpid()
	if procKey(pid) == "" {
		t.Skip("procKey unavailable in this environment")
	}
	inst := &instance{
		id:  runtime.ContainerID("test-inst-2"),
		dir: t.TempDir(),
		meta: meta{
			ID:      "test-inst-2",
			PID:     pid,
			ProcKey: "wrong-key",
			Adopted: true,
		},
		done:     make(chan struct{}),
		doneOnce: &sync.Once{},
	}
	a.instances[inst.id] = inst

	a.checkAdopted() // one suspect strike
	inst.mu.Lock()
	inst.meta.ProcKey = procKey(pid) // now matches -> liveAlive next tick
	inst.mu.Unlock()

	a.checkAdopted()
	inst.mu.Lock()
	strikes := inst.suspect
	exited := inst.meta.Exited
	inst.mu.Unlock()
	if strikes != 0 {
		t.Fatalf("a liveAlive tick should reset the suspect counter to 0, got %d", strikes)
	}
	if exited {
		t.Fatal("instance should not be exited after the counter reset")
	}
}
