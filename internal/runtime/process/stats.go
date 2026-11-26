//go:build unix

package process

import (
	"context"
	"fmt"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// statsInterval matches the docker adapter's effective sampling cadence
// closely enough for the crash/monitoring logic that consumes StatSample
// (which only cares about "latest sample", not a specific rate).
const statsInterval = 2 * time.Second

// Stats streams CPU%/memory samples for id every statsInterval until ctx is
// cancelled or the process is found to have exited. Actual sampling is
// platform-specific: see sampleUsage in stats_linux.go (real /proc reads) and
// stats_other.go (always-zero fallback, documented as a v1 limitation for
// non-Linux hosts).
func (a *Adapter) Stats(ctx context.Context, id runtime.ContainerID) (<-chan runtime.StatSample, <-chan error) {
	out := make(chan runtime.StatSample)
	errs := make(chan error, 1)

	inst, ok := a.getInstance(id)
	if !ok {
		errs <- fmt.Errorf("process runtime: stats: container %s not found", id)
		close(errs)
		close(out)
		return out, errs
	}

	go func() {
		defer close(out)

		ticker := time.NewTicker(statsInterval)
		defer ticker.Stop()

		var prev cpuSample
		inst.mu.Lock()
		memLimit := uint64(0)
		if inst.meta.MemBytes > 0 {
			memLimit = uint64(inst.meta.MemBytes)
		}
		inst.mu.Unlock()

		for {
			select {
			case <-ctx.Done():
				close(errs)
				return
			case <-ticker.C:
				inst.mu.Lock()
				pid := inst.meta.PID
				exited := inst.meta.Exited
				inst.mu.Unlock()
				if exited || pid == 0 {
					close(errs)
					return
				}

				cpuPct, mem, cur, sampled := sampleUsage(pid, prev)
				if !sampled {
					close(errs)
					return
				}
				prev = cur

				sample := runtime.StatSample{
					ContainerID: id,
					CPUPercent:  cpuPct,
					MemUsage:    mem,
					MemLimit:    memLimit,
					Time:        time.Now(),
				}
				select {
				case out <- sample:
				case <-ctx.Done():
					close(errs)
					return
				}
			}
		}
	}()

	return out, errs
}
