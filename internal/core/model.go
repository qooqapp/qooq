// Package core holds the qooq domain model and orchestration use-cases. It
// depends only on the runtime.Runtime abstraction and the Store interface it
// declares here (consumer-defined) — never on a concrete engine or database.
package core

import (
	"errors"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Labels attached to every qooq-managed container. Reconcile and orphan
// detection rely on them. Group membership is not a container label — it lives
// on the Component via Spec.GroupID.
const (
	LabelManaged     = "qooq.managed"
	LabelComponentID = "qooq.component_id"
	// LabelSpec holds the component's ComponentSpec, marshaled to JSON, at
	// container-creation time (see (s *Service).createAndStart). It lets a
	// daemon pointed at a fresh/empty store (e.g. after `docker compose down
	// -v` wiped the bbolt volume but left sibling containers running)
	// reconstruct a Component record from the live container alone — see
	// (s *Service).adoptOrphan in reconcile.go. Env values inside the spec
	// may be sensitive, but `docker inspect` already exposes container env in
	// plaintext, so this label adds no new disclosure surface.
	LabelSpec = "qooq.spec"
	// LabelInstance identifies which qooqd instance (the -instance flag /
	// QOOQ_INSTANCE env) considers a container its own. Absent or empty means
	// the default, unnamed instance — this keeps containers created before
	// M20 (instance labeling) and single-instance deployments working
	// unchanged. Every code path that touches a container it did not itself
	// just create must check (s *Service).OwnsContainer first: containers
	// belonging to a different qooqd instance sharing the same docker host
	// must be completely invisible (no reconcile, no event handling, no
	// orphan adoption, not even a log line).
	LabelInstance = "qooq.instance"
)

// LabelBuilt marks an image qooq built, as opposed to one it merely pulled or
// one that was already on the daemon.
//
// It exists because images, unlike containers, share a single flat namespace
// with everything else on the host and carry no other sign of where they came
// from. Without it, "clean up the images qooq made" can only be guessed at from
// the tag's shape — and `<name>:<short sha>` is a common enough convention that
// guessing means eventually deleting somebody else's build.
//
// It is deliberately not a filter on ListImages: the daemon's view stays the
// whole truth, and deciding what to show is the caller's.
const LabelBuilt = "qooq.built"

// ErrNotFound is returned by Store when a key is absent.
var ErrNotFound = errors.New("not found")

// ErrNameConflict is returned by Deploy when a non-removed component already
// uses the requested name (names are how the CLI addresses components, so
// duplicates would be ambiguous).
var ErrNameConflict = errors.New("component name already in use")

// DesiredState is the operator's intent for a component, and the single source
// of truth for the restart manager and reconcile loop: only a component whose
// DesiredState is Running is ever auto-restarted. Deploy/Start set Running;
// Stop sets Stopped.
const (
	DesiredRunning = "Running"
	DesiredStopped = "Stopped"
)

// ComponentSpec is the desired state of a deployment unit. Replicas is stored
// but not yet acted upon (reserved for later). RestartPolicy is one of "no"
// (default; empty string is treated as "no"), "on-failure" or "always".
type ComponentSpec struct {
	// Namespace scopes Name. Components in different namespaces may share a
	// name: each namespace gets its own network, so the bare name still
	// resolves unambiguously inside it, and the container name is qualified
	// (see containerName). Empty means the unnamespaced default, which is
	// what every component created before this field existed decodes to —
	// their container names and addresses are unchanged.
	Namespace string                `json:"namespace,omitempty"`
	Name      string                `json:"name"`
	Image     string                `json:"image"` // registry ref or image id
	Command   []string              `json:"command,omitempty"`
	Env       map[string]string     `json:"env,omitempty"`
	Ports     []runtime.PortMapping `json:"ports,omitempty"`
	// Mounts are host paths made visible inside the container. Every Source
	// is an absolute host path: the declarative forms (repo-relative file,
	// named volume) are resolved by whoever writes the spec — the watcher
	// materializes them before deploying. Records written before M23 have no
	// mounts key and decode to nil, which means "no mounts" everywhere.
	Mounts []runtime.Mount `json:"mounts,omitempty"`
	// Network is the container network this component joins. Components on
	// the same network reach each other by component name, which is the only
	// way for one component to address another that survives a redeploy
	// (container IPs do not). Empty keeps the engine default, i.e. pre-M23
	// behavior.
	Network       string            `json:"network,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	GroupID       string            `json:"group_id,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
	Replicas      int               `json:"replicas,omitempty"`
	// Domain is the public hostname this component is meant to serve
	// (e.g. "app.example.com"). It is the one-hostname shorthand for
	// Ingress.Hosts, kept because it predates it and because one hostname is
	// the common case. When an ingress is running it is routed; without one it
	// is inert metadata, echoed back and nothing else.
	Domain string `json:"domain,omitempty"`
	// Ingress refines how a reverse proxy exposes this component. Nil means
	// the defaults: every hostname derived from the component's name, routed
	// whole to its first port. Ignored entirely when no ingress is running.
	Ingress       *IngressSpec `json:"ingress,omitempty"`
	CPULimit      float64      `json:"cpu_limit,omitempty"`       // CPU cores, e.g. 0.5
	MemLimitBytes int64        `json:"mem_limit_bytes,omitempty"` // hard memory cap in bytes
	HealthCheck   *HealthCheck `json:"health_check,omitempty"`
}

// IngressSpec refines how a reverse proxy exposes a component. Every field is
// optional; the zero value means "the defaults", which is what a component
// with no ingress: block gets.
//
// core stores and forwards this without acting on it — routing lives in
// internal/ingress. It sits on the spec because that is what survives a
// daemon restart and what the proxy's route table is derived from.
type IngressSpec struct {
	// Hosts are additional hostnames to route here, on top of the ones
	// derived from the component's name and the Domain shorthand.
	Hosts []string `json:"hosts,omitempty"`
	// Port selects which container port to forward to, for a component that
	// exposes several. Zero means the first one in the spec.
	Port int `json:"port,omitempty"`
	// Path restricts the route to a URL prefix, so two components can share a
	// hostname. Empty routes the whole host.
	Path string `json:"path,omitempty"`
	// StripPath removes Path before forwarding. False (the default) passes the
	// URL through untouched, which is right for an app that already expects to
	// live under that prefix; true is right for one that serves from its root.
	StripPath bool `json:"strip_path,omitempty"`
	// TLS overrides the daemon's default for this component. Nil follows the
	// daemon; the pointer is what distinguishes "not specified" from "off".
	TLS *bool `json:"tls,omitempty"`
	// Off keeps the component out of the ingress entirely: no route, no
	// hostname. Set by the watcher on the services it starts — a database is
	// reached from its app's network, not through a public name.
	Off bool `json:"off,omitempty"`
}

// ComponentStatus is the observed state of a deployment unit.
type ComponentStatus struct {
	ContainerID   string        `json:"container_id,omitempty"`
	Phase         runtime.Phase `json:"phase"`
	DesiredState  string        `json:"desired_state,omitempty"` // Running / Stopped
	LastExitCode  int           `json:"last_exit_code"`
	CrashCount    int           `json:"crash_count"`
	RestartCount  int           `json:"restart_count"`
	PreviousImage string        `json:"previous_image,omitempty"`
	StartedAt     time.Time     `json:"started_at,omitempty"`
	FinishedAt    time.Time     `json:"finished_at,omitempty"`
	LastRestartAt time.Time     `json:"last_restart_at,omitempty"`
	BackoffUntil  time.Time     `json:"backoff_until,omitempty"`
	// RestartExhausted is true once EvaluateRestart has given up after
	// maxConsecutiveRestarts consecutive restarts (see restart.go): the
	// restart manager stops re-triggering for this component until a manual
	// Start, Upsert (replace) or Rollback clears the flag and gives it a
	// fresh run of attempts.
	RestartExhausted bool `json:"restart_exhausted,omitempty"`
	// HealthyAt is when this container's health probe first passed, zero
	// while it never has.
	//
	// The phase says a container is running, which is not the same claim: a
	// database process that has started is still seconds away from accepting
	// connections, and anything that connects on startup fails in that window.
	// The health probe already knows the difference — this is what makes the
	// answer readable by anyone but the checker goroutine holding it.
	//
	// Scoped to the current container: createAndStart clears it, so a value
	// here always describes the container named in ContainerID and never the
	// one it replaced.
	HealthyAt time.Time `json:"healthy_at,omitempty"`

	// AdvertiseHost is the address the daemon was advertising when this
	// component was last deployed.
	//
	// Recorded because references resolved into a component's environment —
	// ${component.x.endpoint}, and anything an operator wrote by hand — are
	// baked in at deploy time. When the machine changes networks they keep
	// naming the address that was current then, and nothing about the
	// component says so: it runs, its probe passes, and only a browser finds
	// out. Keeping the value is what makes that difference visible.
	AdvertiseHost string `json:"advertise_host,omitempty"`

	// LastError is the human-readable reason the component last failed to
	// create or start (truncated to maxLastErrorBytes). It exists so an
	// operator can answer "why is this Failed?" from `component get` alone
	// instead of correlating daemon logs by hand. Cleared on every successful
	// start, so a non-empty value always describes the current Failed state.
	LastError string `json:"last_error,omitempty"`
}

// Component is a deployment unit. Its identity is the qooq ULID; the container
// ID is only an attribute, so a crash-rebuild does not change the component.
type Component struct {
	ID        string          `json:"id"`
	Spec      ComponentSpec   `json:"spec"`
	Status    ComponentStatus `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Group is a logical grouping for batch operations (membership lives on the
// Component via GroupID). CRUD is wired in M3.
type Group struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Event is an append-only lifecycle/crash record for a component.
type Event struct {
	ComponentID string    `json:"component_id"`
	Type        string    `json:"type"`
	Message     string    `json:"message,omitempty"`
	ExitCode    int       `json:"exit_code"`
	Time        time.Time `json:"time"`
}

// BatchResult is the per-member outcome of a group batch operation. Err is a
// non-empty string when that member failed; the batch never aborts on it.
type BatchResult struct {
	ComponentID string        `json:"component_id"`
	Name        string        `json:"name"`
	Phase       runtime.Phase `json:"phase"`
	Err         string        `json:"err,omitempty"`
}
