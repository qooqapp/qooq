package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/pkg/client"
)

// stopTimeout is the graceful-stop grace period before SIGKILL.
const stopTimeout = 10 * time.Second

// ErrNothingToRollback is returned by Rollback when the component has no
// PreviousImage recorded (it has never been upserted/rolled back before).
var ErrNothingToRollback = errors.New("nothing to roll back to")

// ErrBuildTimeout is a build stopped by the machine's build timeout.
//
// Named so the API can report it with its detail intact: this is the operator's
// policy taking effect, not a fault, and the same reasoning applies as for
// runtime.ErrPortConflict.
var ErrBuildTimeout = errors.New("build exceeded this daemon's build timeout")

// ErrLowDisk is a build refused because the machine is short of disk space.
// Not the repository's doing — every build is refused until space is freed.
var ErrLowDisk = errors.New("this server is low on disk space, so builds are paused")

// ErrImageTooLarge is a build whose image was larger than the machine allows.
// The image is removed.
var ErrImageTooLarge = errors.New("the built image is larger than this server allows")

// Service orchestrates component lifecycle over a Runtime and a Store. All
// mutating operations on a single component are serialized by a per-id mutex,
// because HTTP handlers, the reconciler and (later) the events goroutine can
// touch the same component concurrently.
type Service struct {
	rt          runtime.Runtime
	store       Store
	runtimeName string
	// instance is this daemon's qooq.instance label value (see
	// LabelInstance); "" is the default, unnamed instance. It is stamped on
	// every container this Service creates and used by OwnsContainer to
	// decide whether a live container observed on the shared docker host
	// belongs to this process at all.
	instance string
	// ingressNetwork, when set, is the network the ingress proxy lives on.
	// Empty (the default) means no ingress. Set once at startup, before any
	// request is served.
	ingressNetwork string
	// autoPortLow/High bound the range AutoHost ports are drawn from.
	autoPortLow, autoPortHigh int
	// publishIP is the host address published ports bind to by default
	// (SetPublishIP); "" is the engine's own default, 0.0.0.0.
	publishIP string
	// advertiseHost reports the address this daemon currently advertises, or
	// nil when nothing supplied one. A function rather than a string: the
	// address changes while the daemon runs.
	advertiseHost func() string
	log           *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// deployMu serializes the name-uniqueness check with the first save in
	// Deploy: the per-id lock cannot do this, because concurrent deploys of
	// the same name run under different ids.
	deployMu sync.Mutex

	// statsMu guards latestStats, an in-memory cache holding only the most
	// recent StatSample per component id (no time-series). The events/stats
	// goroutines write it; HTTP handlers read it.
	statsMu     sync.Mutex
	latestStats map[string]runtime.StatSample

	// buildLimits is the machine's build policy (see BuildLimits).
	buildLimits BuildLimits

	// containerLimits is the machine's per-container resource policy.
	containerLimits ContainerLimits
}

// NewService wires a Service. runtimeName is surfaced by /healthz and /version.
// instance is this daemon's qooq.instance label value ("" = default instance,
// see LabelInstance).
func NewService(rt runtime.Runtime, store Store, runtimeName, instance string, log *slog.Logger) *Service {
	return &Service{
		rt:           rt,
		store:        store,
		runtimeName:  runtimeName,
		instance:     instance,
		autoPortLow:  DefaultAutoPortLow,
		autoPortHigh: DefaultAutoPortHigh,
		log:          log,
		locks:        make(map[string]*sync.Mutex),
		latestStats:  make(map[string]runtime.StatSample),
	}
}

// BuildLimits bounds what an image build may consume.
//
// Machine policy: the operator of the host decides it, and nothing that submits
// a build can influence it. That split is the point — on a shared node the thing
// submitting a build is a repository you do not control.
type BuildLimits struct {
	// Timeout bounds one build. Zero means unbounded, which is almost never
	// what anyone wants: see BuildImage.
	Timeout time.Duration
	// MemoryBytes and CPUs bound the build containers. Zero means unbounded,
	// the right default for a machine building only its operator's own code.
	MemoryBytes int64
	CPUs        float64
	// CacheReservedBytes is how much build cache to keep after a build. Zero
	// disables pruning entirely, which is correct for a single-operator machine
	// where the cache is a pure speed win.
	CacheReservedBytes int64
	// ImageMaxBytes refuses a built image larger than this, removing it.
	// docker has no quota for what a build writes, so a Dockerfile can leave a
	// multi-gigabyte image behind (measured: 3.2 GB, deployed, and kept twice
	// by the watcher's retention). Zero means no limit.
	ImageMaxBytes int64
	// MinFreeDisk is the share of the disk (0–1) that must be free for a build
	// to start; below it every build is refused until space comes back. It
	// protects the machine rather than any one user: a full disk takes down
	// every database on it. Zero disables the check, as does a nil FreeDisk.
	MinFreeDisk float64
	// FreeDisk reports free and total bytes where builds write. Injected by
	// the composition root because it is a platform call (statfs).
	FreeDisk func() (free, total uint64, err error)
}

// SetBuildLimits installs the machine's build policy.
func (s *Service) SetBuildLimits(l BuildLimits) { s.buildLimits = l }

// SetIngressNetwork names the network the ingress proxy is created on, or ""
// (the default) for no ingress. Components do not join it (ConnectProxy).
//
// It is a plain string rather than a dependency on the ingress package because
// core must not know what a reverse proxy is: from here this is only "one more
// network to attach to", and that keeps the direction of dependency pointing
// inward.
func (s *Service) SetIngressNetwork(name string) { s.ingressNetwork = name }

// SetAdvertiseHost supplies the daemon's current advertise host, recorded on
// each component as it is deployed. Called once at startup.
func (s *Service) SetAdvertiseHost(fn func() string) { s.advertiseHost = fn }

// CurrentAdvertiseHost is "" when none is known — which is also what a
// component deployed before this was recorded carries.
func (s *Service) CurrentAdvertiseHost() string {
	if s.advertiseHost == nil {
		return ""
	}
	return s.advertiseHost()
}

// The ingress reaches a component by joining the component's own network,
// never the other way round. Components used to
// join one shared ingress network, and on it every user's containers —
// databases included — could reach every other's by a guessable name. Now the
// proxy is the only container on more than one user's network.

// ConnectProxy joins the proxy container to the network a component lives on,
// so the proxy resolves it by its qualified container name there.
//
// A component with no network of its own sits on the engine's default bridge,
// where names do not resolve; that one alone still joins the ingress network
// under its qualified name, which is how it was reached before. Re-connecting
// is absorbed by the adapter, so callers can simply keep asking.
func (s *Service) ConnectProxy(ctx context.Context, proxy runtime.ContainerID, comp *Component) error {
	if s.ingressNetwork == "" || comp.Status.ContainerID == "" || comp.Spec.Network == s.ingressNetwork {
		return nil
	}
	if comp.Spec.Network == "" {
		if err := s.rt.EnsureNetwork(ctx, s.ingressNetwork); err != nil {
			return fmt.Errorf("ensure ingress network %s: %w", s.ingressNetwork, err)
		}
		return s.rt.ConnectNetwork(ctx, runtime.ContainerID(comp.Status.ContainerID), s.ingressNetwork, []string{ContainerName(comp.Spec)})
	}
	if err := s.rt.ConnectNetwork(ctx, proxy, comp.Spec.Network, nil); err != nil {
		return fmt.Errorf("connect the ingress proxy to %s: %w", comp.Spec.Network, err)
	}
	return nil
}

// LeaveIngressNetwork takes a component off the shared ingress network, where
// a daemon from before ConnectProxy put every component. A component with no
// network of its own is left on it: it has no other way to be reached.
func (s *Service) LeaveIngressNetwork(ctx context.Context, comp *Component) error {
	if s.ingressNetwork == "" || comp.Status.ContainerID == "" || comp.Spec.Network == "" || comp.Spec.Network == s.ingressNetwork {
		return nil
	}
	return s.rt.DisconnectNetwork(ctx, runtime.ContainerID(comp.Status.ContainerID), s.ingressNetwork)
}

// OwnsContainer reports whether a live container (identified by its docker
// labels) belongs to this qooqd instance: its qooq.instance label (absent
// counts as "") must equal this Service's own instance. Every reconcile,
// event-handling and orphan-adoption path that inspects a container it did
// not just create itself must call this first and bail out silently (no log,
// no state change) when it returns false — see LabelInstance's doc comment
// for why.
func (s *Service) OwnsContainer(labels map[string]string) bool {
	return labels[LabelInstance] == s.instance
}

// lockFor returns the per-component mutex, creating it on first use.
func (s *Service) lockFor(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

// RuntimeName reports the active runtime adapter name.
func (s *Service) RuntimeName() string { return s.runtimeName }

// Backup streams a consistent snapshot of the datastore to w. It is a thin
// passthrough to the Store; no lock is taken here because the store's snapshot
// is itself point-in-time consistent (see bbolt.Store.Backup).
func (s *Service) Backup(w io.Writer, sizeHint func(int64)) error {
	return s.store.Backup(w, sizeHint)
}

// Ping checks runtime reachability.
func (s *Service) Ping(ctx context.Context) error { return s.rt.Ping(ctx) }

// PullImage pulls an image ref anonymously and returns its engine image id.
func (s *Service) PullImage(ctx context.Context, ref, platform string) (string, error) {
	return s.PullImageAuth(ctx, ref, platform, "")
}

// PullImageAuth pulls an image ref, optionally authenticating with a
// docker-style base64(JSON) registryAuth string (empty = anonymous), and
// returns its engine image id.
func (s *Service) PullImageAuth(ctx context.Context, ref, platform, registryAuth string) (string, error) {
	id, err := s.rt.PullImage(ctx, ref, runtime.PullOptions{Platform: platform}, registryAuth)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

// PushImage uploads a locally built image to the registry its ref names.
//
// A thin passthrough like PullImageAuth: what to push and under which
// credential are decisions the caller has already made, and core adds nothing
// by re-deciding them.
func (s *Service) PushImage(ctx context.Context, ref, registryAuth string) error {
	return s.rt.PushImage(ctx, ref, registryAuth)
}

// ResolveImageDigest queries the registry for ref's current content digest
// (M20a: watcher image-mode targets poll this to detect upstream changes).
func (s *Service) ResolveImageDigest(ctx context.Context, ref, registryAuth string) (string, error) {
	return s.rt.ResolveImageDigest(ctx, ref, registryAuth)
}

// LoadImage imports a `docker save` tarball and returns the loaded image ids.
// The reader is streamed straight through to the runtime adapter.
func (s *Service) LoadImage(ctx context.Context, tarball io.Reader) ([]string, error) {
	ids, err := s.rt.LoadImage(ctx, tarball)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out, nil
}

// BuildImage builds an image from a tar build context and returns its id.
// maxBuildLogBytes is how much of a build's output is kept.
//
// The TAIL, not the head: a build fails at the end, and the last screenful is
// almost always the whole diagnosis. Keeping the beginning would reliably
// preserve "Step 1/9 : FROM golang" and lose the compiler error.
//
// 64 KiB is roughly eight hundred lines — more than any failure needs, small
// enough to travel in a JSON response and sit in a row without thought.
const maxBuildLogBytes = 64 << 10

// BuildResult is what a build produced.
//
// Log is populated whether or not the build succeeded, and that is the point:
// the failure case is the one somebody needs it for.
type BuildResult struct {
	ImageID string
	Log     string
}

func (s *Service) BuildImage(ctx context.Context, buildCtx io.Reader, dockerfile string, tags []string) (BuildResult, error) {
	// The timeout is applied here rather than left to the caller's context.
	//
	// Without it a Dockerfile that never finishes — `RUN sleep infinity`, a
	// package manager waiting on a prompt — holds a build worker for as long as
	// the daemon runs. The watcher has a small, fixed pool of them, so a
	// handful of such repositories stop builds for everybody, and nothing in
	// any log says why.
	if s.buildLimits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.buildLimits.Timeout)
		defer cancel()
	}

	if err := s.checkFreeDisk(); err != nil {
		return BuildResult{}, err
	}

	log := newTailBuffer(maxBuildLogBytes)

	// Maintain the cache ceiling on every path, not only on success.
	//
	// The ordering argument below — the cache a build just filled is what the
	// next build of the same repository wants — says not to prune BEFORE a
	// build. It says nothing about a build that FAILED, and the failure path is
	// the one that matters: a Dockerfile that fills the disk fails, by
	// definition. Trimming only after a success therefore left the one disk
	// bound that can be enforced at all unenforced on exactly the path that
	// breaches it, which is the whole of the attack.
	//
	// Pruning keeps CacheReservedBytes rather than emptying the cache, so the
	// layers a half-finished build did produce survive inside the ceiling.
	defer s.pruneBuildCacheIfConfigured(ctx)

	id, err := s.rt.BuildImage(ctx, buildCtx, runtime.BuildOptions{
		Dockerfile: dockerfile,
		Tags:       tags,
		Labels:     map[string]string{LabelBuilt: "true"},
		LogSink:    log,
		// Limits are machine policy, set once when the daemon starts. They are
		// deliberately not part of the build request: a request comes from
		// whatever is deploying, and a limit something can raise for itself is
		// not a limit.
		MemoryBytes: s.buildLimits.MemoryBytes,
		CPUs:        s.buildLimits.CPUs,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// A named error so the API can report it as policy rather than as a
			// server fault. Without it the caller sees "internal error (500)"
			// and the only copy of the reason is in this daemon's log — which,
			// as the port-conflict case already established, just forces
			// somebody to go and read it.
			return BuildResult{Log: log.String()}, fmt.Errorf("%w after %s", ErrBuildTimeout, s.buildLimits.Timeout)
		}
		return BuildResult{Log: log.String()}, err
	}

	if err := s.checkImageSize(ctx, id, tags); err != nil {
		return BuildResult{Log: log.String()}, err
	}

	return BuildResult{ImageID: string(id), Log: log.String()}, nil
}

// checkFreeDisk refuses a build when the disk builds write to is too full.
// A check that cannot run is logged and skipped: refusing every build because
// statfs failed would be an outage of its own making.
func (s *Service) checkFreeDisk() error {
	l := s.buildLimits
	if l.MinFreeDisk <= 0 || l.FreeDisk == nil {
		return nil
	}
	free, total, err := l.FreeDisk()
	if err != nil || total == 0 {
		s.log.Warn("cannot check free disk before a build; building anyway", "err", err)
		return nil
	}
	if share := float64(free) / float64(total); share < l.MinFreeDisk {
		s.log.Error("build refused: low on disk", "free_bytes", free, "total_bytes", total, "min_free", l.MinFreeDisk)
		return fmt.Errorf("%w (%.0f%% free, builds need %.0f%%)", ErrLowDisk, share*100, l.MinFreeDisk*100)
	}
	return nil
}

// checkImageSize removes and refuses an image over ImageMaxBytes. The tags go
// first so the image is not left behind under the names the build gave it.
func (s *Service) checkImageSize(ctx context.Context, id runtime.ImageID, tags []string) error {
	if s.buildLimits.ImageMaxBytes <= 0 {
		return nil
	}
	images, err := s.rt.ListImages(ctx)
	if err != nil {
		s.log.Warn("cannot check the built image's size", "err", err)
		return nil
	}
	for _, img := range images {
		if img.ID != id || img.Size <= s.buildLimits.ImageMaxBytes {
			continue
		}
		for _, t := range tags {
			if rerr := s.rt.RemoveImage(ctx, t); rerr != nil {
				s.log.Warn("could not remove an oversized image", "tag", t, "err", rerr)
			}
		}
		return fmt.Errorf("%w: it is %s, and the limit is %s — keep build tools and caches out of the final image (a multi-stage Dockerfile, or a .dockerignore)",
			ErrImageTooLarge, humanBytes(img.Size), humanBytes(s.buildLimits.ImageMaxBytes))
	}
	return nil
}

// humanBytes is a size to one decimal in the largest unit that fits.
func humanBytes(b int64) string {
	const gb, mb = 1 << 30, 1 << 20
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	}
	return fmt.Sprintf("%.0f MB", float64(b)/mb)
}

// pruneBuildCacheTimeout bounds the prune call itself. It is not the build's
// budget: the prune runs after the build is over, including after the build was
// abandoned for taking too long.
const pruneBuildCacheTimeout = 2 * time.Minute

// pruneBuildCacheIfConfigured keeps the build cache under its ceiling.
//
// Build cache is how a machine's disk actually disappears: docker offers no
// per-build disk quota, so a ceiling maintained afterwards is the only bound
// that can be enforced at all.
//
// Failures are logged, never returned. The build succeeded; refusing to report
// that because housekeeping failed would turn a disk-space problem into a
// deployment failure.
func (s *Service) pruneBuildCacheIfConfigured(ctx context.Context) {
	if s.buildLimits.CacheReservedBytes <= 0 {
		return
	}

	// Detached from the build's context, and the detachment is the point of
	// this line rather than tidiness: the build whose cache most needs
	// trimming is the one that hit the timeout, and by then its context is
	// already cancelled. Passing it straight through would make the prune fail
	// instantly, log a warning nobody reads, and leave the disk exactly as
	// full — housekeeping that reports success while doing nothing, which is
	// the failure mode this repository keeps writing down.
	//
	// It keeps a deadline of its own so a wedged daemon cannot hold the
	// goroutine open indefinitely.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pruneBuildCacheTimeout)
	defer cancel()

	freed, err := s.rt.PruneBuildCache(ctx, s.buildLimits.CacheReservedBytes)
	if err != nil {
		s.log.Warn("could not prune build cache", "err", err)
		return
	}
	if freed > 0 {
		s.log.Info("pruned build cache", "freed_bytes", freed,
			"reserved_bytes", s.buildLimits.CacheReservedBytes)
	}
}

// ListImages returns every image known to the runtime (passthrough).
func (s *Service) ListImages(ctx context.Context) ([]runtime.ImageSummary, error) {
	return s.rt.ListImages(ctx)
}

// RemoveImage deletes a locally available image by ref/tag/id (passthrough;
// M18 retention policy consumer is qooq-watcher after a successful upsert).
func (s *Service) RemoveImage(ctx context.Context, ref string) error {
	return s.rt.RemoveImage(ctx, ref)
}

func newID() string { return ulid.Make().String() }

// reserveName atomically checks name uniqueness and persists the initial
// Pending record, so a concurrent Deploy with the same name sees it.
func (s *Service) reserveName(comp *Component) error {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	existing, err := s.store.ListComponents()
	if err != nil {
		return err
	}
	for _, c := range existing {
		if sameIdentity(c.Spec, comp.Spec) && c.Status.Phase != runtime.PhaseRemoved {
			return fmt.Errorf("%w: name %q is already used by component %s", ErrNameConflict, displayName(comp.Spec), c.ID)
		}
	}
	// Assigning host ports here, rather than in a pass of its own, is what
	// makes two simultaneous deploys unable to be handed the same port: this
	// is already the critical section that holds the only complete view of
	// what every component claims, and it ends by writing this one down.
	//
	// A reservation for this name (Reserve) is honoured as if it were this
	// component's previous deploy: the address other apps were already given
	// is the one it gets. Everyone else's reservations count as taken.
	reserved, all, err := s.reservedFor(comp.Spec)
	if err != nil {
		return err
	}
	if hasAutoPort(comp.Spec.Ports) {
		taken := takenHostPorts(existing, comp.ID)
		addReservedPorts(taken, all, comp.Spec)
		ports, err := resolveAutoPorts(comp.Spec.Ports, reserved, taken, s.autoPortLow, s.autoPortHigh)
		if err != nil {
			return err
		}
		comp.Spec.Ports = ports
	}
	if err := s.store.SaveComponent(comp); err != nil {
		return err
	}
	// The component now holds the ports itself; the promise is kept.
	if findReservation(all, comp.Spec) != nil {
		if err := s.store.DeleteReservation(comp.Spec.Namespace, comp.Spec.Name); err != nil {
			s.log.Warn("could not drop a used port reservation", "component", displayName(comp.Spec), "err", err)
		}
	}
	return nil
}

// resolveAutoPortsFor assigns host ports for a spec replacing an existing
// component, reusing whatever that component already had. Unlike the create
// path this cannot be folded into a single write, so it takes the same lock
// for the scan alone; a port that gets claimed in the gap surfaces as
// ErrPortConflict naming it, which is the same way any other port collision
// reports itself.
func (s *Service) resolveAutoPortsFor(spec ComponentSpec, prev *Component) (ComponentSpec, error) {
	if !hasAutoPort(spec.Ports) {
		return spec, nil
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	existing, err := s.store.ListComponents()
	if err != nil {
		return spec, err
	}
	_, all, err := s.reservedFor(spec)
	if err != nil {
		return spec, err
	}
	taken := takenHostPorts(existing, prev.ID)
	addReservedPorts(taken, all, spec)
	ports, err := resolveAutoPorts(spec.Ports, prev.Spec.Ports, taken, s.autoPortLow, s.autoPortHigh)
	if err != nil {
		return spec, err
	}
	spec.Ports = ports
	return spec, nil
}

// Deploy creates a component, creates+starts its container, and persists state.
func (s *Service) Deploy(ctx context.Context, spec ComponentSpec) (*Component, error) {
	spec, err := validateSpec(spec)
	if err != nil {
		return nil, err
	}
	return s.createComponent(ctx, spec)
}

// validateSpec applies the same request-time validation/normalization Deploy
// and Upsert both need: restart policy and health check. It never mutates its
// argument in place.
func validateSpec(spec ComponentSpec) (ComponentSpec, error) {
	if !validRestartPolicy(spec.RestartPolicy) {
		return spec, ErrInvalidRestartPolicy
	}
	// Materialize the restart-policy default so an omitted policy is stored and
	// shown as its concrete value ("always") rather than a blank that looks
	// unset. The restart manager treats "" the same way; this only makes the
	// default visible in component get / API responses.
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = RestartPolicyAlways
	}
	hc, err := normalizeHealthCheck(spec.HealthCheck, spec.Ports)
	if err != nil {
		return spec, err
	}
	spec.HealthCheck = hc
	// Fill in the namespace here rather than at the API boundary alone: "every
	// component has a namespace" is core's invariant, and the callers that do
	// not come through the API — orphan adoption, reading back a qooq.spec
	// label written before the default existed — need it just as much.
	spec.Namespace = client.NormalizeNamespace(spec.Namespace)
	if err := validateNaming(spec); err != nil {
		return spec, err
	}
	if err := validateMounts(spec.Mounts); err != nil {
		return spec, err
	}
	return spec, nil
}

// createComponent is the shared "brand-new component" path: allocate an id,
// reserve its name, create+start its container. Used by Deploy directly and
// by Upsert when no existing component owns the requested name. spec must
// already be validated/normalized by validateSpec.
func (s *Service) createComponent(ctx context.Context, spec ComponentSpec) (*Component, error) {
	id := newID()
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	comp := &Component{
		ID:        id,
		Spec:      spec,
		Status:    ComponentStatus{Phase: runtime.PhasePending, DesiredState: DesiredRunning},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.reserveName(comp); err != nil {
		return nil, err
	}

	if err := s.createAndStart(ctx, comp); err != nil {
		return nil, err
	}
	s.log.Info("deployed component", "id", id, "name", spec.Name, "container", comp.Status.ContainerID)
	return comp, nil
}

// findIDByName scans for a non-Removed component owning name, under deployMu
// (the same lock Deploy's reserveName uses for the uniqueness check), so it
// never races a concurrent Deploy/Upsert of the same name. It returns "" if
// no such component exists. The lock is released before the caller acts on
// the result — see Upsert's TOCTOU handling.
func (s *Service) findIDByName(namespace, name string) (string, error) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	comps, err := s.store.ListComponents()
	if err != nil {
		return "", err
	}
	for _, c := range comps {
		if c.Spec.Namespace == namespace && c.Spec.Name == name && c.Status.Phase != runtime.PhaseRemoved {
			return c.ID, nil
		}
	}
	return "", nil
}

// Upsert creates a component if no non-removed component owns spec.Name, or
// replaces an existing one's container in place (same ULID, new Spec) if one
// does. created reports which path was taken (true = 201, false = 200).
//
// Lock order: findIDByName takes only deployMu (released before returning).
// The replace path then takes the existing id's per-id lock and re-reads the
// record, so a concurrent Remove between the name scan and the lock acquire
// is caught (ErrNotFound) and falls back to createComponent — deployMu is
// never held while a per-id lock is held (or vice versa).
func (s *Service) Upsert(ctx context.Context, spec ComponentSpec) (comp *Component, created bool, err error) {
	spec, err = validateSpec(spec)
	if err != nil {
		return nil, false, err
	}

	existingID, err := s.findIDByName(spec.Namespace, spec.Name)
	if err != nil {
		return nil, false, err
	}
	if existingID == "" {
		comp, err := s.createComponent(ctx, spec)
		return comp, true, err
	}

	comp, err = s.replaceComponent(ctx, existingID, spec)
	if errors.Is(err, ErrNotFound) {
		// Removed between the name scan and taking the per-id lock: the name
		// is free again, so this is really a create.
		comp, err := s.createComponent(ctx, spec)
		return comp, true, err
	}
	return comp, false, err
}

// replaceComponent swaps id's container in place: stop+remove the old one
// (best-effort; it may already be gone), then create+start a new one from
// spec. The component keeps its ULID. Start failure marks the component
// Failed (via createAndStart) but is not rolled back, and PreviousImage stays
// set so the caller can see what it replaced. Returns ErrNotFound if id no
// longer exists (caller falls back to createComponent).
func (s *Service) replaceComponent(ctx context.Context, id string, spec ComponentSpec) (*Component, error) {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		return nil, err
	}
	comp, err = s.replaceComponentLocked(ctx, comp, spec)
	if err != nil {
		return nil, err
	}
	s.log.Info("upserted component", "id", id, "name", spec.Name, "container", comp.Status.ContainerID)
	return comp, nil
}

// replaceComponentLocked does the actual in-place container swap: stop+
// remove the old container (best-effort; it may already be gone), then
// create+start a new one from spec. The caller must already hold comp's
// per-id lock and have loaded comp fresh from the store. It is shared by
// replaceComponent (Upsert's replace path) and Rollback, since "replace this
// component's image with something else, same ULID" is exactly what both do
// — Rollback just picks spec.Image = comp.Status.PreviousImage instead of
// taking a caller-supplied spec.
func (s *Service) replaceComponentLocked(ctx context.Context, comp *Component, spec ComponentSpec) (*Component, error) {
	id := comp.ID

	// Resolve auto ports against what this component already holds, before the
	// old container is torn down: reusing its own port is only possible while
	// the previous spec is still the one on record.
	spec, err := s.resolveAutoPortsFor(spec, comp)
	if err != nil {
		return nil, err
	}

	comp.Status.PreviousImage = comp.Spec.Image
	if comp.Status.ContainerID != "" {
		cid := runtime.ContainerID(comp.Status.ContainerID)
		if err := s.rt.StopContainer(ctx, cid, stopTimeout); err != nil {
			s.log.Warn("replace: stop old container", "id", id, "container", cid, "err", err)
		}
		if err := s.rt.RemoveContainer(ctx, cid, true); err != nil {
			s.log.Warn("replace: remove old container", "id", id, "container", cid, "err", err)
		}
	}

	comp.Spec = spec
	comp.Status.ContainerID = ""
	comp.Status.DesiredState = DesiredRunning
	// A replace is a fresh deployment: forget any pending crash backoff and
	// give the crash-loop terminal state a clean slate, same as a manual
	// Start.
	comp.Status.RestartCount = 0
	comp.Status.BackoffUntil = time.Time{}
	comp.Status.RestartExhausted = false
	comp.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(comp); err != nil {
		return nil, err
	}

	if err := s.createAndStart(ctx, comp); err != nil {
		return nil, err
	}
	return comp, nil
}

// Rollback swaps a component's image back to Status.PreviousImage (the image
// it had before its last Upsert-replace or Rollback), via the same
// stop-old/create-new path as Upsert's replace. Because
// replaceComponentLocked always records the pre-swap image as the new
// PreviousImage, two consecutive Rollback calls exchange the pair back and
// forth: rollback once to go back a version, rollback again to undo that.
func (s *Service) Rollback(ctx context.Context, id string) (*Component, error) {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		return nil, err
	}
	if comp.Status.PreviousImage == "" {
		return nil, ErrNothingToRollback
	}

	spec := comp.Spec
	spec.Image = comp.Status.PreviousImage

	comp, err = s.replaceComponentLocked(ctx, comp, spec)
	if err != nil {
		return nil, err
	}
	s.log.Info("rolled back component", "id", id, "name", comp.Spec.Name,
		"image", comp.Spec.Image, "previous_image", comp.Status.PreviousImage)
	return comp, nil
}

// Logs returns a stream of the component's container output. It is a thin
// passthrough to the runtime adapter: id resolution only, no buffering — the
// per-id lock is deliberately NOT held here (same as Get/LatestStat), since a
// log stream can be long-lived (follow mode) and must never block other
// lifecycle operations on this component.
func (s *Service) Logs(ctx context.Context, id string, opts runtime.LogOptions) (io.ReadCloser, error) {
	comp, err := s.store.GetComponent(id)
	if err != nil {
		return nil, err
	}
	if comp.Status.ContainerID == "" {
		return nil, fmt.Errorf("component %s has no container", id)
	}
	return s.rt.Logs(ctx, runtime.ContainerID(comp.Status.ContainerID), opts)
}

// createAndStart is the single sequence that turns a component record into a
// running container: build the neutral spec, create, then start. It is
// shared by Deploy (first creation) and the restart manager's performRestart
// (crash/drift recreation), so there is exactly one place that issues
// CreateContainer+StartContainer. The caller must hold comp's per-id lock.
func (s *Service) createAndStart(ctx context.Context, comp *Component) error {
	// Recorded on the spec, like a resolved auto port: a stored component
	// says where it is actually published.
	comp.Spec.Ports = s.withPublishIP(comp.Spec.Ports)
	spec := comp.Spec
	labels := make(map[string]string, len(spec.Labels)+2)
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[LabelManaged] = "true"
	labels[LabelComponentID] = comp.ID
	// qooq.spec carries the full ComponentSpec so a daemon started against a
	// fresh/empty store can reconstruct this component from the container
	// alone (see adoptOrphan in reconcile.go). A marshal failure is not fatal
	// to Deploy — it only degrades this component's disaster-recovery story.
	if specJSON, err := json.Marshal(spec); err == nil {
		labels[LabelSpec] = string(specJSON)
	} else {
		s.log.Warn("marshal component spec for qooq.spec label (orphan adoption for this component would fail after data loss)",
			"id", comp.ID, "err", err)
	}
	if s.instance != "" {
		labels[LabelInstance] = s.instance
	}

	cspec := runtime.ContainerSpec{
		// The engine-level name is namespace-qualified so it stays unique
		// across the daemon; the address siblings use is the short alias
		// below.
		Name:           ContainerName(spec),
		Image:          spec.Image,
		Command:        spec.Command,
		Env:            spec.Env,
		Ports:          spec.Ports,
		Mounts:         spec.Mounts,
		Network:        spec.Network,
		NetworkAliases: networkAliases(spec),
		Labels:         labels,
		CPUNanos:       int64(spec.CPULimit * 1e9),
		MemBytes:       spec.MemLimitBytes,
	}
	s.applyContainerLimits(spec.Name, &cspec)

	comp.Status.Phase = runtime.PhaseCreating
	comp.UpdatedAt = time.Now()
	// This save only publishes progress — the authoritative writes are
	// markFailed and the success path below, both of which are checked. A
	// failure here must not abort the deploy, but silently dropping it means a
	// component stuck in an earlier phase has no explanation anywhere.
	if err := s.store.SaveComponent(comp); err != nil {
		s.log.Warn("save component while marking creating", "id", comp.ID, "err", err)
	}

	// Ensure the image is present. A pull failure is not fatal here: the image
	// may already exist locally (offline, or a load/build-produced id), in which
	// case CreateContainer still succeeds; a genuinely missing image surfaces as
	// a clear CreateContainer error below.
	if _, err := s.rt.PullImage(ctx, spec.Image, runtime.PullOptions{}, ""); err != nil {
		s.log.Warn("pull image before create (continuing, image may be local)",
			"image", spec.Image, "err", err)
	}

	// Ensure the network on every create rather than once at deploy time: a
	// restart-manager rebuild or a reconcile-driven recreate must also work
	// after someone removed the network by hand.
	if spec.Network != "" {
		if err := s.rt.EnsureNetwork(ctx, spec.Network); err != nil {
			wrapped := fmt.Errorf("ensure network %s: %w", spec.Network, err)
			s.markFailed(comp, wrapped)
			return wrapped
		}
	}

	cid, err := s.rt.CreateContainer(ctx, cspec)
	if err != nil {
		wrapped := fmt.Errorf("create container: %w", err)
		s.markFailed(comp, wrapped)
		return wrapped
	}
	comp.Status.ContainerID = string(cid)
	// A new container has not proved anything yet. Carrying the previous
	// container's HealthyAt forward would let a replacement be treated as
	// ready the instant it is created, which is exactly the window this
	// field exists to close.
	comp.Status.HealthyAt = time.Time{}

	if err := s.rt.StartContainer(ctx, cid); err != nil {
		wrapped := fmt.Errorf("start container: %w", err)
		s.markFailed(comp, wrapped)
		return wrapped
	}

	comp.Status.Phase = runtime.PhaseRunning
	comp.Status.LastError = ""
	// Stamped on every successful start, so a redeploy is what clears a stale
	// address — which is exactly the action the staleness flag asks for.
	comp.Status.AdvertiseHost = s.CurrentAdvertiseHost()
	comp.Status.StartedAt = time.Now()
	comp.UpdatedAt = time.Now()
	return s.store.SaveComponent(comp)
}

// maxLastErrorBytes caps ComponentStatus.LastError so a pathologically long
// runtime error (docker can echo whole build/daemon dumps) cannot bloat the
// bbolt record. The head of the message carries the diagnosis, so truncating
// the tail is safe.
const maxLastErrorBytes = 1000

// markFailed records the Failed phase together with the reason, so operators
// can read it back via `component get` rather than hunting through daemon
// logs. Pass a nil cause only when there genuinely is none.
func (s *Service) markFailed(comp *Component, cause error) {
	comp.Status.Phase = runtime.PhaseFailed
	// Nothing is running, so there is no start time to report. Left at the
	// last container's start, it made every failed recreate look like the end
	// of a minute-long stable run: EvaluateRestart reset the count to zero each
	// time, so a component whose container cannot be created (a bind source
	// gone) retried every reconcile tick forever, with no backoff and never
	// reaching RestartExhausted.
	comp.Status.StartedAt = time.Time{}
	if cause != nil {
		msg := cause.Error()
		if len(msg) > maxLastErrorBytes {
			msg = msg[:maxLastErrorBytes] + "… (truncated)"
		}
		comp.Status.LastError = msg
	}
	comp.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(comp); err != nil {
		s.log.Error("persist failed state", "id", comp.ID, "err", err)
	}
}

// Start (re)starts the component's existing container.
func (s *Service) Start(ctx context.Context, id string) (*Component, error) {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		return nil, err
	}
	if comp.Status.ContainerID == "" {
		return nil, fmt.Errorf("component %s has no container to start", id)
	}
	if err := s.rt.StartContainer(ctx, runtime.ContainerID(comp.Status.ContainerID)); err != nil {
		return nil, err
	}
	comp.Status.Phase = runtime.PhaseRunning
	comp.Status.DesiredState = DesiredRunning
	comp.Status.LastError = ""
	comp.Status.StartedAt = time.Now()
	// A manual start is a fresh start: forget any pending crash backoff and
	// clear the crash-loop terminal state, giving it a fresh run of attempts.
	comp.Status.RestartCount = 0
	comp.Status.BackoffUntil = time.Time{}
	comp.Status.RestartExhausted = false
	comp.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(comp); err != nil {
		return nil, err
	}
	return comp, nil
}

// Stop gracefully stops the component's container.
func (s *Service) Stop(ctx context.Context, id string) (*Component, error) {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		return nil, err
	}
	if comp.Status.ContainerID == "" {
		return nil, fmt.Errorf("component %s has no container to stop", id)
	}
	if err := s.rt.StopContainer(ctx, runtime.ContainerID(comp.Status.ContainerID), stopTimeout); err != nil {
		return nil, err
	}
	comp.Status.Phase = runtime.PhaseExited
	comp.Status.DesiredState = DesiredStopped
	comp.Status.FinishedAt = time.Now()
	comp.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(comp); err != nil {
		return nil, err
	}
	return comp, nil
}

// Remove force-removes the container (if any) and deletes the component record.
func (s *Service) Remove(ctx context.Context, id string) error {
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(id)
	if err != nil {
		return err
	}
	if comp.Status.ContainerID != "" {
		if err := s.rt.RemoveContainer(ctx, runtime.ContainerID(comp.Status.ContainerID), true); err != nil {
			// The container may already be gone; log and proceed to drop the record.
			s.log.Warn("remove container", "id", id, "container", comp.Status.ContainerID, "err", err)
		}
	}
	// The network createAndStart ensured goes too once nothing is left on it;
	// otherwise every deploy-and-remove leaves one behind until docker runs
	// out of address pools. Best effort: a leftover network is not a reason
	// to keep a component the caller asked to remove.
	if comp.Spec.Network != "" {
		if err := s.rt.RemoveNetworkIfUnused(ctx, comp.Spec.Network); err != nil {
			s.log.Warn("remove network", "id", id, "network", comp.Spec.Network, "err", err)
		}
	}
	if err := s.store.DeleteComponent(id); err != nil {
		return err
	}
	// Drop per-component bookkeeping so a long-running daemon does not
	// accumulate entries for removed components.
	//
	// NOTHING MAY BE ADDED AFTER THIS DELETE. Removing the entry while the lock
	// is still held means a concurrent lockFor(id) gets a fresh, uncontended
	// mutex and runs alongside whatever follows — mutual exclusion for this id
	// ends here, not at the deferred Unlock. That is harmless only because the
	// component is already deleted from the store and nothing below touches it.
	// Fixing it properly needs refcounted locks, which is disproportionate for
	// a window with nothing in it; keeping the window empty is the invariant.
	s.mu.Lock()
	delete(s.locks, id)
	s.mu.Unlock()
	s.statsMu.Lock()
	delete(s.latestStats, id)
	s.statsMu.Unlock()
	return nil
}

// Get returns a single component from the store.
func (s *Service) Get(ctx context.Context, id string) (*Component, error) {
	return s.store.GetComponent(id)
}

// List returns all components from the store.
func (s *Service) List(ctx context.Context) ([]*Component, error) {
	return s.store.ListComponents()
}

// HandleEvent applies a neutral runtime lifecycle event to the addressed
// component: it updates the persisted phase/crash count and appends an event
// record. It is called by the monitor's events goroutine. Events for
// containers belonging to a different qooqd instance, with no
// qooq.component_id label, or for unknown components, are ignored. The
// per-component mutex serializes this against HTTP lifecycle ops.
func (s *Service) HandleEvent(ctx context.Context, ev runtime.Event) {
	if !s.OwnsContainer(ev.Labels) {
		return
	}
	cid := ev.Labels[LabelComponentID]
	if cid == "" {
		return
	}

	lock := s.lockFor(cid)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(cid)
	if err != nil {
		if !errorsIsNotFound(err) {
			s.log.Warn("handle event: load component", "component", cid, "err", err)
		}
		return
	}

	now := time.Now()
	var evType, msg string

	switch ev.Action {
	case "start":
		comp.Status.Phase = runtime.PhaseRunning
		comp.Status.LastError = ""
		comp.Status.StartedAt = now
		evType = "started"
	case "die":
		if comp.Status.ContainerID != "" && comp.Status.ContainerID != string(ev.ContainerID) {
			// Stale event for a container we've already replaced (e.g. a
			// health-triggered force-remove of the old container racing
			// with its own die event after a new container is already
			// running). Record it for visibility but do not let it
			// clobber the current record's phase/crash bookkeeping.
			if err := s.store.AppendEvent(cid, Event{
				ComponentID: cid,
				Type:        "died_stale",
				Message:     fmt.Sprintf("stale die event for superseded container %s (exit %d)", ev.ContainerID, ev.ExitCode),
				ExitCode:    ev.ExitCode,
				Time:        now,
			}); err != nil {
				s.log.Error("handle event: append stale die event", "component", cid, "err", err)
			}
			return
		}
		comp.Status.LastExitCode = ev.ExitCode
		comp.Status.FinishedAt = now
		switch {
		case comp.Status.DesiredState == DesiredStopped:
			// Operator-initiated stop: an app that ignores SIGTERM often
			// exits non-zero (137/143) once SIGKILL lands. That is expected
			// behavior for a requested stop, not a crash.
			comp.Status.Phase = runtime.PhaseExited
			evType = "exited"
			msg = "container exited after requested stop"
		case ev.ExitCode != 0:
			comp.Status.Phase = runtime.PhaseFailed
			comp.Status.CrashCount++
			evType = "died"
			msg = fmt.Sprintf("container exited with code %d", ev.ExitCode)
		default:
			comp.Status.Phase = runtime.PhaseExited
			evType = "exited"
			msg = "container exited cleanly"
		}
	case "oom":
		evType = "oom"
		msg = oomMessage(comp.Spec.MemLimitBytes)
		// Into LastError as well as the event log: without it the phase says
		// Failed and nothing says why, and "it hit its memory limit" is the
		// one thing the owner can act on. The die that follows does not
		// clear it; only the next successful start does.
		comp.Status.LastError = msg
	case "stop":
		evType = "stopped"
	default:
		return
	}

	comp.UpdatedAt = now
	if err := s.store.SaveComponent(comp); err != nil {
		s.log.Error("handle event: persist component", "component", cid, "err", err)
	}
	if err := s.store.AppendEvent(cid, Event{
		ComponentID: cid,
		Type:        evType,
		Message:     msg,
		ExitCode:    ev.ExitCode,
		Time:        now,
	}); err != nil {
		s.log.Error("handle event: append event", "component", cid, "err", err)
	}
}

// oomMessage says the container was killed for memory, and at what limit.
func oomMessage(limit int64) string {
	if limit <= 0 {
		return "out of memory: the container was killed by the system"
	}
	return fmt.Sprintf("out of memory: it used its whole memory limit (%dMi) and was stopped — raise limits.memory in qooq.yaml, or use less", limit>>20)
}

// RecordStat caches the latest resource-usage sample for a component. Called by
// the monitor's per-container stats goroutine.
func (s *Service) RecordStat(componentID string, sample runtime.StatSample) {
	s.statsMu.Lock()
	s.latestStats[componentID] = sample
	s.statsMu.Unlock()
}

// LatestStat returns the most recent sample for a component, if one exists.
func (s *Service) LatestStat(componentID string) (runtime.StatSample, bool) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	sample, ok := s.latestStats[componentID]
	return sample, ok
}

// NoteHealthUnreachable records that a component's health probe has never once
// succeeded, so the monitor is declining to restart it.
//
// It is a separate entry point rather than a general "append an event" method
// because the distinction it records is a decision, not an observation: a probe
// that has never worked is evidence about the probe, not about the app, and
// acting on it would restart a healthy component forever. Written to the event
// log so it is visible from `qooq component get` — a daemon log line is not
// somewhere an operator thinks to look when their app "just keeps restarting".
func (s *Service) NoteHealthUnreachable(ctx context.Context, componentID, detail string) {
	if _, err := s.store.GetComponent(componentID); err != nil {
		return
	}
	if err := s.store.AppendEvent(componentID, Event{
		ComponentID: componentID,
		Type:        "health_unreachable",
		Message:     detail,
		Time:        time.Now().UTC(),
	}); err != nil {
		s.log.Error("append health_unreachable event", "id", componentID, "err", err)
	}
}

// NoteHealthy records that a container's health probe has passed for the
// first time, so "this component is ready" becomes a question anything can
// answer — the watcher waiting to deploy an app against its database, a
// stack waiting on depends_on — and not just the checker goroutine that
// happens to hold the tracker.
//
// Writes once per container and then stops: the caller only reports the
// transition, and a set HealthyAt short-circuits the rest. That matters
// because this runs from the monitor, and a store write every probe interval
// for every component would be a steady cost for a value that never changes
// after the first success.
//
// containerID is checked against the record under the lock because a checker
// outlives the container it was started for by however long it takes to
// notice: without this, a probe result from the container an Upsert just
// replaced would mark the new one ready before it has run.
func (s *Service) NoteHealthy(ctx context.Context, componentID, containerID string) {
	lock := s.lockFor(componentID)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(componentID)
	if err != nil {
		return
	}
	if comp.Status.ContainerID != containerID || !comp.Status.HealthyAt.IsZero() {
		return
	}
	comp.Status.HealthyAt = time.Now().UTC()
	if err := s.store.SaveComponent(comp); err != nil {
		s.log.Error("record healthy_at", "id", componentID, "err", err)
	}
}

// NotePanicRecovered records that a panic was caught and recovered while
// processing something for this component — a lifecycle event, a scheduled
// restart — so the fact that it happened is visible from `qooq component get`
// and not just a daemon log line an operator was not watching at the moment
// it fired. source names what was being processed (e.g. "event dispatch
// (start)", "restart (crash)"); r is the recovered panic value.
//
// componentID may not resolve to anything (a panic can happen with a
// component id that no longer exists, or none at all); in that case there is
// nothing to attach the event to, and the caller's own ERROR log is the only
// durable record — which remains the caller's responsibility regardless of
// what this method does.
func (s *Service) NotePanicRecovered(ctx context.Context, componentID, source string, r any) {
	if componentID == "" {
		return
	}
	if _, err := s.store.GetComponent(componentID); err != nil {
		return
	}
	if err := s.store.AppendEvent(componentID, Event{
		ComponentID: componentID,
		Type:        "panic_recovered",
		Message:     fmt.Sprintf("recovered panic during %s: %v", source, r),
		Time:        time.Now().UTC(),
	}); err != nil {
		s.log.Error("append panic_recovered event", "id", componentID, "err", err)
	}
}

// ListEvents returns the persisted event log for a component (oldest first).
func (s *Service) ListEvents(ctx context.Context, id string) ([]Event, error) {
	if _, err := s.store.GetComponent(id); err != nil {
		return nil, err
	}
	return s.store.ListEvents(id)
}

func errorsIsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
