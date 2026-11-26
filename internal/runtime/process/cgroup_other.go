//go:build unix && !linux

package process

import "log/slog"

// Non-Linux hosts (macOS) have no cgroups; resource limits are silently
// unenforced, matching the Linux adapter's own best-effort degradation path.

func cgroupsAvailable() bool { return false }

func setupCgroup(id string, cpuNanos, memBytes int64, log *slog.Logger) string { return "" }

func addPidToCgroup(dir string, pid int, log *slog.Logger) {}

func removeCgroup(dir string) {}
