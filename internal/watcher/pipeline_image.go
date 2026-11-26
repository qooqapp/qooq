package watcher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// splitRegistryCredential splits an "image" mode target's AuthCredential
// (documented in model.go's Target.AuthCredential comment as
// "username:password") into its two parts. No colon means the whole value is
// treated as the username with an empty password — some registries (e.g.
// GHCR with a personal access token used as a bearer-style credential)
// authenticate off a bare token in the username slot. An empty cred returns
// two empty strings, which pkg/client.PullImageAuth/ImageDigest treat as
// "anonymous", matching pre-M20a behavior.
func splitRegistryCredential(cred string) (username, password string) {
	if cred == "" {
		return "", ""
	}
	if i := strings.IndexByte(cred, ':'); i >= 0 {
		return cred[:i], cred[i+1:]
	}
	return cred, ""
}

// checkAndMaybeDeployImage is the M20a "image" DeployMode counterpart of
// checkAndMaybeBuild: instead of resolving a git remote HEAD and cloning, it
// asks qooqd's registry-digest endpoint for t.ImageRef's current digest and,
// only if it differs from t.LastImageDigest, pulls and deploys the image —
// no clone, no build, no manifest involved.
func (w *Watcher) checkAndMaybeDeployImage(ctx context.Context, t *Target) {
	log := w.log.With(zap.String("target", t.Name))

	username, password := splitRegistryCredential(t.AuthCredential)

	digest, err := w.client.ImageDigest(ctx, t.ImageRef, username, password)
	if err != nil {
		log.Warn("resolve image digest failed", zap.Error(err))
		w.recordFailure(t, fmt.Errorf("resolve image digest: %w", notTheDaemon(err)))
		return
	}

	// ForceRebuild is a person asking for this deploy (a UI's
	// 重新部署, `qooq watch trigger --force`). Without honouring it here the
	// button did nothing on an image service whose tag had not moved: the
	// sweep saw the same digest, cleared an old failure and returned, so the
	// person watched the status flip to success with an unchanged time and no
	// redeploy — and the flag stayed set forever.
	if digest == t.LastImageDigest && !t.ForceRebuild {
		// Same digest upstream — but that says nothing about whether what was
		// deployed is still on qooqd. See componentIsGone (pipeline.go).
		if w.componentIsGone(ctx, t) {
			log.Warn("the component this target deploys is gone from qooqd — redeploying it",
				zap.String("component", w.componentRefFor(t).String()))
		} else {
			// Nothing to do this sweep beyond retiring any stale failure the
			// successful re-check just disproved.
			w.clearStaleFailure(t)
			return
		}
	}

	// A digest that did not start and was rolled back is not redeployed by
	// polling — the tag still points at it, so without this every poll would
	// take the service down again. A new digest or a forced deploy clears it.
	if digest == t.RejectedCommit && !t.ForceRebuild {
		return
	}
	if t.ForceRebuild && digest == t.LastImageDigest {
		log.Info("redeploying the same image at the operator's request", zap.String("digest", digest))
	} else {
		log.Info("new image digest detected", zap.String("digest", digest), zap.String("previous", t.LastImageDigest))
	}
	w.setBuilding(t, digest)

	if err := w.runImageDeploy(ctx, t, username, password, digest); err != nil {
		log.Error("image deploy failed", zap.Error(err))
		w.recordFailure(t, err)
		return
	}

	log.Info("image deploy succeeded", zap.String("digest", t.LastImageDigest))
}

// runImageDeploy pulls t.ImageRef (authenticated with username/password, both
// empty = anonymous) and upserts the component from it. A pull failure aborts
// the whole attempt (unlike the ad-hoc /deploy endpoint's "warn and
// continue" stance) — for a poll-driven target a pull failure most likely
// means "digest changed to something we can't fetch yet" (auth expired,
// transient registry issue), and deploying a stale local image while
// silently ignoring that would be surprising; failing loud lets the next
// sweep retry cleanly and surfaces the real problem via LastBuildError.
// Success updates LastImageDigest (via recordImageSuccess) so the next sweep
// sees no change.
func (w *Watcher) runImageDeploy(ctx context.Context, t *Target, username, password, digest string) error {
	if err := w.client.PullImageAuth(ctx, t.ImageRef, username, password); err != nil {
		return fmt.Errorf("pull image: %w", notTheDaemon(err))
	}

	req, err := w.buildDeployRequest(t, t.ImageRef, deploySpecFields{})
	if err != nil {
		return err
	}

	comp, err := w.client.Upsert(ctx, req)
	if err != nil {
		return fmt.Errorf("upsert component (image %s): %w", t.ImageRef, notTheDaemon(err))
	}
	// Same rule as a git build: a new image that does not start is rolled
	// back. LastImageDigest still names the version that was running, which
	// is how the previous one is found when the tag moved.
	if err := w.verifyOrRollBackPlan(ctx, t, comp, imageRollbackPlan(t.ImageRef, digest, t.LastImageDigest)); err != nil {
		return err
	}

	return w.recordImageSuccess(t, t.ImageRef, digest)
}

// recordImageSuccess marks a successful image-mode deploy: LastBuildStatus/
// LastBuildError/LastBuildAt always update (mirroring recordSuccess's role
// for the source pipeline). LastImageDigest is only touched when digest is
// non-empty — the ad-hoc POST /v1/targets/:name/deploy handler (M20b) calls
// this with digest=="" when it could not resolve one (best-effort), and that
// must not clobber a previously known-good digest with an empty value.
//
// newImageRef, when non-empty and different from t.ImageRef, also updates
// the target's polled ref. This matters for the M20b handler: without it, an
// ad-hoc `watch deploy --image X` on an "image" mode target would record X's
// digest as LastImageDigest while ImageRef still points at the old ref — the
// very next sweep tick would then see "ImageRef's digest != LastImageDigest"
// (because nothing about ImageRef's own digest changed) and immediately
// redeploy the old ref, silently reverting the ad-hoc deploy. Making the
// ad-hoc deploy's image the new baseline the target polls is also the more
// useful behavior for its actual use case (CI just pushed X, X is now the
// intended running version). The sweep-driven path (runImageDeploy above)
// passes t.ImageRef itself here, so this is a no-op for that caller.
func (w *Watcher) recordImageSuccess(t *Target, newImageRef, digest string) error {
	now := time.Now()
	t.LastBuildStatus = BuildStatusSuccess
	t.LastBuildError = ""
	t.LastBuildAt = now
	t.ForceRebuild = false
	updates := map[string]any{
		"last_build_status": t.LastBuildStatus,
		"last_build_error":  t.LastBuildError,
		"last_build_at":     t.LastBuildAt,
		// Done what was asked; same as recordSuccess on the source side.
		"force_rebuild": false,
	}
	if newImageRef != "" && newImageRef != t.ImageRef {
		t.ImageRef = newImageRef
		updates["image_ref"] = newImageRef
	}
	if digest != "" {
		t.LastImageDigest = digest
		updates["last_image_digest"] = digest
		// What was deployed, by the identity the image sweep uses. The sweep
		// set it already; an ad-hoc deploy set it to the ref it was given.
		t.LastBuildRevision = digest
		updates["last_build_revision"] = digest
	}
	return w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(updates).Error
}
