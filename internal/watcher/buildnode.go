package watcher

import (
	"fmt"

	"github.com/qooqapp/qooq/pkg/client"
)

// Where a build runs, as opposed to where the result runs.
//
// A build executes whatever is in a repository you do not control, and the one bound
// it cannot be given is disk: the classic builder has no per-build disk quota,
// so `RUN dd if=/dev/zero of=/big` is not a feature to switch off (the way
// build.command, mounts and ${env.*} are, see trust.go) but a consequence of
// running arbitrary code at all. On one machine that fills the disk the docker
// daemon itself writes to, and then every user's container restart, health
// probe and reconcile fails, the watcher's sqlite stops accepting writes and
// so does qooqd's bbolt — one repository stopping everybody, with symptoms
// (containers in crash loops) that point nowhere near the cause.
//
// So the build moves to a machine that is allowed to break. buildAddr names
// it. Three things make this cheap rather than a new subsystem: qooqd is
// already the component that runs builds (the watcher has never built
// anything itself), pkg/client has always spoken to it over HTTP with the
// address coming from config, and registry.prefix (#25) already exists to get
// the result off the machine that built it — that is the moment it was written
// for.
//
// The address is CONFIGURATION, not the output of a scheduler. Picking a build
// node at runtime means a node registry, health and load, which is Stage C;
// one fixed address is a string and fences the same attack.
//
// What this fences is the blast radius: the build node holds no user's app,
// so filling its disk breaks no running service. It is NOT per-user disk
// fairness — two users still share that node's disk, and one can starve the
// other's build. Real fairness needs a disposable environment per build, which
// is the scheduler again. Do not describe this as user isolation.
//
// buildNode returns the client for build-side calls. A Watcher with no build
// address configured — every offline install, and every test that assembles a
// Watcher by hand — gets the one client it already had, so the single-machine
// behaviour is not merely equivalent but literally the same code path.
func (w *Watcher) buildNode() *client.Client {
	if w.buildClient != nil {
		return w.buildClient
	}
	return w.client
}

// buildNodeIsSeparate reports whether builds land on a different daemon than
// deployments.
//
// It exists for image retention, which is the one place the split changes more
// than an address. A built image is tagged twice (the local name and the
// registry name) on the node that built it, and the registry name appears a
// THIRD time on the deploy node, which pulls it. Pruning only the build node
// would therefore leave the deploy node growing by one image per build forever
// — a disk leak introduced by the very change meant to contain one.
func (w *Watcher) buildNodeIsSeparate() bool {
	return w.buildClient != nil && w.buildClient != w.client
}

// imageHosts returns every daemon that can be holding a tag this target's
// builds produced, newest-relevant first, paired with a label for logs.
//
// Single machine: one entry, and the calls made are exactly the calls made
// before this split existed.
func (w *Watcher) imageHosts() []imageHost {
	hosts := []imageHost{{c: w.buildNode(), label: "build"}}
	if w.buildNodeIsSeparate() {
		hosts = append(hosts, imageHost{c: w.client, label: "deploy"})
	}
	return hosts
}

type imageHost struct {
	c     *client.Client
	label string
}

// checkBuildNodeRegistry refuses a separate build node without a registry.
// The image is built on the build node and stays there; the deploy happens on
// the deploy node, which then cannot pull a tag that was "just built" — an
// error that names neither setting. It is refused at start, not at the first
// deploy, because with this configuration every deploy fails.
//
// build_addr equal to qooqd.addr is one node (no second client is made for
// it), and a registry without a build node is an ordinary single-machine
// setup; neither is refused.
func checkBuildNodeRegistry(buildAddr, deployAddr string, reg registryConfig) error {
	if buildAddr == "" || buildAddr == deployAddr || reg.enabled() {
		return nil
	}
	return fmt.Errorf("watcher: %s is set (%s, separate from %s %s) but %s is empty: "+
		"images built there would stay on the build node, and the deploy node could not pull them; "+
		"set %s (env QOOQ_WATCHER_REGISTRY_PREFIX) or unset %s",
		configQooqdBuildAddr, buildAddr, configQooqdAddr, deployAddr, configRegistryPrefix,
		configRegistryPrefix, configQooqdBuildAddr)
}
