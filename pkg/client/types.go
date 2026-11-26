// Package client is the Go SDK for the qooq REST API. The wire DTO types here
// are the single source of truth for the JSON contract; internal/api encodes
// and decodes these same types.
package client

import "time"

// HealthResponse is returned by GET /v1/healthz.
type HealthResponse struct {
	Status  string `json:"status"`
	Runtime string `json:"runtime"`
}

// VersionResponse is returned by GET /v1/version.
type VersionResponse struct {
	Version    string `json:"version"`
	Runtime    string `json:"runtime"`
	APIVersion string `json:"api_version"`
}

// PortSpec is a published port in a deploy request.
type PortSpec struct {
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	// AutoHost asks the daemon to choose the host port, written "auto:8080"
	// wherever ports are spelled as strings. The response always carries the
	// port that was chosen, with AutoHost false: it is a request, not a
	// property of a deployed component.
	//
	// It exists because a host port is the one thing an app's own repo cannot
	// know — it belongs to the machine, not the code — and picking one by hand
	// means every app on a host has to be checked against every other.
	AutoHost bool `json:"auto_host,omitempty"`
}

// MountSpec makes a host path visible inside the container. HostPath and
// ContainerPath must both be absolute: the declarative forms a qooq.yaml can
// use (repo-relative file, named volume) are resolved into host paths by the
// watcher before the deploy request is built, so qooqd only ever sees
// already-resolved paths.
type MountSpec struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only,omitempty"`
}

// HealthCheckSpec configures liveness probing in a deploy request/response.
// Type is "http" (HTTPPath+HTTPPort, probed against the published host port)
// or "exec" (ExecCommand, run inside the container). Unset numeric fields get
// server-side defaults (interval 10s, timeout 5s, failure_threshold 3,
// initial_delay 5s).
type HealthCheckSpec struct {
	Type                string   `json:"type"`
	HTTPPath            string   `json:"http_path,omitempty"`
	HTTPPort            int      `json:"http_port,omitempty"`
	ExecCommand         []string `json:"exec_command,omitempty"`
	IntervalSeconds     int      `json:"interval_seconds,omitempty"`
	TimeoutSeconds      int      `json:"timeout_seconds,omitempty"`
	FailureThreshold    int      `json:"failure_threshold,omitempty"`
	InitialDelaySeconds int      `json:"initial_delay_seconds,omitempty"`
}

// DeployRequest is the body of POST /v1/components.
type DeployRequest struct {
	// Namespace scopes Name: two namespaces may each hold a component called
	// "api". Empty is the unnamespaced default and behaves exactly as before
	// namespaces existed.
	Namespace     string            `json:"namespace,omitempty"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Command       []string          `json:"command,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Ports         []PortSpec        `json:"ports,omitempty"`
	Mounts        []MountSpec       `json:"mounts,omitempty"`
	Network       string            `json:"network,omitempty"` // container network to join; siblings on it resolve each other by component name
	GroupID       string            `json:"group_id,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"` // "", "no", "on-failure" or "always"
	CPULimit      float64           `json:"cpu_limit,omitempty"`      // CPU cores, e.g. 0.5
	MemLimitBytes int64             `json:"mem_limit_bytes,omitempty"`
	Domain        string            `json:"domain,omitempty"` // public hostname; the one-host shorthand for Ingress.Hosts
	Ingress       *IngressSpec      `json:"ingress,omitempty"`
	HealthCheck   *HealthCheckSpec  `json:"health_check,omitempty"`
}

// IngressSpec refines how a reverse proxy exposes a component. Every field is
// optional; omitting the whole block means every hostname derived from the
// component's name, routed whole to its first port. Ignored when the daemon
// runs no ingress.
type IngressSpec struct {
	// Hosts are additional hostnames, on top of the derived ones and Domain.
	Hosts []string `json:"hosts,omitempty"`
	// Port picks which container port to forward to; 0 means the first.
	Port int `json:"port,omitempty"`
	// Path restricts the route to a URL prefix so two components can share a
	// hostname; empty routes the whole host.
	Path string `json:"path,omitempty"`
	// StripPath removes Path before forwarding (default: pass through as-is).
	StripPath bool `json:"strip_path,omitempty"`
	// TLS overrides the daemon default for this component; nil follows it.
	TLS *bool `json:"tls,omitempty"`
	// Off keeps the component out of the ingress: no route, no hostname.
	Off bool `json:"off,omitempty"`
}

// ComponentResponse is the API view of a component.
type ComponentResponse struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// ContainerName is the engine-level name ("<namespace>_<name>", or just
	// the name when unnamespaced) — what `docker ps` shows.
	ContainerName string     `json:"container_name,omitempty"`
	Image         string     `json:"image"`
	Phase         string     `json:"phase"`
	ContainerID   string     `json:"container_id,omitempty"`
	ExitCode      int        `json:"exit_code"`
	CrashCount    int        `json:"crash_count"`
	GroupID       string     `json:"group_id,omitempty"`
	Ports         []PortSpec `json:"ports,omitempty"`
	// Endpoints are ready-to-open URLs for the published host ports, composed
	// by the daemon from its advertise host — so a client never has to guess
	// what address this machine is reachable on. Empty when the component
	// publishes no host port (nothing outside the container network can reach
	// it). Always http and never the component's domain: qooqd terminates no
	// TLS and routes no domain, so anything else would be an address that
	// doesn't answer.
	Endpoints []string `json:"endpoints,omitempty"`
	// AdvertiseHostAtDeploy is the address the daemon advertised when this
	// component was last deployed, present only when it differs from the
	// current one AND this component's environment actually names it — i.e.
	// only when something it carries has genuinely gone stale.
	//
	// The pair exists so a reader does not have to compare two values from
	// two places to answer "is this why my browser cannot reach it".
	AdvertiseHostAtDeploy string `json:"advertise_host_at_deploy,omitempty"`
	// AdvertiseHostNow accompanies it: what a redeploy would write instead.
	AdvertiseHostNow string            `json:"advertise_host_now,omitempty"`
	Mounts           []MountSpec       `json:"mounts,omitempty"`
	Network          string            `json:"network,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	DesiredState     string            `json:"desired_state,omitempty"`
	RestartPolicy    string            `json:"restart_policy,omitempty"`
	RestartCount     int               `json:"restart_count"`
	PreviousImage    string            `json:"previous_image,omitempty"`
	CPULimit         float64           `json:"cpu_limit,omitempty"`
	MemLimitBytes    int64             `json:"mem_limit_bytes,omitempty"`
	Domain           string            `json:"domain,omitempty"`
	Ingress          *IngressSpec      `json:"ingress,omitempty"`
	LastRestartAt    time.Time         `json:"last_restart_at,omitempty"`
	BackoffUntil     time.Time         `json:"backoff_until,omitempty"`
	RestartExhausted bool              `json:"restart_exhausted,omitempty"`
	// LastError explains why the component last failed to create or start.
	// Empty whenever the component is running normally.
	LastError   string           `json:"last_error,omitempty"`
	HealthCheck *HealthCheckSpec `json:"health_check,omitempty"`
	// HealthyAt is when the running container's health probe first passed,
	// zero while it never has (and always zero for a component with no
	// health check, which makes no such claim either way).
	//
	// It is the difference between "started" and "ready": anything that
	// waits for this component — a stack's depends_on, an app whose database
	// this is — needs the second question answered, and StartedAt answers
	// only the first.
	HealthyAt time.Time `json:"healthy_at,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PullRequest is the body of POST /v1/images/pull. Username/Password (M20a)
// are optional registry credentials for private images; empty means
// anonymous pull, matching prior behavior.
type PullRequest struct {
	Ref      string `json:"ref"`
	Platform string `json:"platform,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// PushRequest is the body of POST /v1/images/push.
//
// Username/Password rather than a pre-encoded auth blob, matching PullRequest:
// one credential shape for both directions, and the encoding stays an API-layer
// detail rather than something every caller has to reproduce.
type PushRequest struct {
	Ref      string `json:"ref"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// PullResponse is returned by POST /v1/images/pull.
type PullResponse struct {
	ImageID string `json:"image_id"`
}

// ImageDigestRequest is the body of POST /v1/images/digest (M20a).
// Username/Password are optional registry credentials.
type ImageDigestRequest struct {
	Ref      string `json:"ref"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// ImageDigestResponse is returned by POST /v1/images/digest.
type ImageDigestResponse struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// LoadResponse is returned by POST /v1/images/load.
type LoadResponse struct {
	ImageIDs []string `json:"image_ids"`
}

// BuildResponse is returned by POST /v1/images/build.
type BuildResponse struct {
	ImageID string `json:"image_id"`
	// Log is the build's output, tail-truncated by the daemon. Empty from a
	// daemon older than this field, which is why nothing treats its absence as
	// a failure.
	Log string `json:"log,omitempty"`
}

// ImageResponse is one entry in GET /v1/images.
type ImageResponse struct {
	ID      string    `json:"id"`
	Tags    []string  `json:"tags,omitempty"`
	Created time.Time `json:"created"`
	Size    int64     `json:"size"`
	// Built reports whether qooq built this image (the qooq.built label), as
	// opposed to pulling it or finding it already there. Images share one flat
	// namespace with everything else on the host, so this is the only way to
	// tell qooq's own from the rest.
	Built bool `json:"built,omitempty"`
}

// StatsResponse is returned by GET /v1/components/{id}/stats (latest sample).
type StatsResponse struct {
	ComponentID string    `json:"component_id"`
	CPUPercent  float64   `json:"cpu_percent"`
	MemUsage    uint64    `json:"mem_usage"`
	MemLimit    uint64    `json:"mem_limit"`
	Time        time.Time `json:"time"`
}

// EventResponse is one entry in GET /v1/components/{id}/events.
type EventResponse struct {
	ComponentID string    `json:"component_id"`
	Type        string    `json:"type"`
	Message     string    `json:"message,omitempty"`
	ExitCode    int       `json:"exit_code"`
	Time        time.Time `json:"time"`
}

// CreateGroupRequest is the body of POST /v1/groups.
type CreateGroupRequest struct {
	Name string `json:"name"`
}

// GroupResponse is the API view of a group.
type GroupResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// GroupDetailResponse adds the group's current members.
type GroupDetailResponse struct {
	GroupResponse
	Members []ComponentResponse `json:"members"`
}

// AddMemberRequest is the body of POST /v1/groups/{id}/members.
type AddMemberRequest struct {
	ComponentID string `json:"component_id"`
}

// BatchResultResponse is one member's outcome from a group start/stop/delete.
type BatchResultResponse struct {
	ComponentID string `json:"component_id"`
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	Err         string `json:"err,omitempty"`
}

// CapacityResponse is returned by GET /v1/system/capacity: host resource
// info (not container/runtime state) used by watcher's M14 `requires`
// validation before it builds/deploys an app-manifest-described component.
type CapacityResponse struct {
	CPUCores    int    `json:"cpu_cores"`
	MemoryBytes int64  `json:"memory_bytes"`
	Arch        string `json:"arch"`
	OS          string `json:"os"`
	// Runtime is the qooqd runtime backend ("docker" or "process"), added in
	// M16 so watcher can validate a manifest's requires.runtime before
	// building. Empty on a pre-M16 qooqd (defensive: caller must treat empty
	// as "unknown, skip validation", not as a mismatch).
	Runtime string `json:"runtime"`
	// AdvertiseHost is the address this host is reachable at from other
	// machines (-advertise-host/QOOQ_ADVERTISE_HOST, or detected). It is
	// exposed here so a manifest can reference it as ${host} instead of
	// hardcoding a LAN IP that only works on one machine. Empty on a
	// pre-existing qooqd or when detection found nothing.
	AdvertiseHost string `json:"advertise_host,omitempty"`
}

// ErrorResponse is the uniform error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
	// BuildLog carries a failed build's output alongside the failure.
	//
	// Only the build endpoint sets it. It is on the shared envelope rather than
	// in a bespoke shape because the status mapping is shared too, and a second
	// error type for one endpoint would be a second thing to keep in step.
	BuildLog string `json:"build_log,omitempty"`
}
