package watcher

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrTargetNotFound is returned by store lookups when no target matches.
var ErrTargetNotFound = errors.New("target not found")

// ErrTargetNameConflict is returned by createTarget when Name is already in use.
var ErrTargetNameConflict = errors.New("target name already in use")

// ErrComponentDuplicate is returned when another target already deploys the
// same component from the same repository, in a different namespace. That is
// two independent copies of one app — deliberate for staging next to
// production, and a mistake when a project was moved into a stack without
// removing the standalone target it used to have. Nothing in the data
// distinguishes the two, so the deliberate case says so with AllowDuplicate
// and the mistake is stopped here rather than discovered days later.
var ErrComponentDuplicate = errors.New("the same component is already deployed from this repository")

// ErrUploadInStack is returned when a stack project would be an upload
// target, which v1 does not support.
var ErrUploadInStack = errors.New("a stack project cannot use deploy_mode \"upload\"")

// ErrComponentClaimed is returned when another target already deploys to the
// same component.
var ErrComponentClaimed = errors.New("another target already deploys to that component")

// checkComponentUnclaimed refuses a second target aimed at a component some
// other target already owns.
//
// Two targets on one component do not share it — they fight over it. Each
// build upserts the same component, so whichever built last wins, and the
// deployment silently flips between two repositories with nothing in either
// one's status to explain it. The identity of a deployment is (namespace,
// component name); nothing else in the system is a stable handle on it, which
// is exactly why this pair has to stay unique.
//
// Namespace comes from the deploy spec, so two stacks that each contain an
// "api" project do not collide.
func checkComponentUnclaimed(db *gorm.DB, t *Target) error {
	var siblings []Target
	if err := db.Where("component_name = ?", t.ComponentName).Find(&siblings).Error; err != nil {
		return fmt.Errorf("check component ownership: %w", err)
	}
	want := namespaceOf(t)
	for _, s := range siblings {
		if s.ID == t.ID || namespaceOf(&s) != want {
			continue
		}
		qualified := t.ComponentName
		if want != "" {
			qualified = want + "/" + t.ComponentName
		}
		return fmt.Errorf("%w: target %q already deploys to %s", ErrComponentClaimed, s.Name, qualified)
	}
	return nil
}

// checkComponentDuplicate catches the same component being deployed twice from
// one repository under two namespaces. checkComponentUnclaimed already covers a
// collision inside one namespace; this is the cross-namespace case, which the
// name and namespace checks both let through because neither actually collides.
//
// The signal is (repo, component name). A monorepo producing several components
// names them differently and is unaffected; two deployments of one app do not,
// which is exactly what makes them indistinguishable from an accident.
func checkComponentDuplicate(db *gorm.DB, t *Target) error {
	if t.AllowDuplicate || t.RepoURL == "" || t.DeployMode == DeployModeImage {
		return nil
	}
	var siblings []Target
	if err := db.Where("component_name = ? AND repo_url = ?", t.ComponentName, t.RepoURL).
		Find(&siblings).Error; err != nil {
		return fmt.Errorf("check duplicate deployment: %w", err)
	}
	want := namespaceOf(t)
	for _, s := range siblings {
		if s.ID == t.ID {
			continue
		}
		other := namespaceOf(&s)
		if other == want {
			// Same namespace is checkComponentUnclaimed's error to report, and
			// it says something more specific.
			continue
		}
		return fmt.Errorf("%w: target %q already deploys it as %s. "+
			"Two targets on one repository build and deploy independently of each other. "+
			"To move it here, remove the old one first (qooq watch rm %s). "+
			"To run both on purpose, pass --allow-duplicate",
			ErrComponentDuplicate, s.Name, qualifiedName(other, s.ComponentName), s.Name)
	}
	return nil
}

// qualifiedName renders a component the way the CLI addresses it.
func qualifiedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// namespaceOf reports which namespace a target's component lands in. A
// deploy spec that will not parse is reported as the empty namespace: the
// pipeline surfaces that failure itself, and guessing here would only turn one
// clear error into two.
func namespaceOf(t *Target) string {
	spec, err := parseDeploySpec(t.DeploySpec)
	if err != nil {
		return ""
	}
	return spec.Namespace
}

func createTarget(db *gorm.DB, t *Target) error {
	if t.DeployMode == "" {
		t.DeployMode = DeployModeSource
	}
	if t.AuthType == "" {
		t.AuthType = AuthTypeNone
	}
	// The source-pipeline defaults (branch/dockerfile/manifest path) are
	// meaningless for an "image" mode target (api.go's handleCreateTarget
	// rejects them being set at all). Note: because those columns also carry
	// a gorm `default:` tag, gorm's Create still fills them in at the SQL
	// level for a zero-value field regardless of this guard (it omits
	// zero-value defaulted columns from the INSERT, letting sqlite's column
	// default apply) — this guard is defense-in-depth for any future direct
	// DB write path, not a way to suppress that cosmetic detail today.
	// An upload target cannot belong to a stack: a stack's projects are all
	// cloned from git (stack_sync.go), and an upload project would need a
	// second way for the stack to fetch it. Refused here, below every caller,
	// so no path into createTarget can make one.
	if t.DeployMode == DeployModeUpload && t.StackName != "" {
		return fmt.Errorf("%w: stack %q project %q", ErrUploadInStack, t.StackName, t.ProjectName)
	}
	if t.DeployMode != DeployModeImage {
		if t.Branch == "" && t.DeployMode != DeployModeUpload {
			t.Branch = "main"
		}
		if t.Dockerfile == "" {
			t.Dockerfile = "Dockerfile"
		}
		if t.ManifestPath == "" {
			t.ManifestPath = "qooq.yaml"
		}
	}
	t.PollIntervalSeconds = normalizePollInterval(t.PollIntervalSeconds)
	if t.LastBuildStatus == "" {
		t.LastBuildStatus = BuildStatusNever
	}

	if err := checkComponentUnclaimed(db, t); err != nil {
		return err
	}
	if err := checkComponentDuplicate(db, t); err != nil {
		return err
	}
	// NextCheckAt left at zero value so the first sweep picks it up
	// immediately.

	err := db.Create(t).Error
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrTargetNameConflict
		}
		// gorm's sqlite driver surfaces UNIQUE constraint failures as a
		// plain *sqlite3 error rather than gorm.ErrDuplicatedKey unless
		// TranslateError is on (it is, per sqlite_connector's gorm.Config,
		// but double-check via the unique-index probe below as a fallback).
		var existing Target
		if lookupErr := db.Where("name = ?", t.Name).First(&existing).Error; lookupErr == nil {
			return ErrTargetNameConflict
		}
		return err
	}
	if t.DeployMode == DeployModeUpload && t.Branch != "" {
		// The column's `default:main` fills an empty string on insert, and an
		// upload has no branch: left alone, every upload target says "main".
		if err := db.Model(t).Update("branch", "").Error; err != nil {
			return err
		}
		t.Branch = ""
	}
	return nil
}

func getTargetByName(db *gorm.DB, name string) (*Target, error) {
	var t Target
	err := db.Where("name = ?", name).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTargetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func listTargets(db *gorm.DB) ([]Target, error) {
	var ts []Target
	if err := db.Order("name").Find(&ts).Error; err != nil {
		return nil, err
	}
	return ts, nil
}

func deleteTargetByName(db *gorm.DB, name string) error {
	res := db.Where("name = ?", name).Delete(&Target{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTargetNotFound
	}
	return nil
}

// triggerNow sets NextCheckAt to the zero-ish "now" so the next sweep tick
// picks this target up immediately, regardless of its poll interval.
//
// For a stack-owned target it also pokes the stack. Poking only the target would
// be a silent no-op — dueTargets skips stack-owned rows — and "I pushed, and
// the webhook fired, and nothing happened" is the least debuggable outcome
// available. The stack deploys every project in order, this one included.
func triggerNow(db *gorm.DB, name string) error {
	t, err := getTargetByName(db, name)
	if err != nil {
		return err
	}
	now := time.Now()
	// Triggering is also the resume for a target paused after repeated build
	// failures (buildgiveup.go). "Check this now" is exactly what somebody does
	// after fixing the repository, so it needs no second verb — and a paused
	// target that ignored a trigger would look broken in a new way.
	updates := map[string]any{"next_check_at": now}
	if t.PausedReason != "" {
		updates["paused_reason"] = ""
		updates["consecutive_failures"] = 0
	}
	if err := db.Model(&Target{}).Where("id = ?", t.ID).Updates(updates).Error; err != nil {
		return err
	}
	if t.StackName != "" {
		return db.Model(&Stack{}).Where("name = ?", t.StackName).Update("next_check_at", now).Error
	}
	return nil
}

// dueTargets returns every standalone target whose NextCheckAt has arrived
// (including the sqlite zero-value default, so brand-new targets get an
// immediate first check).
//
// Stack-owned targets are excluded: their stack sweeps them in the order its
// stack manifest declares (see deployStack). Sweeping them here as well would deploy a
// project before the projects it waits for, which is precisely what depends_on: exists to
// prevent. The IS NULL arm covers rows written before the column existed,
// whose value AutoMigrate cannot backfill on every driver.
// owner is this watcher's instance id, so a target another *live* watcher has
// claimed is left out of the batch entirely rather than queued and then
// skipped — with several watchers that would have every one of them spending a
// build worker per tick to discover work it cannot do. An expired lease is not
// excluded: taking over from a watcher that died mid-build is the point of
// having a lease at all. Pass "" when there is no claimant to compare against
// (tests, and any caller that only wants the schedule's own answer).
func dueTargets(db *gorm.DB, now time.Time, owner string) ([]Target, error) {
	var ts []Target
	// paused_reason excludes a target that has failed too many times in a row
	// (buildgiveup.go). Excluded here rather than checked inside the build
	// because the point is to stop occupying a build worker at all — a check
	// after the worker is taken would still pay for the clone.
	// deploy_paused is the operator's own pause, and force_rebuild is how an
	// explicit trigger says "I mean it": pausing automatic deploys must not
	// swallow a deliberate one, or the deploy button would silently do nothing
	// on exactly the projects somebody is most likely to be fixing.
	q := db.Where("next_check_at <= ? AND (stack_name IS NULL OR stack_name = '') AND (paused_reason IS NULL OR paused_reason = '')", now)
	q = q.Where("deploy_paused IS NULL OR deploy_paused = 0 OR force_rebuild = 1")
	// A quota hold refuses even a forced deploy: the account is over its
	// limit, and "I mean it" does not change that.
	q = q.Where("quota_hold IS NULL OR quota_hold = ''")
	q = q.Where("claimed_by = '' OR claimed_by IS NULL OR claimed_by = ? OR claimed_until IS NULL OR claimed_until < ?", owner, now)
	// Longest waiting first. The sweep offers targets in this order and, under
	// a per-namespace cap, the first takes the slot; a target refused admission
	// keeps its next_check_at, so it is the oldest next time. In no particular
	// order, targets polled often were due again every tick and listed first,
	// and the rest of their namespace never built (found by make smoke-multi).
	q = q.Order("next_check_at").Order("id")
	if err := q.Find(&ts).Error; err != nil {
		return nil, err
	}
	return ts, nil
}

// rescheduleTarget pushes a target's NextCheckAt to now + its poll interval,
// but only if NextCheckAt still equals expected — the value the sweep loop read
// before it began processing this target. This is an optimistic (compare-and-
// swap) update: if a webhook/trigger fired triggerNow mid-build and moved
// NextCheckAt to "now", the WHERE no longer matches, zero rows are affected,
// and the poke is preserved (the next sweep tick picks it up) instead of being
// clobbered by now+interval. It returns whether the reschedule was applied
// (false = a concurrent poke won, leave NextCheckAt as-is). Correctness rests
// on the DB row's atomicity, not on the sweep and webhook goroutines' ordering.
func rescheduleTarget(db *gorm.DB, id uint, interval time.Duration, expected time.Time) (bool, error) {
	res := db.Model(&Target{}).
		Where("id = ? AND next_check_at = ?", id, expected).
		Update("next_check_at", time.Now().Add(interval))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
