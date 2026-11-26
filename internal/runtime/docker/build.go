package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"

	"github.com/qooqapp/qooq/internal/runtime"
)

// BuildImage builds an image from a tar build context (docker build). A
// failed build still returns HTTP 200; the error is only visible inside the
// JSON progress stream, so the stream must be fully consumed and inspected
// for {"error"/"errorDetail"} entries before declaring success.
func (a *Adapter) BuildImage(ctx context.Context, buildCtx io.Reader, opts runtime.BuildOptions) (runtime.ImageID, error) {
	dockerfile := opts.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	// A BuildID lets the daemon-side build be cancelled.
	//
	// Without it, abandoning the request does NOT stop the build: the classic
	// builder keeps going after a client disconnect, so a timeout freed qooq's
	// build worker while the build step container ran on to its own completion —
	// measured at eight minutes past the point the build was reported failed,
	// plus a fresh one for every retry. BuildCancel is docker's own answer to
	// this, and it needs an id agreed up front.
	buildID, err := newBuildID()
	if err != nil {
		return "", fmt.Errorf("build image: %w", err)
	}

	bo := build.ImageBuildOptions{
		Tags:       opts.Tags,
		Dockerfile: dockerfile,
		Labels:     opts.Labels,
		Remove:     true,
		// ForceRemove too (docker build --force-rm): Remove alone clears the
		// step containers only when the build succeeds, so every failed build
		// left its last step behind with whatever it wrote — measured, a step
		// that wrote 3 GB and failed kept all 3 GB, and the retry after it
		// would leave another. Failed builds are exactly the ones strangers'
		// repos produce most.
		ForceRemove: true,
		Memory:      opts.MemoryBytes,
		BuildID:     buildID,
	}
	// MemorySwap equal to Memory disables swap for the build.
	//
	// Left at zero, docker grants swap equal to memory again on top — so a
	// 512MB limit lets a build use 1GB, and a build that should have been
	// killed instead swaps the machine to a standstill, which is worse for
	// everyone else than the OOM would have been.
	if opts.MemoryBytes > 0 {
		bo.MemorySwap = opts.MemoryBytes
	}
	if opts.CPUs > 0 {
		// docker expresses a CPU cap as quota within a period; 100ms is the
		// convention its own --cpus flag uses.
		bo.CPUPeriod = 100000
		bo.CPUQuota = int64(opts.CPUs * 100000)
	}

	// Ask the daemon to stop as soon as our context ends, and stop asking once
	// the build returns on its own.
	finished := make(chan struct{})
	defer close(finished)
	// cancelErr records why a cancel failed, so the caller can say so instead of
	// reporting a clean timeout while a build keeps running. The first version
	// discarded it with `_ = err`, which made the one question worth asking —
	// "did the cancel actually work?" — unanswerable.
	var cancelMu sync.Mutex
	var cancelErr error
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
			// A fresh context: the one that just expired cannot carry the
			// cancel request, and without a deadline of its own this goroutine
			// could outlive the process it is tidying up after.
			cctx, ccancel := context.WithTimeout(context.Background(), buildCancelTimeout)
			defer ccancel()
			err := a.cli.BuildCancel(cctx, buildID)
			cancelMu.Lock()
			cancelErr = err
			cancelMu.Unlock()
		}
	}()

	resp, err := a.cli.ImageBuild(ctx, buildCtx, bo)
	if err != nil {
		return "", fmt.Errorf("build image: %w", err)
	}
	defer resp.Body.Close()

	// stepContainer tracks the container the current step is running in, taken
	// from the builder's own "---> Running in <id>" line. It is the only handle
	// there is on a step in flight.
	var stepContainer string
	id, err := drainBuildStreamTracking(resp.Body, opts.LogSink, &stepContainer)
	if err != nil && ctx.Err() != nil {
		// The build was abandoned. Whether the daemon actually stopped is the
		// difference between a bounded failure and one that keeps consuming the
		// machine, so it goes in the error rather than nowhere.
		cancelMu.Lock()
		ce := cancelErr
		cancelMu.Unlock()

		// BuildCancel stops the build BETWEEN steps; the classic builder checks
		// the flag at step boundaries and cannot interrupt a step already
		// running. A single long RUN therefore survived the cancel and kept the
		// machine busy — the one gap left after BuildID was introduced.
		//
		// Measured, not reasoned: with this removal disabled, a build of
		// `RUN echo ... && sleep 600` under a 25s timeout left the step
		// container "Up" — the same id the build log names in its
		// "Running in" line — after the build had been reported failed. With
		// it, nothing survives.
		//
		// The step's container is killed directly instead. Its id comes from
		// the builder's own progress stream, so it is certainly this build's
		// and no guess is involved.
		//
		// BuildKit would handle this natively, and was not chosen: it would add
		// moby/buildkit — grpc, containerd, the rest — to a project that counts
		// its dependencies, to fix one case that a container removal already
		// covers. If the stream's wording ever changes, this degrades to the
		// behaviour before it, rather than breaking anything.
		if killErr := a.killBuildStep(stepContainer); killErr != nil {
			return "", fmt.Errorf("%w (and the running build step could not be stopped: %v — it may still be consuming the machine)", err, killErr)
		}
		if ce != nil {
			return "", fmt.Errorf("%w (and cancelling the daemon-side build failed: %v — it may still be running)", err, ce)
		}
	}
	if err != nil {
		return "", fmt.Errorf("build image: %w", err)
	}
	if id == "" {
		return "", errors.New("build image: no image id reported")
	}
	return runtime.ImageID(id), nil
}

// killBuildStep force-removes the container a cancelled build step was running
// in.
//
// Nothing to do when no step was in flight, which is the ordinary case: a build
// cancelled between steps has already had its container removed by the builder.
// A container that has since exited is likewise not an error — the step
// finished on its own in the moment between the cancel and this call.
func (a *Adapter) killBuildStep(containerID string) error {
	if containerID == "" {
		return nil
	}
	// A fresh context for the same reason the cancel goroutine uses one: the
	// caller's has already expired, and this is the tidying up after it.
	ctx, cancel := context.WithTimeout(context.Background(), buildCancelTimeout)
	defer cancel()

	err := a.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// runningInLine matches the classic builder's announcement of the container a
// step is executing in:
//
//	---> Running in 18b4c94eb8bb
//
// Parsing human-readable output is not something to do lightly. It is worth it
// here because this line is the only place the daemon names the container while
// the step is in flight, and because being wrong costs nothing: an id that does
// not match removes nothing, and the result is the behaviour that existed
// before.
var runningInLine = regexp.MustCompile(`(?m)^\s*--->\s+Running in ([0-9a-f]{8,64})\s*$`)

// drainBuildStream consumes the JSON progress stream from ImageBuild and
// returns the final image ID reported via the "aux" side-channel. It returns
// an error as soon as an error line is observed, but keeps draining the rest
// of the caller's defer-closed body regardless.
//
// sink, when non-nil, receives the human-readable half of the stream — the
// "Step 3/9", the compiler output, the failing command. That is the only copy
// there will ever be: docker does not keep it, and once this function returns
// the connection is closed.
//
// The error line is written to the sink as well. It is the most important line
// in the whole log, and a reader looking at the output to find out what went
// wrong should not have to be told separately.
func drainBuildStream(r io.Reader, sink io.Writer) (string, error) {
	return drainBuildStreamTracking(r, sink, nil)
}

// drainBuildStreamTracking is drainBuildStream plus the id of the step
// currently executing, written through stepContainer as the stream reports it.
func drainBuildStreamTracking(r io.Reader, sink io.Writer, stepContainer *string) (string, error) {
	var imageID string
	dec := json.NewDecoder(r)
	for {
		var msg progressMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode response: %w", err)
		}
		if stepContainer != nil && msg.Stream != "" {
			if m := runningInLine.FindStringSubmatch(msg.Stream); m != nil {
				*stepContainer = m[1]
			}
		}
		if sink != nil && msg.Stream != "" {
			// Write errors are ignored on purpose: the sink is a bounded
			// buffer, and failing a build because its log could not be
			// recorded would turn an observability feature into an outage.
			_, _ = io.WriteString(sink, msg.Stream)
		}
		if msgErr := msg.err(); msgErr != "" {
			if sink != nil {
				_, _ = io.WriteString(sink, "\n"+msgErr+"\n")
			}
			return "", errors.New(msgErr)
		}
		if msg.Aux != nil && msg.Aux.ID != "" {
			imageID = msg.Aux.ID
		}
	}
	return imageID, nil
}

// PushImage uploads ref to the registry named in it.
//
// Like ImageBuild, ImagePush reports failure INSIDE its JSON stream while the
// call itself returns no error — an unreachable registry, a rejected
// credential and a successful push are indistinguishable from the return value
// alone. Draining without inspecting would report every failed push as a
// success, and the deploy that followed would pull a tag that was never
// uploaded. The stream is therefore parsed with the same helper the build path
// uses.
func (a *Adapter) PushImage(ctx context.Context, ref string, registryAuth string) error {
	// An empty credential is not the same as an absent one: docker requires
	// the header to be present, and sends an anonymous push when it decodes to
	// empty credentials — which is what an unauthenticated local registry
	// wants.
	if registryAuth == "" {
		registryAuth = emptyRegistryAuth
	}

	rc, err := a.cli.ImagePush(ctx, ref, image.PushOptions{RegistryAuth: registryAuth})
	if err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	defer rc.Close()

	// nil sink: a push produces progress bars, not diagnosis. What matters
	// from it is whether it failed, which the return value carries.
	if _, err := drainBuildStream(rc, nil); err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	return nil
}

// emptyRegistryAuth is base64("{}") — the header docker expects when there are
// no credentials to send. Omitting the header entirely makes the daemon refuse
// the push before it reaches the registry.
const emptyRegistryAuth = "e30="

// PruneBuildCache reclaims build cache above reservedBytes.
func (a *Adapter) PruneBuildCache(ctx context.Context, reservedBytes int64) (uint64, error) {
	report, err := a.cli.BuildCachePrune(ctx, build.CachePruneOptions{
		ReservedSpace: reservedBytes,
		// Only qooq's own build cache would be ideal, but build cache carries no
		// ownership: it is keyed by content, and a layer shared with somebody
		// else's image is the same layer. All: false therefore keeps the prune
		// to unused entries, which is the closest thing to "nothing anybody
		// still needs".
		All: false,
	})
	if err != nil {
		return 0, fmt.Errorf("prune build cache: %w", err)
	}
	return report.SpaceReclaimed, nil
}

// buildCancelTimeout bounds the cancel request itself.
const buildCancelTimeout = 10 * time.Second

// newBuildID returns an identifier for one build, used only to cancel it.
//
// crypto/rand rather than a counter or a timestamp: two qooqd sharing a docker
// host would otherwise be able to generate the same id and cancel each other's
// builds.
func newBuildID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate build id: %w", err)
	}
	return "qooq-" + hex.EncodeToString(b[:]), nil
}
