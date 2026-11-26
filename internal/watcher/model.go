package watcher

import (
	"encoding/json"
	"strings"
	"time"
)

// Poll interval bounds (seconds). defaultPollIntervalSeconds applies when a
// target requests 0 (unset). minPollIntervalSeconds is the floor the API
// enforces: anything below it is pointless because the sweep loop only ticks
// every sweepInterval (10s), so a sub-floor interval just means "due at every
// tick" with no extra freshness — while needlessly hammering the git remote.
const (
	defaultPollIntervalSeconds = 60
	minPollIntervalSeconds     = 10
)

// normalizePollInterval maps a requested poll interval to the stored value:
// 0 (unset) becomes the default, and anything below the floor is raised to it.
// This is the safety net for write paths that skip the API's up-front rejection
// (e.g. a record written straight to the DB); the API still returns a clear
// error for an explicit sub-floor value so operators get feedback.
func normalizePollInterval(seconds int) int {
	if seconds <= 0 {
		return defaultPollIntervalSeconds
	}
	if seconds < minPollIntervalSeconds {
		return minPollIntervalSeconds
	}
	return seconds
}

// Target is a GitOps watch target: a git repository/branch that, when a new
// commit lands, should be built (Dockerfile at ContextSubdir/Dockerfile) and
// upserted as a qooqd component named ComponentName.
//
// Credential v1 stance (deliberately simple):
// AuthCredential and WebhookSecret are stored in plaintext in sqlite. The DB
// file is chmod'd 0600 on module start and a one-time warning is logged.
// AES-GCM encryption behind QOOQ_WATCHER_SECRET_KEY is a documented follow-up,
// not implemented here.
type Target struct {
	ID   uint   `gorm:"primaryKey" json:"-"`
	Name string `gorm:"uniqueIndex;not null" json:"name"`

	// DeployMode is "source" (default: clone+build from RepoURL, M12/M14/M16
	// pipeline) or "image" (M20a: no clone/build — poll a registry ref for
	// digest changes and deploy the image directly via qooqd). See api.go's
	// handleCreateTarget for the field cross-validation between the two modes.
	DeployMode string `gorm:"not null;default:source" json:"deploy_mode"`

	RepoURL string `gorm:"not null" json:"repo_url"`
	Branch  string `gorm:"not null;default:main" json:"branch"`

	// ImageRef (M20a) is the registry ref polled for digest changes when
	// DeployMode is "image" (e.g. "ghcr.io/acme/api:latest"). Unused (must be
	// empty) in "source" mode.
	ImageRef string `json:"image_ref,omitempty"`
	// LastImageDigest (M20a) is the registry digest (sha256:...) of ImageRef
	// as of the last successful check/deploy. Empty until the first
	// successful digest resolution.
	LastImageDigest string `json:"last_image_digest,omitempty"`

	// AuthType is "none", "token" or "ssh-key". Unused in "image" DeployMode.
	AuthType string `gorm:"not null;default:none" json:"auth_type"`
	// AuthCredential holds the token or the PEM-encoded SSH private key, for
	// "source" mode (depending on AuthType); for "image" mode it instead
	// holds an optional registry credential in "username:password" form (no
	// colon = username only, empty password). Never returned by the API in
	// plaintext.
	AuthCredential string `json:"-"`

	Dockerfile    string `gorm:"not null;default:Dockerfile" json:"dockerfile"`
	ContextSubdir string `json:"context_subdir"`

	// ManifestPath is the repo-relative path to the M14 app manifest
	// (qooq.yaml). Empty/omitted defaults to "qooq.yaml"; the sentinel value
	// "-" disables manifest parsing entirely for this target (an escape
	// hatch for repos that don't want it), behaving exactly like M12.
	ManifestPath string `gorm:"not null;default:qooq.yaml" json:"manifest_path"`

	ComponentName string `gorm:"not null" json:"component_name"`
	// AllowDuplicate lets this target deploy a component another target
	// already deploys from the same repository, under a different namespace —
	// staging beside production. Without it checkComponentDuplicate refuses,
	// because the same shape is what a project moved into a stack looks like
	// when its old standalone target was left behind.
	AllowDuplicate bool `gorm:"not null;default:false" json:"allow_duplicate"`
	// PublishDetectedPorts makes a manifest generated for a repo without one
	// publish its ports ("auto:<port>"). Off by default: see
	// renderGeneratedManifest.
	PublishDetectedPorts bool `gorm:"not null;default:false" json:"publish_detected_ports"`
	// BuildGroup is what the concurrent-build cap counts by (see
	// watcherclient.CreateTargetRequest.BuildGroup); empty means the namespace.
	BuildGroup string `gorm:"not null;default:''" json:"build_group,omitempty"`
	// QuotaHold, when non-empty, is why this target may not deploy: its
	// account is over a plan limit (the external manager sets and clears it).
	// Separate from DeployPaused, which is a person's choice and is lifted by
	// a person; this one lifts itself when the account is back under.
	// Unlike DeployPaused it also refuses explicit deploys.
	QuotaHold string `gorm:"not null;default:''" json:"quota_hold,omitempty"`
	// DeploySpec is a JSON blob matching client.DeployRequest minus
	// Name/Image (those are filled in by the pipeline from ComponentName and
	// the freshly built tag). Stored as text so the schema doesn't need to
	// track every field client.DeployRequest ever grows.
	DeploySpec string `json:"deploy_spec"`

	PollIntervalSeconds int `gorm:"not null;default:60" json:"poll_interval_seconds"`

	// ForceRebuild makes the next sweep run the pipeline even though the
	// remote HEAD has not moved. Deploy inputs can change without a commit —
	// editing the generated manifest is the case that made this necessary —
	// and without it a correction would sit unapplied until something
	// unrelated was pushed. Cleared by the build it caused.
	ForceRebuild bool `json:"-"`

	// GeneratedManifest holds the qooq.yaml the watcher generated for a repo
	// that has none (see detect.go). It is written once, on the first build
	// that finds no manifest, and read on every build after that — so an
	// operator who corrects a bad guess keeps their correction instead of
	// having it overwritten next sweep. Empty means "not generated yet", or
	// that the repo has a qooq.yaml of its own.
	GeneratedManifest string `json:"generated_manifest,omitempty"`

	// ServiceState (M23) holds per-service data that must survive redeploys,
	// as JSON keyed by service name — today just generated passwords. A
	// database bakes its password into its data directory on first start, so
	// regenerating it on the next deploy would lock the app out of its own
	// data. Encrypted at rest alongside the other credential fields when a
	// secret key is configured; never returned by the API.
	ServiceState string `json:"-"`

	// Topology is the JSON of watcherclient.Topology (without phases) as of
	// the last successful build: which services it started and which
	// components it referenced. Rewritten on every success so it never
	// describes a deploy older than the one running.
	Topology string `gorm:"not null;default:''" json:"-"`

	// WebhookSecret is reserved for M13 (webhook signature verification).
	// Never returned by the API in plaintext.
	WebhookSecret string `json:"-"`

	// DeclaredInputs is the inputs: block from the last manifest that parsed,
	// as JSON keyed by input name.
	//
	// It is stored rather than read on demand because of where the two halves
	// live: the declaration is in the repo, but whatever has to ask a human
	// for the values — the CLI, a web form — is outside it and cannot clone.
	// Caching what the last clone saw is what lets the API answer "this target
	// is missing STRIPE_KEY, here is what it's for". Same reasoning as
	// Stack.Manifest.
	DeclaredInputs string `gorm:"not null;default:''" json:"-"`

	// InputValues holds the operator-supplied values, as JSON keyed by input
	// name. Values for inputs declared secret are encrypted at rest exactly
	// like ServiceState, and the API masks them.
	InputValues string `gorm:"not null;default:''" json:"-"`

	// StackName and ProjectName link this target to the stack that owns it (see
	// stack.go). Both empty for an ordinary standalone target, which is every
	// target that existed before stacks.
	//
	// A stack-owned target is deliberately excluded from dueTargets: its stack
	// decides when it is built, because building it out of order would defeat
	// the whole point of the manifest depends_on:. Everything else about it — the
	// pipeline, the manifest, the API, `qooq watch get` — works exactly as it
	// does for any other target.
	StackName   string `gorm:"index;not null;default:''" json:"stack_name,omitempty"`
	ProjectName string `gorm:"not null;default:''" json:"project_name,omitempty"`

	// ConsecutiveFailures counts failures since the last success, for the
	// stack manifest policy.max_failures. It is reset by any successful deploy.
	// Only stacks act on it today; a standalone target simply retries forever,
	// which is the behaviour it has always had.
	ConsecutiveFailures int `gorm:"not null;default:0" json:"consecutive_failures,omitempty"`

	// ClaimedBy / ClaimedUntil are the cross-process half of targetLocks: a
	// lease naming the watcher that is working on this target right now. See
	// claim.go for why it is a lease in the row and not SELECT ... FOR UPDATE.
	// Both are json:"-" — which watcher holds a target is an operational
	// detail of the deployment, not part of the target an operator declared.
	ClaimedBy    string     `gorm:"index;not null;default:''" json:"-"`
	ClaimedUntil *time.Time `json:"-"`

	// PausedReason, when non-empty, means this target has failed too many times
	// in a row and is no longer being retried (see buildgiveup.go). It holds the
	// sentence shown to whoever has to fix it, including the command that
	// resumes — a paused target with no stated reason is indistinguishable from
	// one the sweep has simply forgotten.
	PausedReason string `gorm:"not null;default:''" json:"paused_reason,omitempty"`

	// DeployPaused is an operator's own "stop deploying this for now", and is a
	// different thing from PausedReason above.
	//
	// PausedReason is qooq giving up after repeated failures; this is somebody
	// deciding. Keeping them apart matters in both directions: a person who
	// paused a project should not be told it failed ten times, and a project
	// qooq gave up on must not look like a deliberate pause somebody will
	// remember to undo.
	//
	// It stops the POLLING only. An explicit trigger still builds — pausing
	// automatic deploys is not the same as refusing to deploy, and a pause that
	// swallowed a deliberate "deploy now" would look broken.
	DeployPaused bool `gorm:"not null;default:false" json:"deploy_paused,omitempty"`

	LastCommit string `json:"last_commit,omitempty"`

	// UploadRevision is the revision of the most recent source archive
	// uploaded to a deploy_mode "upload" target (see upload.go) — what the
	// sweep compares against LastCommit, the way a source target compares the
	// remote HEAD. It moves when the upload is stored, not when it builds, so
	// (UploadRevision != LastCommit) is "an upload is waiting to be built".
	// LastUploadAt is when that archive arrived. Both empty for other modes.
	UploadRevision string    `gorm:"not null;default:''" json:"upload_revision,omitempty"`
	LastUploadAt   time.Time `json:"last_upload_at,omitempty"`

	// RejectedCommit is a commit that built, was deployed, and did not start
	// (see newversion.go). Polling does not deploy it again: the previous
	// version has just been put back, and redeploying the same broken commit
	// every poll interval would take the service down once per poll. A new
	// commit or an explicit forced deploy clears the way.
	RejectedCommit string `json:"-"`

	LastBuildStatus string `gorm:"not null;default:never" json:"last_build_status"`
	LastBuildError  string `json:"last_build_error,omitempty"`

	// LastBuildLog is the output of the most recent build.
	//
	// Separate from LastBuildError, which is a one-line summary: this is the
	// whole tail — the compiler message, the failing command, the step that
	// broke. It is capped by the daemon that produced it (core's
	// maxBuildLogBytes), so nothing here has to bound it again.
	//
	// Deliberately NOT in TargetResponse: a list of twenty targets would carry
	// a megabyte of build output nobody asked for. It has its own endpoint.
	LastBuildLog string    `gorm:"type:text" json:"-"`
	LastBuildAt  time.Time `json:"last_build_at,omitempty"`

	// BuildStartedAt is when the current or most recent build BEGAN.
	//
	// Separate from LastBuildAt, which is when one finished. Without it there
	// is no way to say how long a build has been running: during a build
	// LastBuildAt still holds the previous build's finish time, so measuring
	// from it would report the idle time since then — hours, for a build that
	// started a minute ago, stated with complete confidence.
	BuildStartedAt time.Time `json:"build_started_at,omitempty"`

	// LastBuildRevision says which input the build status above is about: the
	// commit hash for a source target, the image digest for an image one.
	//
	// Without it the status cannot be trusted by anything automated. LastCommit
	// only moves on success, so between pushing a fix and that fix finishing,
	// a target reports the *previous* revision next to a status that is already
	// about the new one — and a script that polls until "the build for the
	// commit I pushed" is done reads the old failure and gives up. This field
	// is written when the build starts, so the pair (revision, status) always
	// describes the same attempt.
	LastBuildRevision string `gorm:"not null;default:''" json:"last_build_revision,omitempty"`

	// BuildGeneration counts builds started for this target, monotonically.
	//
	// The revision alone cannot answer "did a new build start?" when the answer
	// matters most: a force-rebuild, a retry after a transient failure, and a
	// re-push of the same commit all keep the revision the same. A counter that
	// only ever goes up gives a watcher of this API a single value to compare
	// against the one it saw last.
	BuildGeneration uint64 `gorm:"not null;default:0" json:"build_generation,omitempty"`

	NextCheckAt time.Time `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Build status values for LastBuildStatus.
const (
	BuildStatusNever    = "never"
	BuildStatusBuilding = "building"
	BuildStatusSuccess  = "success"
	BuildStatusFailed   = "failed"
	// BuildStatusNeedsInput means the manifest declares required inputs that
	// have no value yet. It is kept distinct from failed because it asks a
	// different question of whoever is looking: nothing is broken, something
	// is unanswered. The pipeline treats it as a failure for retry and stack
	// purposes — a stack blocked on a missing value should stop and say so
	// rather than rebuild forever — but the CLI can then tell the operator
	// what to supply instead of showing them an error.
	BuildStatusNeedsInput = "needs_input"
)

// Auth type values for AuthType.
const (
	AuthTypeNone   = "none"
	AuthTypeToken  = "token"
	AuthTypeSSHKey = "ssh-key"

	// AuthTypeGitHubApp carries no credential at all: the watcher asks its
	// external manager for a short-lived one per use (controlauth.go). Only usable
	// on a watcher that has a external manager, which is a managed deployment;
	// a stored token would not be an acceptable substitute there.
	AuthTypeGitHubApp = "github-app"
)

// DeployMode values (M20a).
const (
	DeployModeSource = "source"
	DeployModeImage  = "image"
	// DeployModeUpload builds from a source archive pushed to
	// PUT /v1/targets/{name}/source instead of a git clone. There is no
	// remote to poll: only an upload, a forced trigger, or the component
	// having disappeared causes a build (upload.go).
	DeployModeUpload = "upload"
)

const maskedSecret = "***"

// toResponse converts a Target into its API-safe wire representation:
// AuthCredential and WebhookSecret are masked rather than omitted, so
// callers can tell "configured" from "not configured" without ever seeing
// the value.
func (t *Target) toResponse() TargetResponse {
	resp := TargetResponse{
		Topology:            t.topologyForResponse(),
		Name:                t.Name,
		DeployMode:          t.DeployMode,
		RepoURL:             t.RepoURL,
		Branch:              t.Branch,
		AuthType:            t.AuthType,
		Dockerfile:          t.Dockerfile,
		ContextSubdir:       t.ContextSubdir,
		ManifestPath:        t.ManifestPath,
		ComponentName:       t.ComponentName,
		Namespace:           namespaceOf(t),
		DeploySpec:          json.RawMessage(t.DeploySpec),
		PollIntervalSeconds: t.PollIntervalSeconds,
		ImageRef:            t.ImageRef,
		LastImageDigest:     t.LastImageDigest,
		LastCommit:          t.LastCommit,
		UploadRevision:      t.UploadRevision,
		LastBuildStatus:     t.LastBuildStatus,
		LastBuildError:      t.LastBuildError,
		LastBuildRevision:   t.LastBuildRevision,
		BuildGeneration:     t.BuildGeneration,
		PausedReason:        t.PausedReason,
		// How close it is to being paused — the one number the pause is
		// decided on, and it never reached the wire before.
		ConsecutiveFailures: t.ConsecutiveFailures,
		DeployPaused:        t.DeployPaused,
		QuotaHold:           t.QuotaHold,
		StackName:           t.StackName,
		ProjectName:         t.ProjectName,
		CreatedAt:           t.CreatedAt,
		UpdatedAt:           t.UpdatedAt,
	}
	if t.AuthCredential != "" {
		resp.AuthCredential = maskedSecret
	}
	if t.WebhookSecret != "" {
		resp.WebhookSecret = maskedSecret
	}
	// Inputs: what this deployment needs and what it still lacks. Decode
	// failures are reported as "no inputs" rather than failing the whole
	// response — a corrupt cache should not make a target unlistable, and the
	// pipeline reports the corruption where it can act on it.
	if declared, err := decodeDeclaredInputs(t.DeclaredInputs); err == nil {
		var stored map[string]string
		if strings.TrimSpace(t.InputValues) != "" {
			_ = json.Unmarshal([]byte(t.InputValues), &stored)
		}
		resp.Inputs = inputResponses(declared, stored)
		resp.MissingInputs = missingInputs(declared, storedPresence(stored))
	}
	if !t.BuildStartedAt.IsZero() {
		resp.BuildStartedAt = &t.BuildStartedAt
	}
	if !t.LastBuildAt.IsZero() {
		resp.LastBuildAt = &t.LastBuildAt
	}
	if !t.NextCheckAt.IsZero() {
		resp.NextCheckAt = &t.NextCheckAt
	}
	if !t.LastUploadAt.IsZero() {
		resp.LastUploadAt = &t.LastUploadAt
	}
	return resp
}
