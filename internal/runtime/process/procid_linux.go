//go:build linux

package process

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procKey returns a per-pid identifier stable for the process's whole
// lifetime but different after the kernel reuses the pid for someone else:
// the process start time (field 22 of /proc/<pid>/stat, in clock ticks since
// boot) never repeats for a different process sharing the same pid. Returns
// "" when the read fails — callers must treat "" as "probe unavailable",
// never as a mismatch.
func procKey(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// comm (field 2) is parenthesized and may itself contain spaces/parens,
	// so locate the fields that follow it by the *last* ')' in the line.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return ""
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 20 { // starttime is overall field 22 = fields[19] here
		return ""
	}
	return fields[19]
}

// checkLiveness classifies pid's state relative to the identity key recorded
// when the process was started:
//   - kill(pid,0) failing is hard evidence of death -> liveDead.
//   - a readable, non-empty key that differs from the recorded one is only
//     soft evidence (pid reuse) -> liveSuspect; callers must see it on
//     consecutive checks before acting.
//   - everything else (match, no recorded key, /proc read temporarily
//     failing) -> liveAlive.
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
