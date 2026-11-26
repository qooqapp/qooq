// Package docker implements runtime.Runtime against the Docker Engine API via
// the official Go SDK over the local socket. All Docker-native types are
// confined to this package.
package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Adapter is the Docker-backed runtime.Runtime.
type Adapter struct {
	cli *client.Client
}

// Ensure the interface is satisfied at compile time.
var _ runtime.Runtime = (*Adapter)(nil)

// New builds an Adapter from the environment (DOCKER_HOST or the default
// socket) with API version negotiation, so it works against OrbStack/Docker
// Desktop and older Linux daemons alike.
func New() (*Adapter, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Adapter{cli: cli}, nil
}

func (a *Adapter) Ping(ctx context.Context) error {
	_, err := a.cli.Ping(ctx)
	return err
}

func (a *Adapter) Close() error { return a.cli.Close() }

// containerdConfig and containerdDefaultRoot locate containerd's root, which
// is where layers live when docker uses the containerd image store — the
// default from Docker 29 — rather than under docker's data root. Docker's API
// does not report it, and it moves with containerd's own config.
const (
	containerdConfig      = "/etc/containerd/config.toml"
	containerdDefaultRoot = "/var/lib/containerd"
)

// ImageStoreRoot is the directory whose filesystem fills as images and build
// layers are written, and whether that is known rather than guessed. Docker's
// data root is reported by its API; containerd's root is read from
// containerd's config, and when that file cannot be read it is not known —
// a guessed path warned against a correct setting and missed a wrong one on a
// machine whose containerd root had been moved (E4).
func (a *Adapter) ImageStoreRoot(ctx context.Context) (string, bool, error) {
	info, err := a.cli.Info(ctx)
	if err != nil {
		return "", false, err
	}
	for _, kv := range info.DriverStatus {
		for _, v := range kv {
			if strings.Contains(v, "io.containerd.snapshotter") {
				return containerdRoot(containerdConfig)
			}
		}
	}
	return info.DockerRootDir, info.DockerRootDir != "", nil
}

// containerdRoot reads the top-level root = "..." from containerd's config.
// A config without one means containerd's default; no readable config means
// the location is not known.
func containerdRoot(path string) (string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break // past the top level; a section's root is something else
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "root" {
			return strings.Trim(strings.TrimSpace(v), `"'`), true, nil
		}
	}
	return containerdDefaultRoot, true, nil
}

func (a *Adapter) PullImage(ctx context.Context, ref string, opts runtime.PullOptions, registryAuth string) (runtime.ImageID, error) {
	rc, err := a.cli.ImagePull(ctx, ref, image.PullOptions{Platform: opts.Platform, RegistryAuth: registryAuth})
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	// The pull only completes once the response stream is fully consumed.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return "", fmt.Errorf("pull %s: draining response: %w", ref, err)
	}
	insp, err := a.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect %s after pull: %w", ref, err)
	}
	return runtime.ImageID(insp.ID), nil
}

func (a *Adapter) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		port, err := nat.NewPort(proto, strconv.Itoa(p.ContainerPort))
		if err != nil {
			return "", fmt.Errorf("port %d/%s: %w", p.ContainerPort, proto, err)
		}
		exposed[port] = struct{}{}
		// No host port requested means the port is reachable on the container
		// network only — which is what the manifest reference has always said
		// a bare `run.ports` entry does. Emitting a binding with an empty
		// HostPort instead tells docker to publish on a *random* host port,
		// and since nothing ever reports that number back, it was a port
		// consumed for nobody's benefit. With an ingress in front, "exposed
		// but not published" is the normal case rather than a curiosity.
		if p.HostPort != 0 {
			hostIP := p.HostIP
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			bindings[port] = []nat.PortBinding{{HostIP: hostIP, HostPort: strconv.Itoa(p.HostPort)}}
		}
	}

	cfg := &container.Config{
		Image:        spec.Image,
		Env:          env,
		Labels:       spec.Labels,
		ExposedPorts: exposed,
	}
	if len(spec.Command) > 0 {
		cfg.Cmd = strslice.StrSlice(spec.Command)
	}
	// Bind mounts go through HostConfig.Mounts rather than the older
	// HostConfig.Binds: Binds makes the daemon silently mkdir a missing
	// source as root, so a typo'd config path turns into an empty directory
	// and the failure only surfaces later inside the app. Mounts fails the
	// create outright with "bind source path does not exist".
	var mounts []mount.Mount
	for _, m := range spec.Mounts {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	hostCfg := &container.HostConfig{
		PortBindings: bindings,
		Mounts:       mounts,
		Resources:    resourcesFor(spec),
	}
	if spec.Harden {
		// Fail closed: a machine set to harden that quietly starts an
		// unhardened container is the failure this setting exists to prevent.
		if err := hardenHostConfig(hostCfg); err != nil {
			return "", fmt.Errorf("create %s: %w", spec.Name, err)
		}
	}

	// Attaching at create time (rather than connecting afterwards) is what
	// makes the container reachable by name from the moment it starts: a
	// user-defined network's embedded DNS resolves container names and
	// aliases, which the default bridge does not do.
	var netCfg *network.NetworkingConfig
	if spec.Network != "" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				spec.Network: {Aliases: spec.NetworkAliases},
			},
		}
	}

	resp, err := a.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", spec.Name, err)
	}
	return runtime.ContainerID(resp.ID), nil
}

// resourcesFor maps the neutral spec's limits onto docker's. Split out so the
// mapping can be tested without a daemon.
func resourcesFor(spec runtime.ContainerSpec) container.Resources {
	r := container.Resources{
		NanoCPUs: spec.CPUNanos,
		Memory:   spec.MemBytes,
	}
	if spec.MemBytes > 0 {
		// Left at 0, docker grants as much swap again: a 512m limit really
		// allows 1g, and a host swapping that hard stalls everyone on it —
		// worse for the neighbours than the OOM the limit was meant to be.
		r.MemorySwap = spec.MemBytes
	}
	if spec.PidsLimit > 0 {
		// A pointer: nil means "leave unset", and docker treats 0 or -1 as
		// unlimited, so only a positive value is passed.
		n := spec.PidsLimit
		r.PidsLimit = &n
	}
	return r
}

// EnsureNetwork creates name as a bridge network unless it already exists.
// Inspect-then-create races with another qooqd (or a concurrent deploy of a
// sibling component) are absorbed: docker answers a duplicate create with a
// conflict, which means the network exists, which is all the caller wanted.
func (a *Adapter) EnsureNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if _, err := a.cli.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect network %s: %w", name, err)
	}

	_, err := a.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"qooq.managed": "true"},
	})
	if err != nil {
		if errdefs.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// RemoveNetworkIfUnused removes a network EnsureNetwork created once nothing
// is attached to it.
//
// The label is the guard against removing a network qooq does not own: a
// manifest may name one the operator made by hand. "Nothing attached" is the
// guard against a network still shared — a stack's namespace network outlives
// any one of its components. Losing the race with a sibling that attaches in
// between is answered by docker refusing the remove, which is fine: the
// network is in use, which is the case this leaves alone.
func (a *Adapter) RemoveNetworkIfUnused(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	nw, err := a.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect network %s: %w", name, err)
	}
	if nw.Labels["qooq.managed"] != "true" || len(nw.Containers) > 0 {
		return nil
	}
	if err := a.cli.NetworkRemove(ctx, name); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	return nil
}

// ConnectNetwork attaches a container to an additional network.
//
// An already-attached container is not an error: the caller wants the
// container on the network, and it is. Docker signals this two different ways
// depending on how the collision is detected — a conflict for the container
// itself, and an unclassified "endpoint with name X already exists" when the
// alias is taken (which happens after a daemon restart, when the container was
// attached by a previous run). Both mean the same thing, and only the first
// has an errdefs class, so the second is matched on its message.
func (a *Adapter) ConnectNetwork(ctx context.Context, id runtime.ContainerID, netName string, aliases []string) error {
	if netName == "" {
		return nil
	}
	err := a.cli.NetworkConnect(ctx, netName, string(id), &network.EndpointSettings{Aliases: aliases})
	if err == nil || errdefs.IsConflict(err) || strings.Contains(err.Error(), "already exists in network") {
		return nil
	}
	return fmt.Errorf("connect %s to network %s: %w", id, netName, err)
}

// DisconnectNetwork takes a container off a network; not being on it, or the
// network or container being gone, already is the state asked for.
func (a *Adapter) DisconnectNetwork(ctx context.Context, id runtime.ContainerID, netName string) error {
	if netName == "" {
		return nil
	}
	err := a.cli.NetworkDisconnect(ctx, netName, string(id), true)
	if err == nil || errdefs.IsNotFound(err) || strings.Contains(err.Error(), "is not connected") {
		return nil
	}
	return fmt.Errorf("disconnect %s from network %s: %w", id, netName, err)
}

// portConflictPatterns are the daemon-side messages that mean "a published
// host port is already bound". Docker reports this as a plain 500 with no
// errdefs classification (it is not IsConflict), so message matching is the
// only signal available; both spellings occur depending on the network driver
// and platform. A miss here is not harmful — the error simply stays a generic
// 500, which is the pre-existing behavior.
var portConflictPatterns = []string{
	"port is already allocated",
	"address already in use",
}

// portConflictAddrPatterns extract the offending "host:port" out of the two
// message shapes so the operator is told which port to change.
var portConflictAddrPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Bind for (\S+?) failed`),        // "Bind for 0.0.0.0:8080 failed: port is already allocated"
	regexp.MustCompile(`listen tcp[46]? (\S+?): bind:`), // "listen tcp4 0.0.0.0:8080: bind: address already in use"
}

// classifyStartErr wraps a ContainerStart failure, tagging the port-conflict
// case with runtime.ErrPortConflict and naming the offending address. The raw
// daemon error stays in the chain so logs keep full fidelity.
func classifyStartErr(id runtime.ContainerID, err error) error {
	msg := err.Error()
	conflict := false
	for _, p := range portConflictPatterns {
		if strings.Contains(msg, p) {
			conflict = true
			break
		}
	}
	if !conflict {
		return fmt.Errorf("start %s: %w", id, err)
	}

	addr := ""
	for _, re := range portConflictAddrPatterns {
		if m := re.FindStringSubmatch(msg); len(m) == 2 {
			addr = strings.TrimSuffix(m[1], ":")
			break
		}
	}
	where := "one of its published host ports"
	if addr != "" {
		where = fmt.Sprintf("host port %s", addr)
	}
	return fmt.Errorf(
		"start %s: %w: %s is already bound by another container or host process — pick a different host port in run.ports (or free that one): %w",
		id, runtime.ErrPortConflict, where, err)
}

func (a *Adapter) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	if err := a.cli.ContainerStart(ctx, string(id), container.StartOptions{}); err != nil {
		return classifyStartErr(id, err)
	}
	return nil
}

func (a *Adapter) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	opts := container.StopOptions{}
	if timeout > 0 {
		secs := int(timeout.Seconds())
		opts.Timeout = &secs
	}
	if err := a.cli.ContainerStop(ctx, string(id), opts); err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}
	return nil
}

func (a *Adapter) RemoveContainer(ctx context.Context, id runtime.ContainerID, force bool) error {
	if err := a.cli.ContainerRemove(ctx, string(id), container.RemoveOptions{Force: force}); err != nil {
		return fmt.Errorf("remove %s: %w", id, err)
	}
	return nil
}

func (a *Adapter) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerStatus, error) {
	info, err := a.cli.ContainerInspect(ctx, string(id))
	if err != nil {
		return runtime.ContainerStatus{}, fmt.Errorf("inspect %s: %w", id, err)
	}
	st := runtime.ContainerStatus{
		ID:   runtime.ContainerID(info.ID),
		Name: strings.TrimPrefix(info.Name, "/"),
	}
	if info.Config != nil {
		st.Image = info.Config.Image
		st.Labels = info.Config.Labels
	}
	if info.State != nil {
		st.ExitCode = info.State.ExitCode
		st.Error = info.State.Error
		st.Phase = mapPhase(info.State.Status, info.State.ExitCode)
		st.StartedAt = parseDockerTime(info.State.StartedAt)
		st.FinishedAt = parseDockerTime(info.State.FinishedAt)
	}
	st.Created = parseDockerTime(info.Created)
	st.IPAddress = containerIP(info.NetworkSettings)
	return st, nil
}

// containerIP picks the address to reach a container at.
//
// A container can sit on several networks (its own, plus the ingress network),
// so "the" IP needs a rule. Any of them routes to the same container, so the
// rule only has to be deterministic: sort the network names and take the first
// non-empty address. Sorting matters because Go map iteration is randomized,
// and an address that changes between two inspects of an unchanged container
// would be a genuinely confusing thing to debug.
func containerIP(ns *container.NetworkSettings) string {
	if ns == nil {
		return ""
	}
	names := make([]string, 0, len(ns.Networks))
	for name := range ns.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ep := ns.Networks[name]; ep != nil && ep.IPAddress != "" {
			return ep.IPAddress
		}
	}
	return ""
}

func (a *Adapter) ListContainers(ctx context.Context, filter runtime.Filter) ([]runtime.ContainerStatus, error) {
	args := filters.NewArgs()
	for k, v := range filter.Labels {
		args.Add("label", k+"="+v)
	}
	summaries, err := a.cli.ContainerList(ctx, container.ListOptions{All: filter.All, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]runtime.ContainerStatus, 0, len(summaries))
	for _, s := range summaries {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		out = append(out, runtime.ContainerStatus{
			ID:      runtime.ContainerID(s.ID),
			Name:    name,
			Image:   s.Image,
			Phase:   mapPhase(s.State, 0),
			Labels:  s.Labels,
			Created: time.Unix(s.Created, 0),
		})
	}
	return out, nil
}

// ListImages lists the images an operator can act on, with no qooq-specific
// filtering.
//
// Intermediate build-cache layers are excluded (All is false). They are not
// addressable: you cannot deploy one, and removing one is the parent image's
// business. They were 64 of 141 entries on the machine this was measured on,
// which buried every real image under rows nothing can be done with.
//
// Dangling images (untagged, but real: an old build whose tag has moved) are
// kept. They hold disk and can be removed, so a passthrough that dropped them
// would make them impossible to find through qooq at all. Hiding them by
// default is the CLI's job, where there is a flag to ask for them back.
func (a *Adapter) ListImages(ctx context.Context) ([]runtime.ImageSummary, error) {
	summaries, err := a.cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	out := make([]runtime.ImageSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, runtime.ImageSummary{
			ID:      runtime.ImageID(s.ID),
			Tags:    s.RepoTags,
			Created: time.Unix(s.Created, 0),
			Size:    s.Size,
			Labels:  s.Labels,
		})
	}
	return out, nil
}

// RemoveImage deletes a locally available image by ref/tag/id (M18 retention
// policy: watcher calls this to prune superseded `component:<sha7>` tags).
// "In use" (a container still references the image) and "not found" are
// wrapped into the neutral runtime.ErrImageInUse/ErrImageNotFound sentinels
// so callers can distinguish "nothing to do" from a real failure without
// importing docker's errdefs package themselves.
func (a *Adapter) RemoveImage(ctx context.Context, ref string) error {
	_, err := a.cli.ImageRemove(ctx, ref, image.RemoveOptions{})
	if err == nil {
		return nil
	}
	switch {
	case errdefs.IsNotFound(err):
		return fmt.Errorf("remove image %s: %w: %w", ref, runtime.ErrImageNotFound, err)
	case errdefs.IsConflict(err):
		return fmt.Errorf("remove image %s: %w: %w", ref, runtime.ErrImageInUse, err)
	default:
		return fmt.Errorf("remove image %s: %w", ref, err)
	}
}

// ResolveImageDigest queries the registry for ref's current content digest
// (no pull, no local cache lookup) via DistributionInspect — used by M20a's
// image-mode watcher targets to detect upstream changes cheaply. Not-found
// and unauthorized are the two errors an operator is likely to hit (typo'd
// ref, missing/expired credential); only not-found is distinguishable via
// errdefs and wrapped into the neutral sentinel, mirroring RemoveImage.
func (a *Adapter) ResolveImageDigest(ctx context.Context, ref string, registryAuth string) (string, error) {
	insp, err := a.cli.DistributionInspect(ctx, ref, registryAuth)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", fmt.Errorf("resolve digest %s: %w: %w", ref, runtime.ErrImageNotFound, err)
		}
		return "", fmt.Errorf("resolve digest %s: %w", ref, err)
	}
	return insp.Descriptor.Digest.String(), nil
}

// Logs streams a container's combined stdout+stderr as plain text. Docker's
// ContainerLogs response is multiplexed (an 8-byte header per frame
// identifying stdout/stderr + length) whenever the container was created
// without a TTY, which is always true for qooq-managed containers — writing
// that raw stream straight to a client produces binary garbage, so it is
// demultiplexed here via stdcopy.StdCopy before anything leaves this
// package. The demux runs in a goroutine feeding an io.Pipe; cancelling ctx
// tears it down promptly (ContainerLogs aborts, StdCopy returns). Closing the
// returned ReadCloser alone only unblocks the goroutine on its next write —
// a silent container keeps it (and the docker connection) alive, so follow
// callers must cancel ctx when they give up.
func (a *Adapter) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}
	rc, err := a.cli.ContainerLogs(ctx, string(id), container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       tail,
		Timestamps: opts.Timestamps,
	})
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", id, err)
	}

	pr, pw := io.Pipe()
	go func() {
		_, cerr := stdcopy.StdCopy(pw, pw, rc)
		rc.Close()
		pw.CloseWithError(cerr)
	}()
	return pr, nil
}

// mapPhase translates Docker-native container state to a neutral Phase. The
// exit code disambiguates a clean exit from a crash (only known via Inspect;
// list summaries pass 0 and therefore report Exited for any stopped container).
func mapPhase(state string, exitCode int) runtime.Phase {
	switch state {
	case "created":
		return runtime.PhaseCreating
	case "restarting":
		return runtime.PhaseCreating
	case "running", "paused":
		return runtime.PhaseRunning
	case "removing":
		return runtime.PhaseRemoved
	case "exited":
		if exitCode != 0 {
			return runtime.PhaseFailed
		}
		return runtime.PhaseExited
	case "dead":
		return runtime.PhaseFailed
	default:
		return runtime.PhasePending
	}
}

func parseDockerTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
