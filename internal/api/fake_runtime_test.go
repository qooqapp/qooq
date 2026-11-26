package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// fakeRuntime is a minimal, directly-controllable runtime.Runtime used only
// to force specific core.Service error paths (host port conflicts, image
// not-found/in-use) that internal/runtime/process cannot be made to produce
// from an HTTP test (it has no notion of a taken host port or a registry
// image). Every method not overridden by a hook succeeds with a harmless
// default, so a test sets only the hook it needs and the rest of a
// deploy/start/stop sequence goes through unremarkably.
type fakeRuntime struct {
	// startErr, when non-nil, is returned by StartContainer.
	startErr error
	// removeImageErr, when non-nil, is returned by RemoveImage.
	removeImageErr error
	// listImagesErr, when non-nil, is returned by ListImages (used to force
	// the generic-5xx path).
	listImagesErr error
	// pingErr, when non-nil, is returned by Ping: the runtime is down.
	pingErr error
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string, opts runtime.PullOptions, registryAuth string) (runtime.ImageID, error) {
	return runtime.ImageID("fake-image-id"), nil
}

func (f *fakeRuntime) LoadImage(ctx context.Context, tarball io.Reader) ([]runtime.ImageID, error) {
	return nil, errors.New("fakeRuntime: LoadImage not supported")
}

func (f *fakeRuntime) BuildImage(ctx context.Context, buildCtx io.Reader, opts runtime.BuildOptions) (runtime.ImageID, error) {
	return "", errors.New("fakeRuntime: BuildImage not supported")
}

func (f *fakeRuntime) ListImages(ctx context.Context) ([]runtime.ImageSummary, error) {
	if f.listImagesErr != nil {
		return nil, f.listImagesErr
	}
	return nil, nil
}

func (f *fakeRuntime) ResolveImageDigest(ctx context.Context, ref, registryAuth string) (string, error) {
	return "", errors.New("fakeRuntime: ResolveImageDigest not supported")
}

func (f *fakeRuntime) PushImage(ctx context.Context, ref, registryAuth string) error {
	return errors.New("fakeRuntime: PushImage not supported")
}

func (f *fakeRuntime) PruneBuildCache(ctx context.Context, reservedBytes int64) (uint64, error) {
	return 0, nil
}

func (f *fakeRuntime) RemoveImage(ctx context.Context, ref string) error {
	return f.removeImageErr
}

func (f *fakeRuntime) EnsureNetwork(ctx context.Context, name string) error         { return nil }
func (f *fakeRuntime) RemoveNetworkIfUnused(ctx context.Context, name string) error { return nil }

func (f *fakeRuntime) DisconnectNetwork(ctx context.Context, id runtime.ContainerID, network string) error {
	return nil
}

func (f *fakeRuntime) ConnectNetwork(ctx context.Context, id runtime.ContainerID, network string, aliases []string) error {
	return nil
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	return runtime.ContainerID("fake-container-id"), nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	return f.startErr
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	return nil
}

func (f *fakeRuntime) RemoveContainer(ctx context.Context, id runtime.ContainerID, force bool) error {
	return nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerStatus, error) {
	return runtime.ContainerStatus{ID: id, Phase: runtime.PhaseRunning}, nil
}

func (f *fakeRuntime) ListContainers(ctx context.Context, filter runtime.Filter) ([]runtime.ContainerStatus, error) {
	return nil, nil
}

func (f *fakeRuntime) Exec(ctx context.Context, id runtime.ContainerID, cmd []string) (int, error) {
	return 0, nil
}

func (f *fakeRuntime) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeRuntime) Events(ctx context.Context, filter runtime.Filter) (<-chan runtime.Event, <-chan error) {
	ev := make(chan runtime.Event)
	errc := make(chan error)
	close(ev)
	close(errc)
	return ev, errc
}

func (f *fakeRuntime) Stats(ctx context.Context, id runtime.ContainerID) (<-chan runtime.StatSample, <-chan error) {
	s := make(chan runtime.StatSample)
	errc := make(chan error)
	close(s)
	close(errc)
	return s, errc
}

func (f *fakeRuntime) Ping(ctx context.Context) error { return f.pingErr }
func (f *fakeRuntime) Close() error                   { return nil }
