// Package watcherclient is the Go SDK + wire DTOs for qooq-watcher's REST API
// (targets CRUD + trigger; webhook signature verification lives entirely
// server-side, see internal/watcher/webhook.go). The types here are the
// single source of truth for the JSON contract: internal/watcher type-aliases
// them rather than defining its own copies, and cmd/qooq's `qooq watch`
// subcommand imports this package directly — never internal/watcher, which
// would drag gin/fx/gorm/go-git into the qooqd CLI (see the dependency
// wall in docs/ARCHITECTURE.md).
package watcherclient

import (
	"encoding/json"
	"time"
)

// CreateTargetRequest is the body of POST /v1/targets. DeploySpec is a JSON
// object (fields matching qooqd's client.DeployRequest minus name/image),
// kept raw here and stored as text by the watcher.
type CreateTargetRequest struct {
	Name string `json:"name"`
	// DeployMode is "source" (default, omit to keep the pre-M20a behavior),
	// "image" (M20a: poll a registry ref for digest changes instead of
	// cloning/building from a repo), or "upload" (build from an archive sent
	// to PUT /v1/targets/{name}/source — no repo_url, no branch). See TargetResponse's doc comment and
	// qooq-watcher's handleCreateTarget for the field cross-validation
	// between the two modes.
	DeployMode     string `json:"deploy_mode,omitempty"`
	RepoURL        string `json:"repo_url,omitempty"`
	Branch         string `json:"branch,omitempty"`
	AuthType       string `json:"auth_type,omitempty"`
	AuthCredential string `json:"auth_credential,omitempty"`
	Dockerfile     string `json:"dockerfile,omitempty"`
	ContextSubdir  string `json:"context_subdir,omitempty"`
	// ManifestPath is the repo-relative path to the M14 app manifest
	// (qooq.yaml); empty defaults to "qooq.yaml", "-" disables manifest
	// parsing for this target. Only meaningful in "source" mode.
	ManifestPath  string `json:"manifest_path,omitempty"`
	ComponentName string `json:"component_name"`
	// AllowDuplicate permits deploying a component another target already
	// deploys from the same repository, in a different namespace (staging
	// beside production). Without it the watcher refuses, because the same
	// shape is what a stack migration looks like when the old standalone
	// target was never removed.
	AllowDuplicate bool `json:"allow_duplicate,omitempty"`
	// PublishDetectedPorts publishes the ports of a manifest the watcher
	// generates for a repo that has none. Without it those ports stay inside
	// the network and the service has no address.
	PublishDetectedPorts bool `json:"publish_detected_ports,omitempty"`
	// BuildGroup is an opaque key the concurrent-build cap
	// (sweep.builds_per_namespace) counts by instead of the namespace. The
	// external manager sets it to the account, because an account has several
	// namespaces and capping each of them separately let one account fill
	// every build worker. Empty: counted by namespace, as before.
	BuildGroup string `json:"build_group,omitempty"`
	// ImageRef (M20a) is the registry ref polled for digest changes; required
	// when DeployMode is "image", must be empty in "source" mode.
	ImageRef            string          `json:"image_ref,omitempty"`
	DeploySpec          json.RawMessage `json:"deploy_spec,omitempty"`
	PollIntervalSeconds int             `json:"poll_interval_seconds,omitempty"`
	WebhookSecret       string          `json:"webhook_secret,omitempty"`
	// Inputs supplies values for the manifest's inputs: block up front, so a
	// target whose manifest requires them can deploy on its first sweep
	// instead of stopping to ask. Values for inputs the manifest marks secret
	// are encrypted at rest.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// TargetResponse is the API view of a Target. AuthCredential and
// WebhookSecret are masked to "***" when set, never returned in plaintext.
type TargetResponse struct {
	Name           string `json:"name"`
	DeployMode     string `json:"deploy_mode"`
	RepoURL        string `json:"repo_url"`
	Branch         string `json:"branch"`
	AuthType       string `json:"auth_type"`
	AuthCredential string `json:"auth_credential,omitempty"`
	Dockerfile     string `json:"dockerfile"`
	ContextSubdir  string `json:"context_subdir,omitempty"`
	ManifestPath   string `json:"manifest_path"`
	ComponentName  string `json:"component_name"`
	// Namespace scopes ComponentName. Derived from DeploySpec rather than
	// stored, because the component's identity is (namespace, name) and the
	// deploy spec is where that namespace is decided.
	Namespace           string          `json:"namespace,omitempty"`
	ImageRef            string          `json:"image_ref,omitempty"`
	DeploySpec          json.RawMessage `json:"deploy_spec,omitempty"`
	PollIntervalSeconds int             `json:"poll_interval_seconds"`
	WebhookSecret       string          `json:"webhook_secret,omitempty"`
	LastCommit          string          `json:"last_commit,omitempty"`
	// UploadRevision / LastUploadAt describe the most recent archive sent to
	// a deploy_mode "upload" target: its content revision (sha256 over the
	// normalized file list) and when it arrived. UploadRevision differing from
	// LastCommit means that upload has not been deployed (yet, or it failed).
	UploadRevision  string     `json:"upload_revision,omitempty"`
	LastUploadAt    *time.Time `json:"last_upload_at,omitempty"`
	LastImageDigest string     `json:"last_image_digest,omitempty"`
	LastBuildStatus string     `json:"last_build_status"`
	LastBuildError  string     `json:"last_build_error,omitempty"`
	// LastBuildRevision is what LastBuildStatus is about: a commit hash for a
	// source target, an image digest for an image one. It is set when a build
	// starts, so status and revision always describe the same attempt —
	// LastCommit only moves on success and so lags a build in flight.
	LastBuildRevision string `json:"last_build_revision,omitempty"`
	// BuildGeneration increases by one every time a build starts, so a caller
	// can tell "a new build began" apart from "the same one is still running"
	// even when the revision is unchanged (a retry, or a forced rebuild).
	BuildGeneration uint64 `json:"build_generation,omitempty"`
	// PausedReason, when set, means this target stopped being retried after too
	// many consecutive build failures, and says what to do about it. Surfaced
	// because a target that is silently no longer building is the failure this
	// mechanism would otherwise introduce.
	PausedReason string `json:"paused_reason,omitempty"`
	// ConsecutiveFailures is how many builds in a row have failed and counted
	// against the repository; at 10 the target is paused (PausedReason).
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// DeployPaused is an operator's own pause, as opposed to PausedReason's
	// "qooq gave up". A caller has to tell them apart to say anything true
	// about why nothing is being deployed.
	DeployPaused bool `json:"deploy_paused,omitempty"`
	// QuotaHold is why the target may not deploy because its account is over
	// a plan limit; empty when it may. Set and cleared by the external manager.
	QuotaHold   string     `json:"quota_hold,omitempty"`
	LastBuildAt *time.Time `json:"last_build_at,omitempty"`
	// BuildStartedAt is when the current or most recent build began. Absent on
	// a target that has not built since this field existed, which is why a
	// caller must treat nil as "unknown" rather than as a zero duration.
	BuildStartedAt *time.Time `json:"build_started_at,omitempty"`
	NextCheckAt    *time.Time `json:"next_check_at,omitempty"`
	// StackName/ProjectName are set on a target a stack owns (see the stack API):
	// its build order is decided by that stack's manifest rather than by its own
	// poll interval.
	StackName   string `json:"stack_name,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	// Inputs describes the values this deployment needs that its repo cannot
	// know, as declared by the last manifest that parsed. Reported by the API
	// because whatever has to ask a human for them — the CLI, a web form —
	// cannot clone the repo to read the declaration itself.
	Inputs []InputResponse `json:"inputs,omitempty"`
	// MissingInputs names the required inputs with no value yet, in the order
	// they should be asked for. Non-empty means last_build_status is
	// needs_input and nothing was built.
	MissingInputs []string `json:"missing_inputs,omitempty"`

	// The deployed component's state, read from the daemon that is running it.
	//
	// Separate from the build fields above, and that separation is the point: a
	// build can succeed and leave a container crash-looping, and reporting only
	// last_build_status would call that a success. These say what is actually
	// running.
	//
	// All three are best-effort. A daemon that cannot be reached leaves them
	// empty rather than failing the listing — the build state is still true and
	// still worth showing.
	Endpoints      []string `json:"endpoints,omitempty"`
	ComponentPhase string   `json:"component_phase,omitempty"`
	ComponentError string   `json:"component_error,omitempty"`
	// ComponentDesiredState is "Running" or "Stopped": what the daemon has been
	// told to keep this at. Without it a container somebody stopped on purpose
	// and one that exited by itself are both just Exited, and a UI can
	// neither word it correctly nor know whether to offer 停止 or 啟動.
	ComponentDesiredState string `json:"component_desired_state,omitempty"`
	// ComponentVersion is the tag of the image that is running (the short
	// commit, for a git build), and ComponentPreviousVersion the one a
	// rollback would put back — empty when there is none. After a rollback
	// the first is no longer the last commit, which is how a reader can tell.
	ComponentVersion         string `json:"component_version,omitempty"`
	ComponentPreviousVersion string `json:"component_previous_version,omitempty"`

	// Topology says what the last successful build wired together: the
	// services it started and the components it referenced. Empty for image
	// targets and for builds with neither.
	Topology *Topology `json:"topology,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// InputResponse is one declared input and whether it has been supplied. The
// value itself is returned only when the declaration does not mark it secret;
// a secret that has been supplied reports Value "***".
type InputResponse struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Example     string `json:"example,omitempty"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Set         bool   `json:"set"`
	Value       string `json:"value,omitempty"`
}

// SetInputsRequest is the body of PUT /v1/targets/{name}/inputs. Values are
// merged into whatever the target already has; a key with an empty value
// clears that input.
type SetInputsRequest struct {
	Values map[string]string `json:"values"`
}

// CreateStackRequest is the body of POST /v1/stacks. A stack is one deployed
// instance of a manifest (qooq.stack.yaml): several projects deployed together,
// in the order the manifest declares, into a namespace and network named after
// the stack.
type CreateStackRequest struct {
	Name     string `json:"name"`
	RepoURL  string `json:"repo_url"`
	Branch   string `json:"branch,omitempty"`
	AuthType string `json:"auth_type,omitempty"`
	// AuthCredential is a token or PEM-encoded SSH key for the manifest
	// repository. It is never forwarded to a project's external repo: see
	// desiredProjectTarget.
	AuthCredential      string `json:"auth_credential,omitempty"`
	ManifestPath        string `json:"manifest_path,omitempty"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
	// AllowDuplicate is handed to every project target this stack creates.
	// See CreateTargetRequest.AllowDuplicate.
	AllowDuplicate bool `json:"allow_duplicate,omitempty"`
}

// StackResponse is the API view of a stack. AuthCredential is masked to "***"
// when set, never returned in plaintext.
type StackResponse struct {
	Name                string     `json:"name"`
	RepoURL             string     `json:"repo_url"`
	Branch              string     `json:"branch"`
	AuthType            string     `json:"auth_type"`
	AuthCredential      string     `json:"auth_credential,omitempty"`
	ManifestPath        string     `json:"manifest_path"`
	LastManifestCommit  string     `json:"last_manifest_commit,omitempty"`
	PollIntervalSeconds int        `json:"poll_interval_seconds"`
	Status              string     `json:"status"`
	LastError           string     `json:"last_error,omitempty"`
	PausedReason        string     `json:"paused_reason,omitempty"`
	LastSyncAt          *time.Time `json:"last_sync_at,omitempty"`
	NextCheckAt         *time.Time `json:"next_check_at,omitempty"`
	// Projects is filled in by GET /v1/stacks/{name}: one entry per project target,
	// in manifest order where the manifest is known.
	Projects  []StackProjectResponse `json:"projects,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// StackProjectResponse is one project of a stack as seen from the stack: enough to tell
// what it is and how its last deploy went, without having to fetch each project's
// target separately.
type StackProjectResponse struct {
	Name string `json:"name"`
	// Source is the repo URL for a repo: project, or "<repo> (<subdir>)" for a
	// path: project inside the stack's own repositorysitory.
	Source string `json:"source"`
	// depends_on lists what this project waits for, as "name" or "name (started)".
	DependsOn           []string `json:"depends_on,omitempty"`
	TargetName          string   `json:"target_name"`
	LastCommit          string   `json:"last_commit,omitempty"`
	LastBuildStatus     string   `json:"last_build_status"`
	LastBuildError      string   `json:"last_build_error,omitempty"`
	ConsecutiveFailures int      `json:"consecutive_failures,omitempty"`
	// Orphaned marks a project still deployed but no longer in the manifest. qooq
	// never removes one on its own — it may be holding data.
	Orphaned bool `json:"orphaned,omitempty"`
}

// DeployTargetRequest is the body of POST /v1/targets/:name/deploy (M20b): a
// CI pipeline (or any caller holding the watcher's bearer token) tells the
// watcher to deploy a specific image right now, bypassing the poll/build
// pipeline entirely.
type DeployTargetRequest struct {
	Image string `json:"image"`
}

// UpdateTargetRequest is the body of PUT /v1/targets/{name}: the fields of an
// existing target that may change after it is created. Only the branch, for
// now — each field is a pointer so a later one can be added without "absent"
// and "set to empty" meaning the same thing.
//
// A branch, not a tag: the watcher always resolves refs/heads/<branch>, and
// following a ref that never moves is a different feature from following one
// that does.
type UpdateTargetRequest struct {
	Branch *string `json:"branch,omitempty"`
	// AuthType may only move an unauthenticated target to "github-app": a
	// project created before its account granted App access was frozen at
	// "none" and cloned anonymously forever. Nothing that carries a
	// credential can be set this way.
	AuthType *string `json:"auth_type,omitempty"`
	// Domain sets the hostname the platform assigned to this target, in its
	// deploy_spec (the operator layer, which a user's qooq.yaml cannot
	// reach), and rebuilds so it takes effect. An empty string clears it.
	// Nothing else in the deploy_spec changes.
	Domain *string `json:"domain,omitempty"`
}

// ErrorResponse is the uniform error envelope used by both qooqd and
// qooq-watcher.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Topology is what a deployment is connected to, as of its last successful
// build. Services are the databases and caches qooq started for it; References
// are the other components of the same namespace its manifest addressed through
// ${component.<name>.<field>}.
type Topology struct {
	Services   []TopologyService `json:"services,omitempty"`
	References []string          `json:"references,omitempty"`
}

// TopologyService is one manifest services: entry. Phase is read live from the
// daemon when the target is listed; it is not stored.
type TopologyService struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Type      string `json:"type"`
	Version   string `json:"version,omitempty"`
	Phase     string `json:"phase,omitempty"`
	// Dropped is a service the manifest no longer declares. Its container is
	// stopped and its data kept until someone deletes it (DELETE
	// /v1/targets/{name}/services/{service}) or removes the project: deleting
	// a database because a line left a YAML file loses it for good, and a
	// renamed service or an edit undone would want it back.
	Dropped bool `json:"dropped,omitempty"`
}

// UploadSourceResponse is the 202 body of PUT /v1/targets/{name}/source.
//
// BuildGeneration is the target's generation BEFORE the build this upload
// causes, so a caller waiting for the result waits for a generation above
// it — the same baseline rule as the CLI's --wait. BuildQueued is false when
// the archive is identical to what is already deployed (the same revision,
// like pushing a commit that is already HEAD) and no force was asked for:
// nothing will be built, and waiting for a new generation would wait forever.
type UploadSourceResponse struct {
	Revision        string `json:"revision"`
	BuildGeneration uint64 `json:"build_generation"`
	BuildQueued     bool   `json:"build_queued"`
}
