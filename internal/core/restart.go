package core

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// RestartPolicy values. Empty string is treated as RestartPolicyAlways: a
// service that stops — a crash, a docker restart, a reboot — comes back unless
// someone said otherwise (2026-11-12, after D1 found users' apps staying
// down while their databases came back). A component stopped on purpose is
// DesiredStopped and is not restarted whatever its policy.
const (
	RestartPolicyNo        = "no"
	RestartPolicyOnFailure = "on-failure"
	RestartPolicyAlways    = "always"
)

// Restart trigger reasons passed to EvaluateRestart.
const (
	// RestartReasonCrash is used right after a "die" lifecycle event, where
	// comp.Status.LastExitCode reflects the container's actual exit code.
	RestartReasonCrash = "crash"
	// RestartReasonDrift is used when the reconciler notices the container is
	// gone or not running while DesiredState is still Running (no fresh exit
	// code is available for this trigger).
	RestartReasonDrift = "drift"
	// RestartReasonHealth is used by the monitor's health checker when a
	// component's liveness probe (http or exec) fails FailureThreshold times
	// in a row. Unlike crash/drift, the container itself is typically still
	// Running (the process is up but unresponsive), so EvaluateRestart and
	// performRestart must not treat "already Running" as a no-op for this
	// reason — they force a stop+recreate instead.
	RestartReasonHealth = "health"
)

// ErrInvalidRestartPolicy is returned by Deploy when RestartPolicy is set to
// anything other than "", "no", "on-failure" or "always".
var ErrInvalidRestartPolicy = errors.New(`restart_policy must be one of "", "no", "on-failure" or "always"`)

const (
	// restartBackoffBase is the first retry delay; each subsequent retry
	// doubles it, capped at restartBackoffCap.
	restartBackoffBase = 1 * time.Second
	restartBackoffCap  = 60 * time.Second
	// restartStableWindow is how long a component must have stayed Running
	// since its last (re)start before a fresh crash resets RestartCount to 0.
	restartStableWindow = 60 * time.Second
	// maxConsecutiveRestarts is the crash-loop terminal-state threshold
	// once a component accumulates this many
	// consecutive restarts (never having stayed Running for
	// restartStableWindow in between), the restart manager stops retrying,
	// marks the component Failed, and sets Status.RestartExhausted. A manual
	// Start, Upsert (replace) or Rollback clears it and gives the component a
	// fresh run of attempts.
	maxConsecutiveRestarts = 10
)

// validRestartPolicy reports whether p is an accepted RestartPolicy value.
func validRestartPolicy(p string) bool {
	switch p {
	case "", RestartPolicyNo, RestartPolicyOnFailure, RestartPolicyAlways:
		return true
	default:
		return false
	}
}

// backoffDuration returns the delay before the (count+1)-th restart attempt:
// 1s, 2s, 4s, 8s, ... capped at restartBackoffCap.
func backoffDuration(count int) time.Duration {
	if count < 0 {
		count = 0
	}
	if count > 6 { // 1s*2^6 = 64s already exceeds the cap; avoid shift overflow
		count = 6
	}
	d := restartBackoffBase * time.Duration(int64(1)<<uint(count))
	if d > restartBackoffCap {
		d = restartBackoffCap
	}
	return d
}

// shouldRestart applies the restart policy to decide whether a Running-desired
// component whose container is down should be recreated.
func shouldRestart(policy, reason string, lastExitCode int) bool {
	switch policy {
	case RestartPolicyAlways:
		return true
	case RestartPolicyOnFailure:
		if reason == RestartReasonCrash {
			return lastExitCode != 0
		}
		// The container vanished (or is not running) without a fresh exit
		// code to check: treat this as a failure relative to the desired
		// state, same as k3s/kubelet would for an OnFailure pod.
		return true
	default: // "no" or unrecognized
		return false
	}
}

// EvaluateRestart is the restart manager's single entry point. It is the only
// place besides createAndStart's callers that decides "does this component's
// container need to be (re)created". Callers (the monitor's die-event
// handler, and the reconcile loop for drifted/missing containers) only
// trigger it; they never recreate a container directly.
//
// It takes the per-component lock itself, re-reads the persisted state, and
// is idempotent: if a restart is already scheduled (BackoffUntil is in the
// future) or the container is already actually running, it is a no-op.
func (s *Service) EvaluateRestart(ctx context.Context, id, reason string) {
	lock := s.lockFor(id)
	lock.Lock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		lock.Unlock()
		return
	}
	if comp.Status.DesiredState != DesiredRunning {
		lock.Unlock()
		return
	}
	if comp.Status.RestartExhausted {
		// Give up permanently until a manual Start/Upsert/Rollback clears the
		// flag: without this, reconcile's 15s pass and further die/health
		// events would keep re-entering this function and (harmlessly, but
		// pointlessly) re-deriving the same "already exhausted" outcome.
		lock.Unlock()
		return
	}

	policy := comp.Spec.RestartPolicy
	if policy == "" {
		policy = RestartPolicyAlways
	}
	if !shouldRestart(policy, reason, comp.Status.LastExitCode) {
		lock.Unlock()
		return
	}

	now := time.Now()
	if !comp.Status.BackoffUntil.IsZero() && now.Before(comp.Status.BackoffUntil) {
		// A restart is already scheduled; let its timer fire.
		lock.Unlock()
		return
	}

	if comp.Status.ContainerID != "" && reason != RestartReasonHealth {
		// For crash/drift the container is already down by definition, so an
		// Inspect that still shows Running means we raced with another
		// trigger: nothing to do. A health-triggered restart is different —
		// the container is genuinely Running (the process is up but the
		// liveness probe is failing), so this short-circuit must not apply.
		if cs, ierr := s.rt.InspectContainer(ctx, runtime.ContainerID(comp.Status.ContainerID)); ierr == nil && cs.Phase == runtime.PhaseRunning {
			lock.Unlock()
			return
		}
	}

	if !comp.Status.StartedAt.IsZero() && now.Sub(comp.Status.StartedAt) >= restartStableWindow {
		comp.Status.RestartCount = 0
		comp.Status.RestartExhausted = false
	}
	comp.Status.RestartCount++

	if comp.Status.RestartCount >= maxConsecutiveRestarts {
		comp.Status.RestartExhausted = true
		comp.Status.Phase = runtime.PhaseFailed
		comp.Status.BackoffUntil = time.Time{}
		comp.UpdatedAt = now
		if err := s.store.SaveComponent(comp); err != nil {
			s.log.Error("restart manager: persist exhausted state", "id", id, "err", err)
		}
		if err := s.store.AppendEvent(id, Event{
			ComponentID: id,
			Type:        "restart_exhausted",
			Message:     fmt.Sprintf("gave up after %d consecutive restarts", comp.Status.RestartCount),
			Time:        now,
		}); err != nil {
			s.log.Error("restart manager: append restart_exhausted event", "id", id, "err", err)
		}
		s.log.Warn("restart manager: restart attempts exhausted, giving up",
			"id", id, "name", comp.Spec.Name, "restart_count", comp.Status.RestartCount)
		lock.Unlock()
		return
	}

	backoff := backoffDuration(comp.Status.RestartCount - 1)
	comp.Status.BackoffUntil = now.Add(backoff)
	comp.UpdatedAt = now
	if err := s.store.SaveComponent(comp); err != nil {
		s.log.Error("restart manager: persist scheduled restart", "id", id, "err", err)
	}
	if reason == RestartReasonHealth {
		if err := s.store.AppendEvent(id, Event{
			ComponentID: id,
			Type:        "health_restart",
			Message:     "health check failed; restart scheduled",
			Time:        now,
		}); err != nil {
			s.log.Error("restart manager: append health event", "id", id, "err", err)
		}
	}
	s.log.Info("restart manager: scheduling restart",
		"id", id, "name", comp.Spec.Name, "reason", reason,
		"backoff", backoff, "restart_count", comp.Status.RestartCount)
	lock.Unlock()

	time.AfterFunc(backoff, func() {
		s.recoverRestartPanic(ctx, id, reason, func() { s.performRestart(ctx, id, reason) })
	})
}

// recoverRestartPanic runs fn (a scheduled restart) behind a recover
// boundary. fn runs on the goroutine time.AfterFunc spawns for it, with
// nothing else on that call stack to catch a panic — left unrecovered, it
// would crash the whole qooqd process over one component's bad luck, taking
// every other component's restart/health/reconcile handling down with it.
//
// Recovery is logged (ERROR, with the component id/reason and a full stack
// trace) and, when the component still exists, recorded as a panic_recovered
// event via NotePanicRecovered — the same "log + durable record" pairing used
// wherever else this codebase catches a panic, because a recovered panic that
// leaves no trace anywhere is worse than letting the process crash: a crash
// at least gets noticed.
func (s *Service) recoverRestartPanic(ctx context.Context, id, reason string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.log.Error("restart manager: panic recovered during scheduled restart",
				"id", id, "reason", reason, "panic", r, "stack", string(stack))
			s.NotePanicRecovered(ctx, id, fmt.Sprintf("restart (%s)", reason), r)
		}
	}()
	fn()
}

// performRestart recreates a component's container. It is only ever invoked
// via the timer scheduled by EvaluateRestart, and re-validates everything
// under the per-component lock before touching the runtime, so a change of
// mind (Stop, Remove, a concurrent restart) in the meantime is respected.
func (s *Service) performRestart(ctx context.Context, id, reason string) {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		return
	}
	if comp.Status.DesiredState != DesiredRunning {
		return
	}

	if comp.Status.ContainerID != "" {
		if reason != RestartReasonHealth {
			if cs, ierr := s.rt.InspectContainer(ctx, runtime.ContainerID(comp.Status.ContainerID)); ierr == nil && cs.Phase == runtime.PhaseRunning {
				return // already running; idempotent no-op
			}
		}
		// force=true also stops a still-Running container (the health case).
		if err := s.rt.RemoveContainer(ctx, runtime.ContainerID(comp.Status.ContainerID), true); err != nil {
			s.log.Warn("restart manager: remove old container", "id", id, "err", err)
		}
	}

	if err := s.createAndStart(ctx, comp); err != nil {
		s.log.Error("restart manager: recreate failed", "id", id, "err", err)
		return
	}
	comp.Status.LastRestartAt = time.Now()
	comp.UpdatedAt = comp.Status.LastRestartAt
	if err := s.store.SaveComponent(comp); err != nil {
		s.log.Error("restart manager: persist restarted component", "id", id, "err", err)
		return
	}
	s.log.Info("restart manager: container recreated",
		"id", id, "name", comp.Spec.Name, "restart_count", comp.Status.RestartCount,
		"container", comp.Status.ContainerID)
}
