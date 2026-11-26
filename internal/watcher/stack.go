package watcher

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Stack is one deployed instance of a manifest: a group of projects
// deployed together, in an order they declare, into a namespace and network
// of their own.
//
// The manifest (qooq.stack.yaml) lives in a git repository like any other qooq
// input, so changing how the stack is composed is a commit, like changing how
// one of its projects is built. Each project becomes an ordinary Target owned by this
// stack (Target.StackName) — the stack layer decides *when* each project is built,
// the existing pipeline still decides *how*.
type Stack struct {
	ID uint `gorm:"primaryKey" json:"-"`
	// Name is the stack's identity: it is also the namespace its components
	// live in and (via defaultAppNetwork) the network they share, so it must
	// be a DNS label. parseStackManifest enforces the same rule on the manifest's
	// own stack: field.
	Name string `gorm:"uniqueIndex;not null" json:"name"`

	// Where the manifest comes from. A project with path: is cloned from this
	// same repository, so these credentials serve both.
	RepoURL        string `gorm:"not null" json:"repo_url"`
	Branch         string `gorm:"not null;default:main" json:"branch"`
	AuthType       string `gorm:"not null;default:none" json:"auth_type"`
	AuthCredential string `json:"-"`

	ManifestPath string `gorm:"not null;default:qooq.stack.yaml" json:"manifest_path"`

	// AllowDuplicate is passed to every project target this stack creates, so
	// a stack can be stood up beside an existing standalone deployment of the
	// same repositories on purpose. Left false, checkComponentDuplicate stops
	// the far more common version of that shape: a project moved into a stack
	// whose old target was never removed, which otherwise deploys twice in
	// silence.
	AllowDuplicate bool `gorm:"not null;default:false" json:"allow_duplicate"`

	// Manifest is the last manifest text that parsed successfully, kept so a
	// sweep can walk the DAG without cloning when the repository has not moved.
	// LastManifestCommit is the commit it came from.
	Manifest           string `json:"-"`
	LastManifestCommit string `json:"last_manifest_commit,omitempty"`

	PollIntervalSeconds int       `gorm:"not null;default:60" json:"poll_interval_seconds"`
	NextCheckAt         time.Time `json:"-"`

	Status     string    `gorm:"not null;default:never" json:"status"`
	LastError  string    `json:"last_error,omitempty"`
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`

	// PausedReason explains a Status of paused: which project exhausted the
	// manifest's max_failures. Cleared by resume.
	PausedReason string `json:"paused_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Stack status values.
//
// degraded is the one that matters: it means some projects are running the new
// version and others are still on the old one. That state is inherent to
// converging (non-transactional) deployment — so
// it gets a name rather than being folded into "failed".
const (
	StackStatusNever    = "never"
	StackStatusSuccess  = "success"
	StackStatusDegraded = "degraded"
	StackStatusFailed   = "failed"
	StackStatusPaused   = "paused"
)

var (
	// ErrStackNotFound is returned by store lookups when no stack matches.
	ErrStackNotFound = errors.New("stack not found")
	// ErrStackNameConflict is returned by createStack when Name is taken.
	ErrStackNameConflict = errors.New("stack name already in use")
)

// projectTargetName is the name of the Target backing one project. It carries the
// stack name so two stacks made from the same manifest don't collide, using
// the same separator as container names (see internal/core's ContainerName).
func projectTargetName(stack, project string) string { return stack + "_" + project }

// sourceTarget presents the stack's own repository as a Target, so the git
// helpers (auth, host key policy, clone, credential masking) apply to fetching
// a manifest exactly as they do to fetching a project.
func (stack *Stack) sourceTarget() *Target {
	return &Target{
		Name:           stack.Name,
		DeployMode:     DeployModeSource,
		RepoURL:        stack.RepoURL,
		Branch:         stack.Branch,
		AuthType:       stack.AuthType,
		AuthCredential: stack.AuthCredential,
	}
}

// network is the container network every project of this stack joins. One stack,
// one network — which is what lets a project's manifest address its siblings by
// their short names.
//
// Built from the stack name rather than through defaultAppNetwork because a
// stack's name IS its namespace: there is no component to scope, and the
// namespace is what makes the name unique. On a shared node the namespace already
// carries the account (see the external manager's QualifiedNamespace), so this is
// isolated without any further qualification.
func (stack *Stack) network() string { return stackNetwork(stack.Name) }

func createStack(db *gorm.DB, stack *Stack) error {
	if stack.Branch == "" {
		stack.Branch = "main"
	}
	if stack.AuthType == "" {
		stack.AuthType = AuthTypeNone
	}
	if stack.ManifestPath == "" {
		stack.ManifestPath = defaultStackManifestPath
	}
	stack.PollIntervalSeconds = normalizePollInterval(stack.PollIntervalSeconds)
	if stack.Status == "" {
		stack.Status = StackStatusNever
	}
	// NextCheckAt left at zero so the first sweep picks it up immediately.

	if err := db.Create(stack).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrStackNameConflict
		}
		var existing Stack
		if lookupErr := db.Where("name = ?", stack.Name).First(&existing).Error; lookupErr == nil {
			return ErrStackNameConflict
		}
		return err
	}
	return nil
}

func getStackByName(db *gorm.DB, name string) (*Stack, error) {
	var stack Stack
	err := db.Where("name = ?", name).First(&stack).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrStackNotFound
	}
	if err != nil {
		return nil, err
	}
	return &stack, nil
}

func listStacks(db *gorm.DB) ([]Stack, error) {
	var ds []Stack
	if err := db.Order("name").Find(&ds).Error; err != nil {
		return nil, err
	}
	return ds, nil
}

func deleteStackByName(db *gorm.DB, name string) error {
	res := db.Where("name = ?", name).Delete(&Stack{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStackNotFound
	}
	return nil
}

// dueStacks returns every stack whose NextCheckAt has arrived, including the
// zero value so a brand-new stack is deployed on the next tick.
func dueStacks(db *gorm.DB, now time.Time) ([]Stack, error) {
	var ds []Stack
	if err := db.Where("next_check_at <= ?", now).Find(&ds).Error; err != nil {
		return nil, err
	}
	return ds, nil
}

// triggerStackNow makes the next sweep tick deploy this stack regardless of its
// poll interval.
func triggerStackNow(db *gorm.DB, name string) error {
	res := db.Model(&Stack{}).Where("name = ?", name).Update("next_check_at", time.Now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStackNotFound
	}
	return nil
}

// rescheduleStack is the stack counterpart of rescheduleTarget: a compare-and-
// swap on NextCheckAt so a trigger that arrived mid-deploy is not overwritten by
// now+interval.
func rescheduleStack(db *gorm.DB, id uint, interval time.Duration, expected time.Time) (bool, error) {
	res := db.Model(&Stack{}).
		Where("id = ? AND next_check_at = ?", id, expected).
		Update("next_check_at", time.Now().Add(interval))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// projectTargets returns every target owned by this stack, keyed by project name.
func projectTargets(db *gorm.DB, stack string) (map[string]*Target, error) {
	var ts []Target
	if err := db.Where("stack_name = ?", stack).Order("project_name").Find(&ts).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*Target, len(ts))
	for i := range ts {
		out[ts[i].ProjectName] = &ts[i]
	}
	return out, nil
}

// toResponse converts a Stack into its API view. The credential is masked
// rather than omitted, so a caller can tell "configured" from "not".
func (stack *Stack) toResponse() StackResponse {
	resp := StackResponse{
		Name:                stack.Name,
		RepoURL:             stack.RepoURL,
		Branch:              stack.Branch,
		AuthType:            stack.AuthType,
		ManifestPath:        stack.ManifestPath,
		LastManifestCommit:  stack.LastManifestCommit,
		PollIntervalSeconds: stack.PollIntervalSeconds,
		Status:              stack.Status,
		LastError:           stack.LastError,
		PausedReason:        stack.PausedReason,
		CreatedAt:           stack.CreatedAt,
		UpdatedAt:           stack.UpdatedAt,
	}
	if stack.AuthCredential != "" {
		resp.AuthCredential = maskedSecret
	}
	if !stack.LastSyncAt.IsZero() {
		resp.LastSyncAt = &stack.LastSyncAt
	}
	if !stack.NextCheckAt.IsZero() {
		resp.NextCheckAt = &stack.NextCheckAt
	}
	return resp
}
