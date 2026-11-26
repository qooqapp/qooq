//go:build linux

package process

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// cgroupRoot is where per-instance cgroups are created. Requires cgroup v2
// mounted at the conventional path and write permission (root, or a
// delegated subtree) — anything else degrades gracefully, see setupCgroup.
const cgroupRoot = "/sys/fs/cgroup/qooq"

// cgroupWarnOnce ensures repeated cgroup failures (e.g. running as a
// non-privileged user) log a single warning for the whole daemon lifetime
// rather than once per component.
var cgroupWarnOnce sync.Once

// warnCgroup logs a single warning per daemon lifetime. It is only reached
// when a component actually requested a CPU/memory limit (setupCgroup returns
// early otherwise), so the message states plainly that a requested limit is
// going unenforced and how to fix it — silent degradation here means an
// operator believes a cap is active when it is not.
func warnCgroup(log *slog.Logger, err error) {
	cgroupWarnOnce.Do(func() {
		log.Warn("process runtime: a component requested CPU/memory limits but cgroup v2 enforcement is unavailable — limits are NOT applied. Run qooqd as root or with a delegated cgroup subtree.", "err", err)
	})
}

func cgroupsAvailable() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

// setupCgroup best-effort creates a per-instance cgroup and applies the
// CPU/memory limits from the spec. Any failure (EPERM, unmounted cgroupfs,
// ...) is logged once (see cgroupWarnOnce) and otherwise ignored: resource
// limiting is not allowed to block a container from starting.
func setupCgroup(id string, cpuNanos, memBytes int64, log *slog.Logger) string {
	// No limit requested → no cgroup needed. Skip entirely so an unprivileged
	// daemon running only unlimited components stays silent (and does no
	// pointless mkdir work); the warning is reserved for the case that
	// actually misleads an operator — a requested limit going unenforced.
	if cpuNanos <= 0 && memBytes <= 0 {
		return ""
	}
	if !cgroupsAvailable() {
		return ""
	}
	if err := os.MkdirAll(cgroupRoot, 0o755); err != nil {
		warnCgroup(log, err)
		return ""
	}
	// cgroup v2: children only expose cpu.max/memory.max after the parent
	// delegates those controllers via its subtree_control. qooq/ never holds
	// processes directly (no-internal-process rule), so this is always legal.
	// Write controllers one at a time — a kernel missing one (e.g. memory on
	// some configs) must not veto the other.
	for _, ctrl := range []string{"+cpu", "+memory"} {
		if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte(ctrl), 0o644); err != nil {
			warnCgroup(log, err)
		}
	}
	dir := filepath.Join(cgroupRoot, id)
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		warnCgroup(log, err)
		return ""
	}
	if cpuNanos > 0 {
		// cpu.max is "<quota> <period>" in microseconds; cpuNanos is
		// nano-cpus (1 core = 1e9), quota = cores * period(100000).
		quota := cpuNanos / 10000
		line := fmt.Sprintf("%d 100000", quota)
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(line), 0o644); err != nil {
			warnCgroup(log, err)
		}
	}
	if memBytes > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(memBytes, 10)), 0o644); err != nil {
			warnCgroup(log, err)
		}
		// Without this the limit is soft in practice: the kernel pushes the
		// overage to swap (memory.swap.max defaults to "max") instead of
		// OOM-killing. MemLimitBytes is documented as a hard cap.
		if err := os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0o644); err != nil {
			warnCgroup(log, err)
		}
	}
	return dir
}

func addPidToCgroup(dir string, pid int, log *slog.Logger) {
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		warnCgroup(log, err)
	}
}

// removeCgroup best-effort rmdir's the instance's cgroup once its process
// has exited (cgroup.procs must be empty for this to succeed; failures are
// silently ignored — a leftover empty-ish cgroup directory is harmless).
func removeCgroup(dir string) {
	if dir == "" {
		return
	}
	_ = os.Remove(dir)
}
