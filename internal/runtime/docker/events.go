package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Events streams neutral container lifecycle events for containers matching the
// filter's labels. Only container-scoped events are requested and only the
// lifecycle actions qooq cares about are forwarded; everything else is dropped.
// The stream ends (both channels drained/closed) when ctx is cancelled or the
// daemon closes the connection.
func (a *Adapter) Events(ctx context.Context, filter runtime.Filter) (<-chan runtime.Event, <-chan error) {
	out := make(chan runtime.Event)
	errs := make(chan error, 1)

	args := filters.NewArgs()
	args.Add("type", string(events.ContainerEventType))
	for k, v := range filter.Labels {
		args.Add("label", k+"="+v)
	}

	msgs, derr := a.cli.Events(ctx, events.ListOptions{Filters: args})

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				close(errs)
				return
			case e := <-derr:
				errs <- e
				close(errs)
				return
			case m, ok := <-msgs:
				if !ok {
					close(errs)
					return
				}
				ev, keep := mapEvent(m)
				if !keep {
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					errs <- ctx.Err()
					close(errs)
					return
				}
			}
		}
	}()

	return out, errs
}

// mapEvent translates a Docker event Message into a neutral runtime.Event. keep
// is false for actions qooq does not track (exec_*, attach, health, ...).
func mapEvent(m events.Message) (runtime.Event, bool) {
	var action string
	switch m.Action {
	case events.ActionCreate:
		action = "create"
	case events.ActionStart:
		action = "start"
	case events.ActionDie:
		action = "die"
	case events.ActionStop:
		action = "stop"
	case events.ActionDestroy:
		action = "destroy"
	case events.ActionOOM:
		action = "oom"
	default:
		return runtime.Event{}, false
	}

	ev := runtime.Event{
		ContainerID: runtime.ContainerID(m.Actor.ID),
		Action:      action,
		Labels:      m.Actor.Attributes,
		Time:        eventTime(m),
	}
	if code, ok := m.Actor.Attributes["exitCode"]; ok {
		if n, err := strconv.Atoi(code); err == nil {
			ev.ExitCode = n
		}
	}
	return ev, true
}

func eventTime(m events.Message) time.Time {
	if m.TimeNano != 0 {
		return time.Unix(0, m.TimeNano)
	}
	if m.Time != 0 {
		return time.Unix(m.Time, 0)
	}
	return time.Now()
}

// Stats streams normalized CPU/memory samples for a container using the Docker
// streaming stats endpoint (no polling). CPU% uses the official formula:
// cpuDelta/systemDelta*onlineCPUs*100. The stream stops when ctx is cancelled.
func (a *Adapter) Stats(ctx context.Context, id runtime.ContainerID) (<-chan runtime.StatSample, <-chan error) {
	out := make(chan runtime.StatSample)
	errs := make(chan error, 1)

	go func() {
		defer close(out)

		resp, err := a.cli.ContainerStats(ctx, string(id), true)
		if err != nil {
			errs <- err
			close(errs)
			return
		}
		defer resp.Body.Close()

		dec := json.NewDecoder(resp.Body)
		for {
			var v container.StatsResponse
			if err := dec.Decode(&v); err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					close(errs)
					return
				}
				errs <- err
				close(errs)
				return
			}

			sample := runtime.StatSample{
				ContainerID: id,
				CPUPercent:  calcCPUPercent(v),
				MemUsage:    v.MemoryStats.Usage,
				MemLimit:    v.MemoryStats.Limit,
				Time:        v.Read,
			}
			select {
			case out <- sample:
			case <-ctx.Done():
				close(errs)
				return
			}
		}
	}()

	return out, errs
}

// calcCPUPercent applies Docker's official CPU-usage formula.
func calcCPUPercent(v container.StatsResponse) float64 {
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
	onlineCPUs := float64(v.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(v.CPUStats.CPUUsage.PercpuUsage))
	}
	if systemDelta > 0 && cpuDelta > 0 {
		return (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}
	return 0
}
