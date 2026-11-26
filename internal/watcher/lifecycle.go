package watcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// What can be done to a deployment that is not "build it again".
//
// Three verbs, deliberately separate, because they answer three different
// questions and conflating any two of them makes one of the answers a lie:
//
//   - stop/start is about the process. The project, its settings, its data and
//     its images all stay; what stops is the thing answering at the address.
//   - pause/resume is about the watching. New commits stop being deployed; what
//     is already running keeps running.
//   - remove is about all of it, and it now really is all of it — see
//     removeComponentFor.
//
// The pair that is easiest to get wrong is stop and pause: a project that is
// stopped but still watched comes back by itself the next time somebody pushes,
// because a deploy starts what it deploys. That is correct and it is what the
// UI has to say out loud, not something to paper over here by making stop
// silently pause too.

// errNoComponent is "there is nothing deployed for this target".
//
// Not an error for removal — there is nothing to remove, which is the state
// removal is for — but it is one for stop and start, where it means the caller
// is acting on something that was never built.
var errNoComponent = errors.New("this project has nothing deployed yet")

// errNoDaemon is the honest version of a nil client.
var errNoDaemon = errors.New("this watcher has no qooqd configured")

// componentFor finds the component this target deploys, if it is there.
func (w *Watcher) componentFor(ctx context.Context, t *Target) (*client.ComponentResponse, error) {
	if w.client == nil {
		return nil, errNoDaemon
	}
	return w.findComponent(ctx, w.componentRefFor(t))
}

// StopComponentFor stops the target's component without forgetting anything.
func (w *Watcher) stopComponentFor(ctx context.Context, t *Target) error {
	comp, err := w.componentFor(ctx, t)
	if err != nil {
		return err
	}
	if comp == nil {
		return errNoComponent
	}
	if _, err := w.client.StopComponent(ctx, comp.ID); err != nil {
		return fmt.Errorf("stop component: %w", err)
	}
	return nil
}

func (w *Watcher) startComponentFor(ctx context.Context, t *Target) error {
	comp, err := w.componentFor(ctx, t)
	if err != nil {
		return err
	}
	if comp == nil {
		return errNoComponent
	}
	if _, err := w.client.StartComponent(ctx, comp.ID); err != nil {
		return fmt.Errorf("start component: %w", err)
	}
	return nil
}

// errNothingToRollBack: the component has never been replaced, or its
// previous image is the one it is running (an image service on a moving tag).
var errNothingToRollBack = errors.New("there is no earlier version to go back to")

// errDeployInProgress: a build or the check after a deploy holds the target.
// Rolling back underneath it would be read by that check as the new version
// failing, and it would roll back again on its own.
var errDeployInProgress = errors.New("a deploy is in progress; try again when it has finished")

// rollbackComponentFor puts the component's previous image back.
//
// It is qooqd's rollback, so a second one goes forward again. The target is
// not touched: its last commit is still the newer one, so polling sees nothing
// new and the rollback stays until the next push or an explicit deploy.
func (w *Watcher) rollbackComponentFor(ctx context.Context, t *Target) error {
	comp, err := w.componentFor(ctx, t)
	if err != nil {
		return err
	}
	if comp == nil {
		return errNoComponent
	}
	if comp.PreviousImage == "" || comp.PreviousImage == comp.Image {
		return errNothingToRollBack
	}
	if _, err := w.client.RollbackComponent(ctx, comp.ID); err != nil {
		if client.IsConflict(err) {
			return errNothingToRollBack
		}
		return fmt.Errorf("roll back component: %w", err)
	}
	return nil
}

// removeComponentFor takes down what the target deployed.
//
// Deleting a target used to delete only the row: the container kept running,
// the address kept answering, and the project disappeared from every listing —
// so the thing nobody wanted was also the thing nobody could find again.
// Verified by doing it, which is how it was found.
//
// A target with nothing deployed is not an error here. Removal is idempotent by
// nature, and "it was already gone" is the state being asked for.
//
// What it deployed includes the services: block. Those are components of their
// own (`<component>-<service>`), and removing only the app used to leave its
// database running in a namespace nobody could see any more — found after a
// reset, as the one container left standing. The app goes first so it is not
// left crash-looping against a database that just disappeared.
//
// And the data qooq keeps for them goes too (removeComponentData). Removal is
// an explicit, confirmed request to delete the project, and data left behind
// is worse than it looks: a database recreated under the same name finds the
// old data directory, ignores the newly generated password, and the app can
// never log in — while a shared-node user has no way to reach that directory.
// Redeploys still never touch volumes; only removal does.
func (w *Watcher) removeComponentFor(ctx context.Context, t *Target) error {
	if w.client == nil {
		return errNoDaemon
	}
	comps, err := w.client.ListComponents(ctx)
	if err != nil {
		return err
	}
	ref := w.componentRefFor(t)
	names := []string{ref.name}
	if topo := t.topologyForResponse(); topo != nil {
		for _, s := range topo.Services {
			names = append(names, s.Component)
		}
	}
	for _, name := range names {
		for i := range comps {
			c := comps[i]
			if c.Namespace != ref.namespace || c.Name != name || c.Phase == "Removed" {
				continue
			}
			if err := w.client.RemoveComponent(ctx, c.ID); err != nil && !client.IsNotFound(err) {
				// Already gone on the daemon's side is the same answer as
				// success: the caller asked for it not to be there.
				return fmt.Errorf("remove component %s: %w", name, err)
			}
		}
	}
	for _, name := range names {
		w.removeComponentData(componentRef{namespace: ref.namespace, name: name})
	}
	w.removeBuiltImages(ctx, t)
	return nil
}

// removeBuiltImages deletes the images this target's builds produced, now that
// nothing runs them. Every redeploy prunes all but two (pruneOldImages); left
// at removal, those two stay on the machine forever with nothing to name
// them. Only images qooq built: an image-mode target deploys someone else's
// image, and a service's image (postgres) is pulled, not built. Best effort
// — the project is already gone, so a failure here is logged, not returned.
func (w *Watcher) removeBuiltImages(ctx context.Context, t *Target) {
	if t.DeployMode == DeployModeImage {
		return
	}
	localRepo := imageRepo(namespaceOf(t), t.ComponentName)
	// An empty current tag and no previous commit keep nothing.
	w.pruneOldImages(ctx, imageHost{c: w.buildNode(), label: "build"}, localRepo, "", "")
	if w.registry.enabled() {
		for _, h := range w.imageHosts() {
			w.pruneOldImages(ctx, h, w.registry.remoteRef(localRepo), "", "")
		}
	}
}

// removeComponentData deletes everything under the component's mounts
// directory: its volumes (database data included) and its mounted files.
// Only ever a directory qooq itself created, under mounts.dir; a host path a
// manifest bind-mounted is not under it and is never touched.
func (w *Watcher) removeComponentData(ref componentRef) {
	if w.mountsDir == "" || ref.namespace == "" || ref.name == "" {
		return
	}
	dir := w.componentMountsDir(ref)
	if rel, err := filepath.Rel(w.mountsDir, dir); err != nil || !filepath.IsLocal(rel) {
		w.log.Error("refusing to delete data outside the mounts directory", zap.String("dir", dir))
		return
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		w.log.Warn("could not delete the data of a removed component",
			zap.String("component", ref.String()), zap.String("dir", dir), zap.Error(err))
		return
	}
	w.log.Info("deleted the data of a removed component", zap.String("component", ref.String()))
}

// errServiceStillDeclared refuses to delete a service the manifest declares:
// the next deploy would only create it again, empty.
var errServiceStillDeclared = errors.New("this service is still declared in qooq.yaml; remove it from there first")

// handleDeleteDroppedService handles DELETE /v1/targets/:name/services/:service:
// the confirmed deletion of a service the manifest no longer declares — its
// container and its data. Deploys only ever stop such a service; this is the
// one place its data goes, short of removing the whole project.
func (w *Watcher) handleDeleteDroppedService(c *gin.Context) {
	unlock := w.lockTarget(c.Param("name"))
	defer unlock()

	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := w.deleteDroppedService(c.Request.Context(), t, c.Param("service")); err != nil {
		switch {
		case errors.Is(err, errServiceStillDeclared):
			respondErr(c, http.StatusConflict, err)
		case errors.Is(err, errServiceNotFound):
			respondErr(c, http.StatusNotFound, err)
		default:
			respondErr(c, http.StatusBadGateway, err)
		}
		return
	}
	c.Status(http.StatusNoContent)
}

var errServiceNotFound = errors.New("no such service on this project")

// deleteDroppedService removes the component and data of one dropped service
// and takes it off the topology and the stored passwords. The caller holds
// the target lock.
func (w *Watcher) deleteDroppedService(ctx context.Context, t *Target, service string) error {
	topo := t.topologyForResponse()
	if topo == nil {
		return errServiceNotFound
	}
	idx := -1
	for i, s := range topo.Services {
		if s.Name == service {
			idx = i
		}
	}
	if idx < 0 {
		return errServiceNotFound
	}
	s := topo.Services[idx]
	if !s.Dropped {
		return errServiceStillDeclared
	}
	if w.client == nil {
		return errNoDaemon
	}
	ns := w.componentRefFor(t).namespace
	comp, err := w.findComponent(ctx, componentRef{namespace: ns, name: s.Component})
	if err != nil {
		return err
	}
	if comp != nil {
		if err := w.client.RemoveComponent(ctx, comp.ID); err != nil && !client.IsNotFound(err) {
			return fmt.Errorf("remove component %s: %w", s.Component, err)
		}
	}
	w.removeComponentData(componentRef{namespace: ns, name: s.Component})

	// Its password goes too: the data it opened is gone, and a service
	// declared again under this name starts from an empty directory.
	if state, err := w.loadServiceState(t); err == nil {
		if _, ok := state[service]; ok {
			delete(state, service)
			if err := w.saveServiceState(t, state); err != nil {
				return err
			}
		}
	}
	topo.Services = append(topo.Services[:idx], topo.Services[idx+1:]...)
	t.Topology = encodeTopology(topo.Services, topo.References)
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Update("topology", t.Topology).Error; err != nil {
		return err
	}
	w.log.Info("deleted a service no longer in the manifest, with its data",
		zap.String("target", t.Name), zap.String("service", service), zap.String("component", s.Component))
	return nil
}
