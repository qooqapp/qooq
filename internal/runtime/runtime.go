// Package runtime defines the engine-neutral container runtime abstraction.
// It is the ONLY boundary that internal/core is allowed to depend on for
// container operations. No Docker SDK type may leak through this file: adapters
// (internal/runtime/docker, future containerd/podman) translate to and from the
// neutral types declared here.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrImageNotFound and ErrImageInUse are neutral sentinel errors RemoveImage
// implementations wrap so internal/api can map them to 404/409 without
// importing any engine-specific error package (the docker adapter wraps
// errdefs.IsNotFound/IsConflict into these; other adapters return them
// directly).
var (
	ErrImageNotFound = errors.New("image not found")
	ErrImageInUse    = errors.New("image is in use")
)

// ErrPortConflict is the neutral sentinel StartContainer implementations wrap
// when a container cannot start because one of its published host ports is
// already bound (by another container or by a plain host process). It is an
// operator configuration error, not a server fault, so internal/api maps it to
// 409 with the detail intact rather than masking it behind a generic 500 —
// otherwise the only way to learn "port 8080 is taken" is to read daemon logs.
var ErrPortConflict = errors.New("host port already in use")

// ImageID and ContainerID are opaque, engine-assigned identifiers.
type (
	ImageID     string
	ContainerID string
)

// Phase is the runtime-neutral lifecycle state of a container. Docker's native
// state strings (running/exited/created/dead/...) are mapped to these inside the
// docker adapter and never exposed outward.
type Phase string

const (
	PhasePending  Phase = "Pending"
	PhaseCreating Phase = "Creating"
	PhaseRunning  Phase = "Running"
	PhaseExited   Phase = "Exited"
	PhaseFailed   Phase = "Failed"
	PhaseRemoved  Phase = "Removed"
)

// PortMapping describes a single published port.
type PortMapping struct {
	ContainerPort int
	HostPort      int
	Protocol      string // "tcp" (default) or "udp"
	// HostIP restricts which host address the port is published on. Empty
	// means all of them (0.0.0.0), which is what published ports have always
	// done and what makes an app reachable from the LAN. Setting it to
	// 127.0.0.1 keeps a port reachable only from this machine — for a control
	// interface that must not be exposed, such as the ingress admin API.
	HostIP string
	// AutoHost asks the daemon to pick the host port. It is resolved into a
	// concrete HostPort before any adapter sees the spec and before the spec
	// is persisted, so a runtime never has to interpret it and a stored
	// component always names the port it actually got.
	AutoHost bool
}

// Mount describes one host path made visible inside the container. Source is
// always an absolute host path (the caller resolves every declarative form —
// repo-relative file, named volume — into one before it gets here) and Target
// an absolute path inside the container.
//
// Adapters without container isolation (process) cannot bind-mount; they
// materialize the same intent by placing the content in the instance's working
// directory instead. See internal/runtime/process.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// ContainerSpec is the neutral description used to create a container.
type ContainerSpec struct {
	Name    string
	Image   string
	Command []string // overrides the image's default command when non-empty
	Env     map[string]string
	Ports   []PortMapping
	Mounts  []Mount
	// Network is the name of the network to attach the container to. Empty
	// means the engine's default (docker: the default bridge, where
	// containers can only reach each other by IP). Callers are expected to
	// have called EnsureNetwork first.
	Network string
	// NetworkAliases are the DNS names this container answers to on
	// Network. Only meaningful when Network is set.
	NetworkAliases []string
	Labels         map[string]string
	CPUNanos       int64 // CPU limit in nano-cpus (1 core = 1e9); 0 means unset
	MemBytes       int64 // hard memory cap in bytes; 0 means unset
	// PidsLimit caps processes+threads in the container; 0 means unset.
	// Docker only: the process runtime has no per-instance pids cgroup
	// wiring and ignores it.
	PidsLimit int64
	// Harden narrows what the container can ask of the host kernel: no
	// privilege gain through setuid binaries, fewer capabilities, and a
	// syscall filter stricter than the engine's default. For machines that
	// run strangers' code. Docker only; the process runtime has no sandbox to
	// narrow and ignores it.
	Harden bool
}

// ContainerStatus is the neutral observed state of a container.
type ContainerStatus struct {
	ID       ContainerID
	Name     string
	Image    string
	Phase    Phase
	ExitCode int
	Error    string
	// Created is the container's creation time as reported by the engine
	// (docker: ContainerJSONBase.Created; process: instance meta.CreatedAt).
	// Only InspectContainer is guaranteed to populate it; ListContainers may
	// leave it zero on adapters where the list endpoint omits it cheaply.
	Created    time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Labels     map[string]string
	// IPAddress is the container's address on its network, when the engine
	// has one and the container is running. It exists so a health probe can
	// reach a container that publishes no host port — the normal case once an
	// ingress fronts the app.
	//
	// Empty is always a valid answer (a stopped container, an adapter with no
	// notion of container addresses); callers must have a fallback rather than
	// treating it as an error. Only InspectContainer is guaranteed to populate
	// it. Note that reachability from the host is engine- and platform-
	// dependent: it holds on Linux and OrbStack, not on Docker Desktop, where
	// containers live inside a VM.
	IPAddress string
}

// Filter selects containers by label. All=true includes stopped containers.
type Filter struct {
	Labels map[string]string
	All    bool
}

// Event is a neutral lifecycle event (create/start/die/...).
type Event struct {
	ContainerID ContainerID
	Action      string
	ExitCode    int
	Labels      map[string]string
	Time        time.Time
}

// StatSample is a single resource-usage sample. CPUPercent is already
// normalized inside the adapter (engine-specific math does not leak out).
type StatSample struct {
	ContainerID ContainerID
	CPUPercent  float64
	MemUsage    uint64
	MemLimit    uint64
	Time        time.Time
}

// PullOptions and BuildOptions carry image-acquisition parameters.
type PullOptions struct {
	Platform string
}

type BuildOptions struct {
	Tags       []string
	Dockerfile string
	// MemoryBytes and CPUs bound what the build containers may consume. Zero
	// means unbounded, which is the right default on a machine whose builds are
	// all the operator's own and the wrong one on a machine building code it
	// does not control.
	//
	// Expressed in the same units as a component's own limits (bytes, cores)
	// rather than docker's quota/period pair, so the adapter does the
	// engine-specific arithmetic and nothing above it has to know the
	// convention.
	MemoryBytes int64
	CPUs        float64
	// There is deliberately no pids limit here: docker's classic ImageBuild
	// API (types.ImageBuildOptions) exposes memory, memory-swap and CPU
	// fields but nothing for pids, and BuildKit is not used. A fork bomb in a
	// RUN step is bounded by MemoryBytes/CPUs, the build timeout and the
	// step-kill on abandon, not by a process count.
	// Labels are stamped onto the resulting image. Who sets them is a policy
	// decision that belongs to core, not to an adapter — the same split as
	// container labels.
	Labels map[string]string

	// LogSink receives the build's human-readable output as it is produced.
	//
	// It exists because the output was previously parsed for errors and then
	// thrown away, so "why did my build fail" had no answer anywhere: the
	// compiler message, the missing package, the failing test — all of it was
	// read and discarded inside the adapter. nil keeps that behaviour, which is
	// what every caller that does not want the log should pass.
	//
	// A writer rather than a returned string so the adapter neither buffers nor
	// decides how much to keep; bounding it is the caller's policy.
	LogSink io.Writer
}

// LogOptions carries log-retrieval parameters for Runtime.Logs. Tail=0 means
// "all available output"; Follow=true keeps the stream open and delivers new
// output as it is produced, until ctx is cancelled or the caller stops
// reading.
type LogOptions struct {
	Follow bool
	Tail   int
	// Timestamps prefixes every line with the engine-recorded time (docker:
	// RFC3339Nano, like `docker logs -t`). The process runtime ignores it —
	// its stdout.log/stderr.log are raw app output with no per-line times
	// recorded at write time, so there is nothing to prefix (v1 limitation).
	Timestamps bool
}

// ImageSummary is a neutral view of a locally available image, used by the
// GET /v1/images passthrough listing.
type ImageSummary struct {
	ID      ImageID
	Tags    []string
	Created time.Time
	Size    int64
	// Labels as recorded on the image. Carried so a caller can tell an image
	// qooq built from one that merely happens to live on the same daemon;
	// unlike containers, images have no other marker of provenance.
	Labels map[string]string
}

// Runtime is the container engine abstraction. Implemented by
// internal/runtime/docker; containerd/podman adapters can be added without
// touching internal/core.
type Runtime interface {
	// Image acquisition (three neutral entry points). registryAuth is a
	// docker-style base64(JSON) auth string (see
	// registry.AuthConfig/EncodeAuthConfig in the docker SDK); an empty
	// string means anonymous pull, matching prior behavior.
	PullImage(ctx context.Context, ref string, opts PullOptions, registryAuth string) (ImageID, error)
	LoadImage(ctx context.Context, tarball io.Reader) ([]ImageID, error)
	BuildImage(ctx context.Context, buildCtx io.Reader, opts BuildOptions) (ImageID, error)
	ListImages(ctx context.Context) ([]ImageSummary, error)
	// ResolveImageDigest queries the registry (not the local image cache)
	// for ref's current content digest, without pulling it — used by M20a's
	// image-mode watcher targets to detect upstream image changes. Adapters
	// that cannot support this (process runtime) return a plain error.
	// Implementations should wrap ErrImageNotFound when distinguishable.
	ResolveImageDigest(ctx context.Context, ref string, registryAuth string) (string, error)

	// PushImage uploads a locally built image to the registry its ref names.
	//
	// It exists for the moment qooqd is no longer on the machine that built
	// the image: until then a build already lands in the daemon that will run
	// it, and pushing would add a disk copy and a network round trip for
	// nothing. Once the two are separate machines there is no other way to
	// hand the artifact over.
	//
	// registryAuth is the same base64 X-Registry-Auth blob PullImage takes, so
	// one credential shape serves both directions.
	PushImage(ctx context.Context, ref string, registryAuth string) error

	// PruneBuildCache reclaims build cache above reservedBytes and reports how
	// many bytes it freed.
	//
	// Build cache is the usual way a machine's disk disappears: docker has no
	// per-build disk quota, so the only enforceable bound is to keep the cache
	// under a ceiling afterwards. reservedBytes is what to keep, not what to
	// delete.
	PruneBuildCache(ctx context.Context, reservedBytes int64) (uint64, error)
	// RemoveImage deletes a locally available image by ref/tag/id. Adapters
	// that cannot support this (process runtime: "image" is a host
	// executable path, there is nothing to remove) return a plain error.
	// Implementations should wrap ErrImageNotFound/ErrImageInUse when
	// distinguishable, so callers can react (e.g. skip vs retry) without an
	// engine-specific error type.
	RemoveImage(ctx context.Context, ref string) error

	// EnsureNetwork creates the named network if it does not already exist,
	// and is a no-op when it does — callers invoke it before every create
	// rather than tracking network state themselves. Adapters without
	// network isolation (process) return nil: everything already shares the
	// host's network, which is the outcome the caller wanted.
	EnsureNetwork(ctx context.Context, name string) error

	// RemoveNetworkIfUnused removes the named network when qooq created it
	// and no container is attached to it any more; otherwise it does
	// nothing. A network qooq did not create is never removed, and one that
	// does not exist is not an error. Adapters without network isolation
	// return nil.
	RemoveNetworkIfUnused(ctx context.Context, name string) error

	// ConnectNetwork attaches an existing container to an additional network
	// under the given aliases. A container is created on one network (see
	// ContainerSpec.Network); this is how it joins a second one, which is what
	// lets an ingress reach an app that also has its own private network.
	//
	// Called between create and start. Re-attaching a container that is
	// already on the network is not an error: callers reconcile rather than
	// track membership. Adapters without network isolation return nil.
	ConnectNetwork(ctx context.Context, id ContainerID, network string, aliases []string) error

	// DisconnectNetwork takes a container off a network. A container that is
	// not on it, or a network that does not exist, is not an error. Adapters
	// without network isolation return nil.
	DisconnectNetwork(ctx context.Context, id ContainerID, network string) error

	// Lifecycle.
	CreateContainer(ctx context.Context, spec ContainerSpec) (ContainerID, error)
	StartContainer(ctx context.Context, id ContainerID) error
	StopContainer(ctx context.Context, id ContainerID, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id ContainerID, force bool) error

	// Observation.
	InspectContainer(ctx context.Context, id ContainerID) (ContainerStatus, error)
	ListContainers(ctx context.Context, filter Filter) ([]ContainerStatus, error)

	// Exec runs cmd inside a running container and waits for it to finish,
	// returning its exit code. Used by the "exec" health-check probe type.
	Exec(ctx context.Context, id ContainerID, cmd []string) (int, error)

	// Logs returns a stream of the container's combined stdout+stderr output
	// as plain text (no engine-specific multiplexing/framing leaks through).
	// The caller must Close the returned reader. When opts.Follow is true the
	// stream stays open and delivers new output until ctx is cancelled —
	// cancelling ctx is what guarantees background work stops promptly.
	// Closing the reader alone only unblocks the adapter on its next write
	// (i.e. when the container produces output), so a follow caller that may
	// give up must hold a cancellable ctx and cancel it, as the HTTP handler
	// does via r.Context().
	Logs(ctx context.Context, id ContainerID, opts LogOptions) (io.ReadCloser, error)

	// Monitoring streams.
	Events(ctx context.Context, filter Filter) (<-chan Event, <-chan error)
	Stats(ctx context.Context, id ContainerID) (<-chan StatSample, <-chan error)

	Ping(ctx context.Context) error
	Close() error
}
