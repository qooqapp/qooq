package core

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// fakeStore is an in-memory Store implementation guarded by a mutex, used to
// drive Service tests without a real bbolt file. GetComponent/ListComponents
// return deep-enough copies (via value semantics on Component structs stored
// by pointer to a freshly cloned struct) so callers mutating the returned
// pointer do not corrupt the store's internal state unless they explicitly
// SaveComponent it back — mirroring the real bbolt store's marshal/unmarshal
// round trip.
type fakeStore struct {
	mu         sync.Mutex
	components map[string]*Component
	events     map[string][]Event
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		components: make(map[string]*Component),
		events:     make(map[string][]Event),
	}
}

func cloneComponent(c *Component) *Component {
	cp := *c
	return &cp
}

func (f *fakeStore) SaveComponent(c *Component) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.components[c.ID] = cloneComponent(c)
	return nil
}

func (f *fakeStore) GetComponent(id string) (*Component, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.components[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneComponent(c), nil
}

func (f *fakeStore) ListComponents() ([]*Component, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Component, 0, len(f.components))
	for _, c := range f.components {
		out = append(out, cloneComponent(c))
	}
	return out, nil
}

func (f *fakeStore) DeleteComponent(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.components[id]; !ok {
		return ErrNotFound
	}
	delete(f.components, id)
	return nil
}

func (f *fakeStore) SaveGroup(g *Group) error { return nil }
func (f *fakeStore) GetGroup(id string) (*Group, error) {
	return nil, ErrNotFound
}
func (f *fakeStore) ListGroups() ([]*Group, error) { return nil, nil }
func (f *fakeStore) DeleteGroup(id string) error   { return nil }

func (f *fakeStore) AppendEvent(componentID string, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[componentID] = append(f.events[componentID], e)
	return nil
}

func (f *fakeStore) ListEvents(componentID string) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.events[componentID]))
	copy(out, f.events[componentID])
	return out, nil
}

func (f *fakeStore) Backup(w io.Writer, sizeHint func(int64)) error {
	if sizeHint != nil {
		sizeHint(int64(len("fake-backup")))
	}
	_, err := io.WriteString(w, "fake-backup")
	return err
}

func (f *fakeStore) Close() error { return nil }

// call records a single invocation of a fakeRuntime method for assertion.
type call struct {
	method string
	arg    string
}

// fakeRuntime is an in-memory runtime.Runtime implementation. Behavior is
// controlled by injectable function fields (nil means "succeed with a
// default"); every call is also appended to calls for sequence assertions.
type fakeRuntime struct {
	// Build policy observation, for the build-limit tests.
	lastBuild runtime.BuildOptions
	// builtSize, when set, is the size ListImages reports for the image
	// BuildImage returns ("built").
	builtSize    int64
	buildBlocks  bool
	pruneCalls   int
	lastReserved int64
	pruneErr     error
	// buildFails models a Dockerfile that fails, which is what one filling the
	// disk does: it ends with "No space left on device" and a non-zero status.
	buildFails error
	// pruneCtxErr is the state of the context the prune was handed. A cancelled
	// one means the prune could not have done anything, so counting calls alone
	// would certify a no-op.
	pruneCtxErr error

	mu    sync.Mutex
	calls []call

	nextContainerID int

	// Injectable hooks. All are optional.
	CreateContainerFunc    func(spec runtime.ContainerSpec) (runtime.ContainerID, error)
	StartContainerFunc     func(id runtime.ContainerID) error
	StopContainerFunc      func(id runtime.ContainerID) error
	RemoveContainerFunc    func(id runtime.ContainerID) error
	InspectContainerFunc   func(id runtime.ContainerID) (runtime.ContainerStatus, error)
	ListContainersFunc     func(filter runtime.Filter) ([]runtime.ContainerStatus, error)
	PullImageFunc          func(ref string) (runtime.ImageID, error)
	ResolveImageDigestFunc func(ref string) (string, error)
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{}
}

func (f *fakeRuntime) record(method, arg string) {
	f.mu.Lock()
	f.calls = append(f.calls, call{method: method, arg: arg})
	f.mu.Unlock()
}

func (f *fakeRuntime) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string, opts runtime.PullOptions, registryAuth string) (runtime.ImageID, error) {
	f.record("PullImage", ref)
	if f.PullImageFunc != nil {
		return f.PullImageFunc(ref)
	}
	return runtime.ImageID(ref), nil
}

func (f *fakeRuntime) ResolveImageDigest(ctx context.Context, ref string, registryAuth string) (string, error) {
	f.record("ResolveImageDigest", ref)
	if f.ResolveImageDigestFunc != nil {
		return f.ResolveImageDigestFunc(ref)
	}
	return "sha256:fake", nil
}

func (f *fakeRuntime) PushImage(ctx context.Context, ref string, registryAuth string) error {
	return nil
}

func (f *fakeRuntime) PruneBuildCache(ctx context.Context, reservedBytes int64) (uint64, error) {
	f.pruneCalls++
	f.lastReserved = reservedBytes
	f.pruneCtxErr = ctx.Err()
	if f.pruneErr != nil {
		return 0, f.pruneErr
	}
	return 0, nil
}

func (f *fakeRuntime) LoadImage(ctx context.Context, tarball io.Reader) ([]runtime.ImageID, error) {
	f.record("LoadImage", "")
	return nil, nil
}

func (f *fakeRuntime) BuildImage(ctx context.Context, buildCtx io.Reader, opts runtime.BuildOptions) (runtime.ImageID, error) {
	f.record("BuildImage", "")
	f.lastBuild = opts
	// buildBlocks models a Dockerfile that never finishes: it returns only when
	// the caller's context ends, which is what the timeout has to produce.
	if f.buildBlocks {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.buildFails != nil {
		return "", f.buildFails
	}
	if f.builtSize > 0 {
		return "built", nil
	}
	return "", nil
}

func (f *fakeRuntime) ListImages(ctx context.Context) ([]runtime.ImageSummary, error) {
	f.record("ListImages", "")
	if f.builtSize > 0 {
		return []runtime.ImageSummary{{ID: "built", Size: f.builtSize}}, nil
	}
	return nil, nil
}

func (f *fakeRuntime) RemoveImage(ctx context.Context, ref string) error {
	f.record("RemoveImage", ref)
	return nil
}

func (f *fakeRuntime) EnsureNetwork(ctx context.Context, name string) error {
	f.record("EnsureNetwork", name)
	return nil
}

func (f *fakeRuntime) DisconnectNetwork(ctx context.Context, id runtime.ContainerID, network string) error {
	f.record("DisconnectNetwork", string(id)+"@"+network)
	return nil
}

func (f *fakeRuntime) ConnectNetwork(ctx context.Context, id runtime.ContainerID, network string, aliases []string) error {
	f.record("ConnectNetwork", network)
	f.record("ConnectNetworkAs", string(id)+"@"+network)
	return nil
}

func (f *fakeRuntime) RemoveNetworkIfUnused(ctx context.Context, name string) error {
	f.record("RemoveNetworkIfUnused", name)
	return nil
}

// callArgs is the argument of every call to method, in order.
func (f *fakeRuntime) callArgs(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.arg)
		}
	}
	return out
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	f.record("CreateContainer", spec.Name)
	if f.CreateContainerFunc != nil {
		return f.CreateContainerFunc(spec)
	}
	f.mu.Lock()
	f.nextContainerID++
	id := f.nextContainerID
	f.mu.Unlock()
	return runtime.ContainerID(itoa(id)), nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	f.record("StartContainer", string(id))
	if f.StartContainerFunc != nil {
		return f.StartContainerFunc(id)
	}
	return nil
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	f.record("StopContainer", string(id))
	if f.StopContainerFunc != nil {
		return f.StopContainerFunc(id)
	}
	return nil
}

func (f *fakeRuntime) RemoveContainer(ctx context.Context, id runtime.ContainerID, force bool) error {
	f.record("RemoveContainer", string(id))
	if f.RemoveContainerFunc != nil {
		return f.RemoveContainerFunc(id)
	}
	return nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerStatus, error) {
	f.record("InspectContainer", string(id))
	if f.InspectContainerFunc != nil {
		return f.InspectContainerFunc(id)
	}
	return runtime.ContainerStatus{ID: id, Phase: runtime.PhaseRunning}, nil
}

func (f *fakeRuntime) ListContainers(ctx context.Context, filter runtime.Filter) ([]runtime.ContainerStatus, error) {
	f.record("ListContainers", "")
	if f.ListContainersFunc != nil {
		return f.ListContainersFunc(filter)
	}
	return nil, nil
}

func (f *fakeRuntime) Exec(ctx context.Context, id runtime.ContainerID, cmd []string) (int, error) {
	f.record("Exec", string(id))
	return 0, nil
}

func (f *fakeRuntime) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	f.record("Logs", string(id))
	return io.NopCloser(nil), nil
}

func (f *fakeRuntime) Events(ctx context.Context, filter runtime.Filter) (<-chan runtime.Event, <-chan error) {
	f.record("Events", "")
	return nil, nil
}

func (f *fakeRuntime) Stats(ctx context.Context, id runtime.ContainerID) (<-chan runtime.StatSample, <-chan error) {
	f.record("Stats", string(id))
	return nil, nil
}

func (f *fakeRuntime) Ping(ctx context.Context) error {
	f.record("Ping", "")
	return nil
}

func (f *fakeRuntime) Close() error {
	f.record("Close", "")
	return nil
}

// itoa avoids importing strconv just for this tiny id formatter.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
