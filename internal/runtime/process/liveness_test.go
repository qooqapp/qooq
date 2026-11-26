//go:build unix

package process

import (
	"os"
	"os/exec"
	"testing"
)

// spawnAndReap runs a trivial child process to completion and returns its
// pid — guaranteed to no longer exist (kill(pid,0) must fail) since it has
// already been waited on and reaped.
func spawnAndReap(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn/wait helper process: %v", err)
	}
	return cmd.Process.Pid
}

// TestCheckLivenessAliveMatchingKey uses os.Getpid() as a pid guaranteed to
// be alive. With no recorded key, checkLiveness must report liveAlive.
func TestCheckLivenessAliveNoKey(t *testing.T) {
	if got := checkLiveness(os.Getpid(), ""); got != liveAlive {
		t.Fatalf("checkLiveness(self, \"\") = %v, want liveAlive", got)
	}
}

// TestCheckLivenessAliveMatchingKey: a key that matches the current
// procKey() for our own pid must report liveAlive.
func TestCheckLivenessAliveMatchingKey(t *testing.T) {
	pid := os.Getpid()
	key := procKey(pid)
	if key == "" {
		t.Skip("procKey unavailable in this environment")
	}
	if got := checkLiveness(pid, key); got != liveAlive {
		t.Fatalf("checkLiveness(self, matching key) = %v, want liveAlive", got)
	}
}

// TestCheckLivenessDead uses pid<=0 which checkLiveness short-circuits to
// liveDead without ever calling kill(2) or a real process probe.
func TestCheckLivenessDeadInvalidPid(t *testing.T) {
	if got := checkLiveness(0, ""); got != liveDead {
		t.Fatalf("checkLiveness(0, \"\") = %v, want liveDead", got)
	}
	if got := checkLiveness(-1, ""); got != liveDead {
		t.Fatalf("checkLiveness(-1, \"\") = %v, want liveDead", got)
	}
}

// TestCheckLivenessDeadUnusedPid finds a pid that is definitely not running
// (spawn a short-lived child, wait for it to exit and be reaped) and
// verifies kill(pid,0) failing is treated as hard evidence of death.
func TestCheckLivenessDeadExitedProcess(t *testing.T) {
	pid := spawnAndReap(t)
	if got := checkLiveness(pid, ""); got != liveDead {
		t.Fatalf("checkLiveness(exited pid, \"\") = %v, want liveDead", got)
	}
}

// TestCheckLivenessSuspectKeyMismatch: our own pid is alive, but a key that
// does not match its actual identity key must be soft evidence
// (liveSuspect), not treated as death.
func TestCheckLivenessSuspectKeyMismatch(t *testing.T) {
	pid := os.Getpid()
	realKey := procKey(pid)
	if realKey == "" {
		t.Skip("procKey unavailable in this environment")
	}
	if got := checkLiveness(pid, "definitely-not-the-real-key"); got != liveSuspect {
		t.Fatalf("checkLiveness(self, wrong key) = %v, want liveSuspect", got)
	}
}
