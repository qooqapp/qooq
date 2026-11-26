//go:build unix && !linux

package process

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// procKey on non-Linux unix hosts (macOS) uses the process start timestamp
// via `ps -o lstart=` as the pid-reuse guard: unlike the command name it is
// stable for the process's whole lifetime (macOS /bin/sh, for one, re-execs
// itself through /var/select/sh, so a comm-based key recorded right after
// spawn can legitimately change later). Returns "" when ps fails or the pid
// is gone — callers must treat "" as "probe unavailable", never as a
// mismatch.
func procKey(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// checkLiveness classifies pid's state relative to the identity key recorded
// when the process was started:
//   - kill(pid,0) failing is hard evidence of death -> liveDead.
//   - a readable, non-empty key that differs from the recorded one is only
//     soft evidence (pid may have been reused, or ps output was odd) ->
//     liveSuspect; callers must see it on consecutive checks before acting.
//   - everything else (match, no recorded key, ps temporarily failing) ->
//     liveAlive.
func checkLiveness(pid int, key string) liveness {
	if pid <= 0 {
		return liveDead
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return liveDead
	}
	if key == "" {
		return liveAlive
	}
	cur := procKey(pid)
	if cur == "" || cur == key {
		return liveAlive
	}
	return liveSuspect
}
