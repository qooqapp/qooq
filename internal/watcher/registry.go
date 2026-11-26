package watcher

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Registry mode.
//
// Single-machine qooq deliberately does not use a registry: a build lands in the
// very daemon that is about to run it, so pushing and pulling again would buy
// nothing but a second copy on disk and a network round trip. That is a choice,
// not a shortcut, and it stays the default.
//
// It becomes necessary at the moment qooqd is no longer on the machine that
// built the image — multi-node, or any setup where builds and runs are
// separated. Then there is no other way to hand the artifact over, because
// qooq has no cross-machine image transport of its own.
//
// What this does NOT do, so nobody reads more into it than is here: it does not
// isolate the build from the runtime. A build still runs on the local docker
// daemon; the registry is where its product goes afterwards. Moving builds off
// that daemon is separate work.

// registryConfig is where built images are pushed, when anywhere.
type registryConfig struct {
	// Prefix is the registry host and optional path that image names are
	// prefixed with, e.g. "registry.example.com" or "localhost:5000/qooq".
	// Empty means registry mode is off and everything behaves as before.
	Prefix string
	// Username and Password authenticate the push. Empty is legitimate: a
	// registry on a private network often takes anonymous writes.
	Username string
	Password string
}

// enabled reports whether built images should be pushed.
func (r registryConfig) enabled() bool { return r.Prefix != "" }

// remoteRef turns a locally built tag into the ref it is pushed and pulled as.
//
// The local tag already carries the namespace (imageRepo), so the remote ref
// inherits that isolation rather than inventing a second scheme — two users'
// images cannot collide in the registry for the same reason they cannot collide
// locally.
func (r registryConfig) remoteRef(localTag string) string {
	if !r.enabled() {
		return localTag
	}
	return strings.TrimRight(r.Prefix, "/") + "/" + localTag
}

// buildTags is every name the build should apply.
//
// The local tag always comes first and is always present, so an image is
// addressable on the machine that built it whether or not a registry is
// configured. When one is, the remote name is applied by the same build — one
// pass, two names, and no window in which the two could point at different
// images.
func (w *Watcher) buildTags(localTag string) []string {
	if !w.registry.enabled() {
		return []string{localTag}
	}
	return []string{localTag, w.registry.remoteRef(localTag)}
}

// pushBuiltImage uploads the image and returns the ref a deploy should use.
//
// Returns the LOCAL tag unchanged when registry mode is off, so the caller has
// one code path and the offline product keeps deploying exactly what it built.
//
// A push failure fails the build. The alternative — warn and deploy the local
// tag — would work on the machine that built it and fail on every other one,
// which is precisely the case the registry exists for. A deploy that succeeds
// only where it happened to be built is worse than one that refuses.
func (w *Watcher) pushBuiltImage(ctx context.Context, localTag string) (string, error) {
	if !w.registry.enabled() {
		return localTag, nil
	}

	remote := w.registry.remoteRef(localTag)
	// Pushed from the node that built it: that is the only machine the image
	// exists on at this point.
	if err := w.buildNode().PushImage(ctx, remote, w.registry.Username, w.registry.Password); err != nil {
		return "", fmt.Errorf("push %s: %w", remote, err)
	}

	w.log.Info("pushed image to registry", zap.String("ref", remote))
	return remote, nil
}
