package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/pkg/client"
)

// Reconcile compares stored components with the live runtime state and
// corrects drift:
//   - store has a component but its container is gone  -> Running-desired
//     components are handed to the restart manager; Stopped-desired ones are
//     just marked Exited.
//   - container exists but is not Running while DesiredState is Running     -> handed to the restart manager too (covers a crash whose die event was
//     missed, or a container stopped by something outside qooqd).
//   - container exists and is Running (or DesiredState is Stopped)          -> adopt its live phase.
//   - managed container with no matching component, owned by this instance
//     and carrying a qooq.spec label -> reconstruct and adopt it as a new
//     component (M20, e.g. `docker compose down -v` wiped the bbolt volume
//     but left sibling containers running); otherwise log as orphan.
//   - managed container belonging to a different qooqd instance (qooq.instance
//     label mismatch) -> completely ignored, not even logged (see
//     LabelInstance's doc comment).
//
// It runs once at daemon boot and, via ReconcileLoop, on a fixed interval for
// the lifetime of the daemon: that interval is also the fallback driver that
// retries a scheduled restart if its in-process timer was lost (e.g. the
// daemon itself restarted mid-backoff).
func (s *Service) Reconcile(ctx context.Context) error {
	comps, err := s.store.ListComponents()
	if err != nil {
		return err
	}
	live, err := s.rt.ListContainers(ctx, runtime.Filter{
		Labels: map[string]string{LabelManaged: "true"},
		All:    true,
	})
	if err != nil {
		return err
	}

	byComponentID := make(map[string]runtime.ContainerStatus, len(live))
	for _, c := range live {
		if !s.OwnsContainer(c.Labels) {
			// Another qooqd instance's container sharing this docker host:
			// completely invisible to us, not even as an orphan candidate.
			continue
		}
		if cid := c.Labels[LabelComponentID]; cid != "" {
			byComponentID[cid] = c
		}
	}

	known := make(map[string]bool, len(comps))
	for _, comp := range comps {
		known[comp.ID] = true
		comp = s.ensureDesiredStateDefault(comp)
		comp = s.ensureNamespaceDefault(comp)

		cs, ok := byComponentID[comp.ID]
		if !ok {
			s.reconcileMissing(ctx, comp)
			continue
		}
		// A component whose container qooqd adopted rather than started (an
		// orphan, or a record written before started_at existed) has no start
		// time of its own, so it reports no uptime for as long as it runs.
		// The container knows when it started; ask it once.
		if cs.Phase == runtime.PhaseRunning && comp.Status.StartedAt.IsZero() {
			s.backfillStartedAt(ctx, comp.ID, cs.ID)
		}

		// Adopt whatever phase the container is actually in — including a down
		// container we still want running. Whether it gets restarted is the
		// restart manager's decision below, and under the default policy the
		// answer is no: recording it as Running until something restarts it
		// would describe a container that exited days ago as healthy, in the
		// one place an operator looks to find out.
		if comp.Status.Phase != cs.Phase || comp.Status.ContainerID != string(cs.ID) {
			changed, err := s.reconcileAdopt(comp.ID, cs)
			if err != nil {
				return err
			}
			if changed {
				s.log.Info("reconcile: adopted container",
					"component", comp.ID, "name", comp.Spec.Name, "phase", cs.Phase)
			}
		}

		if cs.Phase != runtime.PhaseRunning && comp.Status.DesiredState == DesiredRunning {
			// The container exists but is down (crashed, stopped externally)
			// while we still want it running: let the restart manager decide.
			// It re-reads the record and takes the lock itself, so this runs
			// after the adopt above rather than inside it.
			s.EvaluateRestart(ctx, comp.ID, RestartReasonDrift)
		}
	}

	for cid, c := range byComponentID {
		if !known[cid] {
			s.adoptOrphan(ctx, cid, c)
		}
	}
	return nil
}

// adoptOrphan attempts to reconstruct and adopt a Component record for a
// live, instance-owned, managed container with no matching store entry — the
// disaster-recovery path for "docker compose down -v wiped the bbolt volume
// but the sibling containers are still running".
//
// It requires the qooq.spec label (written at create time by createAndStart)
// to reconstruct a valid ComponentSpec; anything it cannot safely
// reconstruct or that conflicts with an existing component falls back to the
// pre-M20 warn-only behavior (no adoption, no state change).
//
// Concurrency: label parsing and the InspectContainer call happen before any
// lock is taken (pure/read-only work). The store read-check-write is then
// done under cid's per-id lock (re-reading the store first, the same
// "per-id lock, re-read, then write" pattern reconcileAdopt uses), so a
// second reconcile pass racing this one cannot double-adopt. The name
// uniqueness check + initial save go through reserveName — the same
// deployMu-guarded atomic section Deploy/Upsert use — because the per-id
// lock alone cannot exclude a concurrent Deploy of the *same name* under a
// *different* ULID (adversarial-review finding, 2026-07-17). Lock order is
// legal: deployMu is only ever taken as a leaf, here while holding cid's
// per-id lock, and reserveName itself takes no other lock.
func (s *Service) adoptOrphan(ctx context.Context, cid string, cs runtime.ContainerStatus) {
	specJSON := cs.Labels[LabelSpec]
	if specJSON == "" {
		s.log.Warn("reconcile: orphan managed container has no qooq.spec label, cannot adopt",
			"container", cs.ID, "component_id", cid, "name", cs.Name)
		return
	}
	var spec ComponentSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		s.log.Warn("reconcile: orphan container qooq.spec label is not valid JSON, cannot adopt",
			"container", cs.ID, "component_id", cid, "name", cs.Name, "err", err)
		return
	}
	spec, err := validateSpec(spec)
	if err != nil {
		s.log.Warn("reconcile: orphan container spec failed validation, cannot adopt",
			"container", cs.ID, "component_id", cid, "name", cs.Name, "err", err)
		return
	}

	// Re-inspect for an accurate phase/created time (the ListContainers
	// summary that got us here is cheaper but less precise — e.g. it cannot
	// distinguish a Stopped-desired clean exit from a crash the way
	// InspectContainer's exit-code-aware mapPhase can). The container may
	// have disappeared between the list and this call; that just means
	// there's nothing to adopt this pass, and it will be retried next pass
	// if it reappears.
	full, err := s.rt.InspectContainer(ctx, cs.ID)
	if err != nil {
		s.log.Warn("reconcile: orphan adopt: inspect container", "container", cs.ID, "component_id", cid, "err", err)
		return
	}

	lock := s.lockFor(cid)
	lock.Lock()
	defer lock.Unlock()

	if _, err := s.store.GetComponent(cid); err == nil {
		return // already adopted by a racing pass; nothing to do
	} else if !errors.Is(err, ErrNotFound) {
		s.log.Warn("reconcile: orphan adopt: load existing component", "component_id", cid, "err", err)
		return
	}

	// DesiredState is inferred from what we observe, never assumed: a
	// Running container becomes Running-desired; anything else becomes
	// Stopped-desired so we do not resurrect something the previous operator
	// deliberately stopped before the store was lost.
	desired := DesiredStopped
	if full.Phase == runtime.PhaseRunning {
		desired = DesiredRunning
	}
	now := time.Now()
	createdAt := full.Created
	if createdAt.IsZero() {
		createdAt = now
	}
	comp := &Component{
		ID:   cid,
		Spec: spec,
		Status: ComponentStatus{
			ContainerID:  string(full.ID),
			Phase:        full.Phase,
			DesiredState: desired,
		},
		CreatedAt: createdAt,
		UpdatedAt: now,
	}
	// Name conflict guard: never create a second component with the same
	// name as one the operator already (re)deployed by hand into the fresh
	// store — that would make the CLI's name-addressed commands ambiguous.
	// reserveName is the deployMu-guarded atomic check-and-save Deploy/Upsert
	// use; going through it (rather than a bare ListComponents scan + save)
	// is what makes this adoption safe against a concurrent Deploy of the
	// same name (see the doc comment above).
	if err := s.reserveName(comp); err != nil {
		if errors.Is(err, ErrNameConflict) {
			s.log.Warn("reconcile: orphan container name already used by another component, not adopting",
				"container", full.ID, "component_id", cid, "name", spec.Name, "err", err)
		} else {
			s.log.Error("reconcile: orphan adopt: save component", "component_id", cid, "err", err)
		}
		return
	}
	if err := s.store.AppendEvent(cid, Event{
		ComponentID: cid,
		Type:        "adopted_orphan",
		Message:     fmt.Sprintf("adopted orphaned container %s (desired=%s)", full.ID, desired),
		Time:        now,
	}); err != nil {
		s.log.Error("reconcile: orphan adopt: append event", "component_id", cid, "err", err)
	}
	s.log.Info("reconcile: adopted orphaned container",
		"component", cid, "name", spec.Name, "container", full.ID,
		"desired_state", desired, "phase", full.Phase)
}

// reconcileMissing handles a component whose container cannot be found at
// all. Running-desired components are handed to the restart manager (it
// takes the per-id lock itself); everything else is updated in place under
// the per-id lock, re-reading the record first to avoid clobbering a
// concurrent change.
func (s *Service) reconcileMissing(ctx context.Context, comp *Component) {
	// A component that already had a container (now gone, whatever its last
	// known phase) and is still desired Running is the restart manager's
	// job. A component that never got a container (ContainerID empty, e.g. a
	// deploy interrupted mid-create) falls through to the plain bookkeeping
	// below regardless of DesiredState.
	if comp.Status.DesiredState == DesiredRunning && comp.Status.ContainerID != "" {
		s.EvaluateRestart(ctx, comp.ID, RestartReasonDrift)
		return
	}

	lock := s.lockFor(comp.ID)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := s.store.GetComponent(comp.ID)
	if err != nil {
		return
	}
	// "The container is missing" came from the snapshot taken before this
	// lock, and a deploy may have been holding the lock to put a new one in
	// place — an upsert clears ContainerID and sets the new one. Re-reading
	// the record is not enough when the premise is stale: seen for real, this
	// marked a freshly deployed, running service Exited 139ms after the
	// upsert, the watcher's check then reported the new version as not having
	// started, and the deploy counted toward pausing the project. A different
	// container (empty to set included) is a different fact; the next pass
	// judges it. Same rule as NoteHealthy and HandleEvent's die branch.
	if fresh.Status.ContainerID != comp.Status.ContainerID {
		return
	}
	switch fresh.Status.Phase {
	case runtime.PhaseRunning:
		// Container disappeared while the daemon was down and we no longer
		// want it running (DesiredState is Stopped).
		fresh.Status.Phase = runtime.PhaseExited
		fresh.Status.FinishedAt = time.Now()
		fresh.UpdatedAt = time.Now()
		if err := s.store.SaveComponent(fresh); err != nil {
			s.log.Error("reconcile: persist missing-container state", "component", fresh.ID, "err", err)
			return
		}
		s.log.Info("reconcile: container missing, marked exited",
			"component", fresh.ID, "name", fresh.Spec.Name)
	case runtime.PhasePending, runtime.PhaseCreating:
		// Deploy was interrupted before the container existed; without this
		// the record stays Pending forever.
		fresh.Status.Phase = runtime.PhaseFailed
		fresh.UpdatedAt = time.Now()
		if err := s.store.SaveComponent(fresh); err != nil {
			s.log.Error("reconcile: persist interrupted-deploy state", "component", fresh.ID, "err", err)
			return
		}
		s.log.Warn("reconcile: deploy interrupted, marked failed",
			"component", fresh.ID, "name", fresh.Spec.Name)
	}
}

// reconcileAdopt patches a component's Phase/ContainerID to match the live
// container under the per-id lock, re-reading the record first so it never
// clobbers a concurrent write (e.g. HandleEvent or performRestart updating
// CrashCount/RestartCount/BackoffUntil) with the stale snapshot Reconcile's
// outer ListComponents call took.
func (s *Service) reconcileAdopt(id string, cs runtime.ContainerStatus) (bool, error) {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := s.store.GetComponent(id)
	if err != nil {
		return false, nil
	}
	phase := cs.Phase
	// The runtime cannot know an exit was operator-intended: a stopped
	// container's non-zero exit code (apps that don't trap SIGTERM) maps to
	// Failed at the adapter level. Mirror HandleEvent's stop semantics here
	// so reconcile doesn't flip an intentional Exited back to Failed.
	if phase == runtime.PhaseFailed && fresh.Status.DesiredState == DesiredStopped {
		phase = runtime.PhaseExited
	}
	// "created" only means something while a deploy is in flight, and a deploy
	// holds this same lock for its whole duration — so by the time reconcile
	// can look, it is over. What is left is a container that was created and
	// never started: a deployment that failed, not one still working. Adopting
	// Creating here would overwrite the verdict the deploy recorded (and leave
	// its reason in LastError contradicting the phase beside it), replacing it
	// with a phase that reads as "in progress" — so an operator waits for
	// something that is never going to finish.
	if phase == runtime.PhaseCreating && fresh.Status.Phase == runtime.PhaseFailed {
		phase = runtime.PhaseFailed
	}
	if fresh.Status.Phase == phase && fresh.Status.ContainerID == string(cs.ID) {
		return false, nil
	}
	fresh.Status.Phase = phase
	fresh.Status.ContainerID = string(cs.ID)
	fresh.UpdatedAt = time.Now()
	return true, s.store.SaveComponent(fresh)
}

// backfillStartedAt records when an adopted container actually started, for
// components that carry no start time of their own.
//
// The inspect happens outside the lock (it is a read of the runtime, not of
// qooq's state) and the result is only applied if the record still lacks a
// start time and still points at the same container — anything that ran in
// between knows more than this does. A container the runtime cannot date is
// left alone rather than dated "now", which would restart the stable window
// on every daemon boot.
func (s *Service) backfillStartedAt(ctx context.Context, id string, cid runtime.ContainerID) {
	full, err := s.rt.InspectContainer(ctx, cid)
	if err != nil || full.StartedAt.IsZero() {
		return
	}

	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := s.store.GetComponent(id)
	if err != nil {
		return
	}
	if !fresh.Status.StartedAt.IsZero() || fresh.Status.ContainerID != string(cid) {
		return
	}
	fresh.Status.StartedAt = full.StartedAt
	fresh.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(fresh); err != nil {
		s.log.Warn("reconcile: record adopted start time", "component", id, "err", err)
	}
}

// ensureDesiredStateDefault back-fills DesiredState for components persisted
// before M6 introduced the field, inferring it from the last known Phase so
// old data does not silently stop being managed. It is idempotent and safe to
// call on every reconcile pass.
func (s *Service) ensureDesiredStateDefault(comp *Component) *Component {
	if comp.Status.DesiredState != "" {
		return comp
	}

	lock := s.lockFor(comp.ID)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := s.store.GetComponent(comp.ID)
	if err != nil {
		return comp
	}
	if fresh.Status.DesiredState != "" {
		return fresh
	}
	if fresh.Status.Phase == runtime.PhaseRunning {
		fresh.Status.DesiredState = DesiredRunning
	} else {
		fresh.Status.DesiredState = DesiredStopped
	}
	fresh.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(fresh); err != nil {
		s.log.Error("reconcile: persist desired-state default", "component", fresh.ID, "err", err)
	}
	return fresh
}

// ensureNamespaceDefault fills in the namespace of components persisted while
// "no namespace" was still spelled as the empty string. It is idempotent and
// safe to call on every reconcile pass, following ensureDesiredStateDefault.
//
// This only rewrites a record. ContainerName does not qualify names in the
// default namespace, so the container the record points at keeps the name, the
// network aliases and the ingress routes it already had: nothing has to be
// recreated for the migration to take effect.
func (s *Service) ensureNamespaceDefault(comp *Component) *Component {
	if comp.Spec.Namespace != "" {
		return comp
	}

	lock := s.lockFor(comp.ID)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := s.store.GetComponent(comp.ID)
	if err != nil {
		return comp
	}
	if fresh.Spec.Namespace != "" {
		return fresh
	}
	fresh.Spec.Namespace = client.DefaultNamespace
	fresh.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(fresh); err != nil {
		s.log.Error("reconcile: persist namespace default", "component", fresh.ID, "err", err)
		return comp
	}
	return fresh
}

// ReconcileLoop runs Reconcile on a fixed interval until ctx is cancelled. It
// is the persistent counterpart to the boot-time Reconcile call, and also the
// fallback driver for a scheduled restart whose in-process timer was lost.
func (s *Service) ReconcileLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Reconcile(ctx); err != nil {
				s.log.Warn("reconcile loop: pass failed", "err", err)
			}
		}
	}
}
