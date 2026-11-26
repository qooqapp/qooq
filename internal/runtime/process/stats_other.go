//go:build unix && !linux

package process

// cpuSample carries no state on non-Linux hosts: there is no lightweight,
// dependency-free way to read a pid's CPU/RSS usage per-tick without cgo, so
// monitoring degrades to always-zero samples here (still emitted on the same
// cadence, so core's "latest sample" cache stays fresh). A v1 limitation.
type cpuSample struct{}

func sampleUsage(pid int, prev cpuSample) (cpuPercent float64, memBytes uint64, cur cpuSample, ok bool) {
	return 0, 0, cpuSample{}, true
}
