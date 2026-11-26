package watcher

import (
	"context"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// Where a deployment actually is, and whether it is actually running.
//
// A target row knows what was built. It does not know whether the container
// came up, and it does not know the address somebody can open — both of those
// live on the daemon. Without them the product's whole promise ("connect a
// repo, get a working URL") ends at "built successfully", and the person who
// deployed has to ask an operator where their app is.
//
// It also closes a gap this codebase keeps finding in other forms: a build that
// succeeded and a container that is crash-looping look identical if only the
// build is reported.

// attachRuntimeState fills in the deployed state for a set of targets.
//
// One ListComponents call for the whole set, not one per target: a page listing
// twenty projects would otherwise make twenty round trips to say the same
// thing.
//
// Best-effort by design. A daemon that is down, restarting, or older than these
// fields leaves them empty and the listing still answers — what was built is
// still true, and refusing to list anything because the runtime could not be
// reached would make an unreachable daemon look like lost data.
func (w *Watcher) attachRuntimeState(ctx context.Context, targets []TargetResponse) {
	if len(targets) == 0 || w.client == nil {
		return
	}

	comps, err := w.client.ListComponents(ctx)
	if err != nil {
		w.log.Debug("could not read deployed state; listing without it", zap.Error(err))
		return
	}

	// Keyed by (namespace, name), which is the pair that identifies a component
	// — two namespaces may each have an "api", and matching on name alone would
	// show one account another's address.
	type key struct{ ns, name string }
	byRef := make(map[key]*client.ComponentResponse, len(comps))
	for i := range comps {
		if comps[i].Phase == "Removed" {
			continue
		}
		byRef[key{comps[i].Namespace, comps[i].Name}] = &comps[i]
	}

	for i := range targets {
		ns := targets[i].Namespace
		if ns == "" {
			ns = client.DefaultNamespace
		}
		// Service phases come from the same batch, matched the same way.
		if topo := targets[i].Topology; topo != nil {
			for j := range topo.Services {
				if sc, ok := byRef[key{ns, topo.Services[j].Component}]; ok {
					topo.Services[j].Phase = sc.Phase
				}
			}
		}
		c, ok := byRef[key{ns, targets[i].ComponentName}]
		if !ok {
			continue
		}
		targets[i].Endpoints = c.Endpoints
		targets[i].ComponentPhase = c.Phase
		targets[i].ComponentError = c.LastError
		targets[i].ComponentDesiredState = c.DesiredState
		// Tags, not image names: the name carries the qualified namespace.
		targets[i].ComponentVersion = versionOf(c.Image)
		if c.PreviousImage != "" && c.PreviousImage != c.Image {
			targets[i].ComponentPreviousVersion = versionOf(c.PreviousImage)
		}
	}
}
