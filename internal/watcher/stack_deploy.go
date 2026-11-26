package watcher

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// projectOutcome is what one project did during one deploy.
type projectOutcome string

const (
	// projectDeployed means the project's pipeline ran without error — including
	// the common case where there was nothing new to deploy.
	projectDeployed projectOutcome = "deployed"
	// projectFailed means its pipeline failed, or it deployed but never became
	// ready for a dependent that waits on readiness.
	projectFailed projectOutcome = "failed"
	// projectSkipped means it was not attempted because something it waits for
	// did not get there. A skipped project is not a stopped project: whatever
	// version of it is already running keeps running.
	projectSkipped projectOutcome = "skipped"
)

// lockStack serializes deploying one stack, the way lockTarget does for a target.
// A trigger arriving mid-deploy waits rather than starting a second walk over
// the same projects.
func (w *Watcher) lockStack(name string) (unlock func()) {
	v, _ := w.stackLocks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// sweepStacks deploys every stack whose check is due.
func (w *Watcher) sweepStacks(ctx context.Context) {
	due, err := dueStacks(w.db, time.Now())
	if err != nil {
		w.log.Error("sweep: list due stacks failed", zap.Error(err))
		return
	}
	for i := range due {
		w.sweepStackSafe(ctx, due[i].Name)
	}
}

// sweepStackSafe runs sweepStack for one stack behind a recover boundary. It
// is called in a loop over every due stack in the same pass (sweepStacks),
// itself run on the single stack worker goroutine (runStackWorker, queue.go)
// — a panic while deploying one stack must not stop the pass from reaching
// the stacks after it, and must not escape to that goroutine either: nothing
// above it recovers, so it would crash the whole qooq-watcher process over a
// bug in one stack's manifest or one of its projects.
func (w *Watcher) sweepStackSafe(ctx context.Context, name string) {
	w.recoverStackPanic(name, func() { w.sweepStack(ctx, name) })
}

// recoverStackPanic runs fn and, if it panics, records the outcome against
// the named stack instead of letting the panic escape: an ERROR log (with a
// full stack trace) for daemon-log watchers, and a Status/LastError update —
// the same fields an ordinary deploy failure sets via touchStack — so the
// fact that it happened is visible from `qooq stack get`.
func (w *Watcher) recoverStackPanic(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			w.recordStackPanic(name, r, debug.Stack())
		}
	}()
	fn()
}

// recordStackPanic is recoverStackPanic's log+persist half, split out so
// tests can drive it directly without needing to trigger a real panic deep in
// a stack's manifest sync or project walk.
func (w *Watcher) recordStackPanic(name string, r any, stack []byte) {
	w.log.Error("stack worker: panic recovered, continuing",
		zap.String("stack", name), zap.Any("panic", r), zap.ByteString("stack_trace", stack))
	s, err := getStackByName(w.db, name)
	if err != nil {
		// Deleted since the panic — nothing left to record it on; the ERROR
		// log above is the only trace.
		return
	}
	w.touchStack(s, StackStatusFailed, fmt.Sprintf("internal error: panic recovered: %v", r), "")
}

// sweepStack deploys one stack under its lock, re-reading the row inside the lock
// for the same reason sweepTarget does: a resume or a trigger may have landed
// between the batch read and here.
func (w *Watcher) sweepStack(ctx context.Context, name string) {
	unlock := w.lockStack(name)
	defer unlock()

	stack, err := getStackByName(w.db, name)
	if err != nil {
		w.log.Debug("sweep: stack vanished before processing", zap.String("stack", name), zap.Error(err))
		return
	}
	dueBefore := stack.NextCheckAt

	w.deployStack(ctx, stack)

	interval := time.Duration(stack.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultPollIntervalSeconds * time.Second
	}
	if applied, err := rescheduleStack(w.db, stack.ID, interval, dueBefore); err != nil {
		w.log.Error("sweep: reschedule stack failed", zap.String("stack", stack.Name), zap.Error(err))
	} else if !applied {
		w.log.Debug("sweep: stack reschedule skipped, poke pending", zap.String("stack", stack.Name))
	}
}

// deployStack runs one full pass: fetch the manifest, make the project targets match
// it, then walk the DAG deploying what can be deployed.
func (w *Watcher) deployStack(ctx context.Context, stack *Stack) {
	if stack.Status == StackStatusPaused {
		// Paused freezes deployment, not the stack: every container it already
		// started keeps running. Only `stack resume` lifts it.
		w.log.Debug("stack is paused, not deploying",
			zap.String("stack", stack.Name), zap.String("reason", stack.PausedReason))
		return
	}

	r, err := w.syncStackManifest(stack)
	if err != nil {
		msg := maskRepoCredentials(err.Error(), stack.RepoURL)
		w.log.Error("stack: manifest sync failed", zap.String("stack", stack.Name), zap.String("err", msg))
		w.touchStack(stack, StackStatusFailed, msg, "")
		return
	}

	targets, orphans, err := w.reconcileProjects(stack, r)
	if err != nil {
		msg := maskRepoCredentials(err.Error(), stack.RepoURL)
		w.log.Error("stack: project reconcile failed", zap.String("stack", stack.Name), zap.String("err", msg))
		w.touchStack(stack, StackStatusFailed, msg, "")
		return
	}
	for _, name := range orphans {
		// Reported every pass on purpose: an orphan is a divergence between
		// what is deployed and what the manifest describes, and silence about it
		// is how a forgotten component ends up running for months.
		w.log.Warn("stack: project is no longer in the manifest (not removed; use stack rm --prune or watch rm)",
			zap.String("stack", stack.Name), zap.String("project", name))
	}

	outcomes := w.walkProjects(ctx, stack, r, targets)
	w.finishDeploy(stack, r, targets, outcomes, orphans)
}

// walkProjects deploys the projects in dependency order, honouring each project's
// depends_on: and skipping whatever cannot go yet.
//
// The order is fixed at parse time (deployOrder), so this walk is sequential:
// one project at a time, no goroutines. Independent branches could run in
// parallel, but a single-VM host is building images — the bottleneck is the
// docker daemon, not the walk — and a sequential deploy log is one anybody can
// read top to bottom.
func (w *Watcher) walkProjects(ctx context.Context, stack *Stack, r *stackManifest, targets map[string]*Target) map[string]projectOutcome {
	order, err := r.deployOrder()
	if err != nil {
		// Unreachable: parseStackManifest computes the order and refuses a cycle.
		w.log.Error("stack: deploy order", zap.String("stack", stack.Name), zap.Error(err))
		return nil
	}

	outcomes := make(map[string]projectOutcome, len(order))
	readiness := make(map[string]error, len(order))

	for _, name := range order {
		project := r.Projects[name]

		if blocked := blockedDeps(project, outcomes); len(blocked) > 0 {
			outcomes[name] = projectSkipped
			w.log.Info("stack: project skipped, waiting on an upstream project that did not deploy",
				zap.String("stack", stack.Name), zap.String("project", name), zap.Strings("waiting_on", blocked))
			continue
		}

		if dep, err := w.awaitDeps(ctx, stack, project, outcomes, readiness); err != nil {
			// The project that never became ready is the one that failed; this
			// one just could not be attempted. Attributing the failure here
			// instead would blame the wrong project and count against the wrong
			// failure budget.
			outcomes[dep] = projectFailed
			outcomes[name] = projectSkipped
			w.log.Error("stack: upstream project never became ready",
				zap.String("stack", stack.Name), zap.String("project", dep), zap.Error(err))
			if t := targets[dep]; t != nil {
				w.recordFailure(t, fmt.Errorf("deployed, but never became ready: %w", err))
			}
			continue
		}

		outcomes[name] = w.deployProject(ctx, stack, name, targets)
	}
	return outcomes
}

// blockedDeps returns the dependencies of project that did not deploy this pass —
// the reason to skip it. A dependency with no recorded outcome cannot happen
// (the walk is in topological order), and is treated as fine rather than
// blocking, so a bug here degrades to "deploy it" rather than "never deploy
// anything again".
func blockedDeps(project stackProject, outcomes map[string]projectOutcome) []string {
	var blocked []string
	for _, dep := range project.DependsOn {
		switch outcomes[dep.Name] {
		case projectFailed, projectSkipped:
			blocked = append(blocked, dep.Name)
		}
	}
	return blocked
}

// awaitDeps blocks until every dependency this project waits on has reached the
// level it asked for. It returns the name of the dependency that did not get
// there, if any.
//
// wait: started needs no waiting at all — the walk is sequential, so an
// upstream project's deploy has already returned by the time we get here. Only
// wait: ready (the default) actually blocks.
func (w *Watcher) awaitDeps(ctx context.Context, stack *Stack, project stackProject, outcomes map[string]projectOutcome, readiness map[string]error) (string, error) {
	for _, dep := range project.DependsOn {
		if dep.Wait != waitReady || outcomes[dep.Name] != projectDeployed {
			continue
		}
		if err := w.projectReady(ctx, stack, dep.Name, readiness); err != nil {
			return dep.Name, err
		}
	}
	return "", nil
}

// projectReady waits for one project's component to be running, memoizing the result
// so several dependents waiting on the same project pay for it once.
func (w *Watcher) projectReady(ctx context.Context, stack *Stack, name string, readiness map[string]error) error {
	if err, done := readiness[name]; done {
		return err
	}
	err := w.waitProjectReady(ctx, stack, name)
	readiness[name] = err
	return err
}

func (w *Watcher) waitProjectReady(ctx context.Context, stack *Stack, name string) error {
	ref := componentRef{namespace: stack.Name, name: name}
	comp, err := w.findComponent(ctx, ref)
	if err != nil {
		return fmt.Errorf("look up component: %w", err)
	}
	if comp == nil {
		return fmt.Errorf("no component %s exists", ref)
	}
	return w.waitForComponentReady(ctx, comp.ID, ref.String())
}

// deployProject runs one project's pipeline, under that target's own lock so a manual
// `watch deploy`/webhook on the same target still serializes against it.
func (w *Watcher) deployProject(ctx context.Context, stack *Stack, name string, targets map[string]*Target) projectOutcome {
	targetName := projectTargetName(stack.Name, name)

	unlock := w.lockTarget(targetName)
	defer unlock()

	// Re-read inside the lock: reconcileProjects may have set force_rebuild, and
	// something else may have touched the row since.
	t, err := getTargetByName(w.db, targetName)
	if err != nil {
		w.log.Error("stack: project target vanished", zap.String("stack", stack.Name), zap.String("project", name), zap.Error(err))
		return projectFailed
	}
	targets[name] = t

	if err := w.decryptTarget(t); err != nil {
		w.recordFailure(t, err)
		return projectFailed
	}

	w.checkAndMaybeBuild(ctx, t)

	if buildBlocked(t.LastBuildStatus) {
		return projectFailed
	}
	return projectDeployed
}

// finishDeploy records the outcome: per-project failure counters, the stack's
// summary status, and the pause that stops a permanently broken project from
// letting its healthy siblings drift arbitrarily far ahead of it.
func (w *Watcher) finishDeploy(stack *Stack, r *stackManifest, targets map[string]*Target, outcomes map[string]projectOutcome, orphans []string) {
	maxFailures := r.Policy.maxFailures()
	var pausedReason string
	var problems []string

	for _, name := range sortedProjectNames(r.Projects) {
		t := targets[name]
		if t == nil {
			continue
		}
		switch outcomes[name] {
		case projectDeployed:
			if t.ConsecutiveFailures != 0 {
				w.setProjectFailures(t, 0)
			}
		case projectFailed:
			n := t.ConsecutiveFailures + 1
			w.setProjectFailures(t, n)
			problems = append(problems, fmt.Sprintf("%s: %s", name, dashIfBlank(t.LastBuildError)))
			if maxFailures > 0 && n >= maxFailures && pausedReason == "" {
				pausedReason = fmt.Sprintf("project %q failed %d times in a row; fix it and run `qooq stack resume %s`", name, n, stack.Name)
			}
		case projectSkipped:
			problems = append(problems, name+": skipped (an upstream project did not deploy)")
		}
	}

	status := stackStatusFor(outcomes)
	if pausedReason != "" {
		status = StackStatusPaused
		w.log.Warn("stack paused", zap.String("stack", stack.Name), zap.String("reason", pausedReason))
	}
	if len(orphans) > 0 {
		problems = append(problems, "no longer in the manifest: "+strings.Join(orphans, ", "))
	}

	w.touchStack(stack, status, truncateError(strings.Join(problems, "; ")), pausedReason)
	w.log.Info("stack deployed",
		zap.String("stack", stack.Name), zap.String("status", status),
		zap.Int("projects", len(outcomes)), zap.Any("outcomes", outcomes))
}

// stackStatusFor summarizes the pass. degraded is the honest name for "some
// projects moved and others did not": the stack is running a mix of versions.
func stackStatusFor(outcomes map[string]projectOutcome) string {
	if len(outcomes) == 0 {
		return StackStatusNever
	}
	deployed := 0
	for _, o := range outcomes {
		if o == projectDeployed {
			deployed++
		}
	}
	switch {
	case deployed == len(outcomes):
		return StackStatusSuccess
	case deployed == 0:
		return StackStatusFailed
	default:
		return StackStatusDegraded
	}
}

func (w *Watcher) setProjectFailures(t *Target, n int) {
	t.ConsecutiveFailures = n
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Update("consecutive_failures", n).Error; err != nil {
		w.log.Warn("stack: persist project failure count failed", zap.String("target", t.Name), zap.Error(err))
	}
}

// resumeStack lifts a pause: every project gets a fresh failure budget and the
// stack is deployed again on the next tick.
func (w *Watcher) resumeStack(stack *Stack) error {
	if err := w.db.Model(&Target{}).Where("stack_name = ?", stack.Name).
		Update("consecutive_failures", 0).Error; err != nil {
		return err
	}
	w.touchStack(stack, StackStatusNever, "", "")
	return triggerStackNow(w.db, stack.Name)
}

// maxStackErrorBytes caps the summary stored on the stack. The per-project errors
// are kept in full on each project's own target; this field is a pointer to them.
const maxStackErrorBytes = 2000

func truncateError(s string) string {
	if len(s) <= maxStackErrorBytes {
		return s
	}
	return s[:maxStackErrorBytes] + "…"
}
