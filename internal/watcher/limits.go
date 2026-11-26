package watcher

import (
	"fmt"
	"strconv"
)

// Server-wide container limits.
//
// A container's limits come from its qooq.yaml (limits:, or requests: times
// limits.multiplier), and a service without one used to get none at all:
// measured, an app with no qooq.yaml ran at 200% CPU and 840 MiB with
// Memory=0 NanoCpus=0 — one forgotten busy loop could take the machine from
// every other user on it. Most people deploying through qooq Cloud never write
// a qooq.yaml, so the unlimited case was the common one.
//
// Two settings close it:
//
//   - the default, for a container whose manifest set nothing;
//   - the maximum, which a manifest may not exceed. Over it the deploy is
//     refused with the number, rather than clamped: a clamped app that runs
//     out of memory fails far from the line that asked for more.
//
// All four unset (0) keeps the old behaviour, which is right for a
// self-hosted install where every app is the operator's own.
type containerLimits struct {
	defaultCPU float64
	defaultMem int64
	maxCPU     float64
	maxMem     int64
}

const (
	configLimitsDefaultCPU    = "limits.default_cpu"
	configLimitsDefaultMemory = "limits.default_memory"
	configLimitsMaxCPU        = "limits.max_cpu"
	configLimitsMaxMemory     = "limits.max_memory"
)

// newContainerLimits validates the four settings. A bad value is an error, not
// a warning that leaves the limit off: the setting exists to bound strangers'
// code, and silently not bounding it is the failure it is there to prevent.
func newContainerLimits(defaultCPU float64, defaultMem string, maxCPU float64, maxMem string) (containerLimits, error) {
	l := containerLimits{defaultCPU: defaultCPU, maxCPU: maxCPU}
	var err error
	if defaultCPU < 0 || maxCPU < 0 {
		return l, fmt.Errorf("%s and %s must not be negative", configLimitsDefaultCPU, configLimitsMaxCPU)
	}
	if defaultMem != "" {
		if l.defaultMem, err = parseManifestMemBytes(defaultMem); err != nil {
			return l, fmt.Errorf("%s: %w", configLimitsDefaultMemory, err)
		}
	}
	if maxMem != "" {
		if l.maxMem, err = parseManifestMemBytes(maxMem); err != nil {
			return l, fmt.Errorf("%s: %w", configLimitsMaxMemory, err)
		}
	}
	if l.maxCPU > 0 && l.defaultCPU > l.maxCPU {
		return l, fmt.Errorf("%s (%g) is above %s (%g)", configLimitsDefaultCPU, l.defaultCPU, configLimitsMaxCPU, l.maxCPU)
	}
	if l.maxMem > 0 && l.defaultMem > l.maxMem {
		return l, fmt.Errorf("%s (%s) is above %s (%s)", configLimitsDefaultMemory, defaultMem, configLimitsMaxMemory, maxMem)
	}
	return l, nil
}

// apply returns the limits a container is deployed with: what it asked for,
// the default where it asked for nothing, or an error when it asked for more
// than the maximum. The message is for the person who wrote the qooq.yaml.
func (l containerLimits) apply(cpu float64, mem int64) (float64, int64, error) {
	if l.maxCPU > 0 && cpu > l.maxCPU {
		return 0, 0, fmt.Errorf("the CPU limit would be %s cores, and this server allows at most %s — lower limits.cpu in qooq.yaml (without it the limit is requests.cpu × the server's multiplier)",
			fmtCores(cpu), fmtCores(l.maxCPU))
	}
	if l.maxMem > 0 && mem > l.maxMem {
		return 0, 0, fmt.Errorf("the memory limit would be %s, and this server allows at most %s — lower limits.memory in qooq.yaml (without it the limit is requests.memory × the server's multiplier)",
			fmtBytes(mem), fmtBytes(l.maxMem))
	}
	if cpu == 0 {
		cpu = l.defaultCPU
	}
	if mem == 0 {
		mem = l.defaultMem
	}
	return cpu, mem, nil
}

func fmtCores(c float64) string { return strconv.FormatFloat(c, 'f', -1, 64) }

// fmtBytes is a byte count in the unit qooq.yaml would write it in.
func fmtBytes(b int64) string {
	const mi, gi = 1 << 20, 1 << 30
	switch {
	case b >= gi && b%gi == 0:
		return fmt.Sprintf("%dGi", b/gi)
	case b >= mi && b%mi == 0:
		return fmt.Sprintf("%dMi", b/mi)
	case b >= mi:
		return fmt.Sprintf("%.1fMi", float64(b)/mi)
	}
	return fmt.Sprintf("%d bytes", b)
}
