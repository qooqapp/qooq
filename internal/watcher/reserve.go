package watcher

import (
	"context"

	"go.uber.org/zap"
)

// reserveAddress asks qooqd to set this deploy's address aside before its
// environment is resolved, so another app's ${component.<this>.endpoint} can
// resolve before this one is running. That is what lets two apps that name
// each other deploy at all: each reserves its own address first, so whichever
// is tried second finds the first one's, deploys, and the first follows.
//
// It runs from the manifest's half of the spec merged with deploy_spec, the
// same request the deploy itself will send, so the reserved ports are the
// ones the deploy gets. Best effort: a qooqd without reservations (older
// version) or a request that cannot be built yet just means references to
// this app wait until it is deployed, as they always did.
func (w *Watcher) reserveAddress(ctx context.Context, t *Target, image string, manifestSpec deploySpecFields) {
	if w.client == nil {
		return
	}
	req, err := w.buildDeployRequest(t, image, manifestSpec)
	if err != nil || len(req.Ports) == 0 {
		return
	}
	if _, err := w.client.Reserve(ctx, req); err != nil {
		w.log.Debug("could not reserve the address ahead of the deploy", zap.String("target", t.Name), zap.Error(err))
	}
}

// releaseReservation drops whatever address was reserved for t's component,
// once the target is gone. A deploy that happened already used it up; one
// that never happened would otherwise hold its ports forever.
func (w *Watcher) releaseReservation(ctx context.Context, t *Target) {
	if w.client == nil {
		return
	}
	ref := w.componentRefFor(t)
	if err := w.client.ReleaseReservation(ctx, ref.namespace, ref.name); err != nil {
		w.log.Debug("could not release the reserved address", zap.String("target", t.Name), zap.Error(err))
	}
}
