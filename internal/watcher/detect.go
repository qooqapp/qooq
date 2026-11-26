package watcher

import (
	"fmt"
	"strings"

	"go.uber.org/zap"

	qm "github.com/qooqapp/qooq/pkg/manifest"
)

// Detection is produced by pkg/manifest, which owns both the format and the
// rules for guessing one — so the draft `qooq init` writes locally and the one
// the watcher generates for a repo without a qooq.yaml can never diverge.
type detection = qm.Detection

func detectManifest(dir, app, preferredDockerfile string) (*detection, error) {
	return qm.Detect(dir, app, preferredDockerfile)
}

// renderGeneratedManifest leaves ports unpublished unless the target says
// otherwise: on a node, publishing a repo qooq only guessed at is the
// operator's call. A target the external manager created says so, because there
// nobody else will make that call and a service without an address is a
// deploy that looks done and cannot be opened.
func renderGeneratedManifest(d *detection, publish bool) []byte {
	return qm.Render(d, qm.RenderOptions{PublishPorts: publish})
}

// resolveManifest returns the manifest a build should use: the repo's own
// qooq.yaml when it has one, otherwise the generated stand-in.
//
// Generation happens exactly once per target. The result is stored and reused,
// because the whole point is that an operator can correct a wrong guess — a
// correction that gets overwritten on the next sweep is not a correction. A
// target whose manifest is explicitly disabled ("-") is left alone: that is
// the escape hatch for repos that want nothing to do with manifests.
func (w *Watcher) resolveManifest(t *Target, dir string) (*manifest, error) {
	mf, err := w.loadManifest(dir, t)
	if err != nil {
		return nil, err
	}
	if mf != nil || t.ManifestPath == "-" {
		return mf, nil
	}

	if stored := strings.TrimSpace(t.GeneratedManifest); stored != "" {
		mf, err := parseManifest([]byte(t.GeneratedManifest))
		if err != nil {
			// The stored copy is editable, so this is usually someone's typo.
			// Saying where it lives is the difference between a fixable error
			// and a mystery.
			return nil, fmt.Errorf("the generated manifest for this target is invalid (edit it with `qooq watch manifest %s`): %w", t.Name, err)
		}
		return mf, nil
	}

	d, err := detectManifest(dir, t.ComponentName, t.Dockerfile)
	if err != nil {
		return nil, err
	}
	rendered := renderGeneratedManifest(d, t.PublishDetectedPorts)
	parsed, err := parseManifest(rendered)
	if err != nil {
		return nil, fmt.Errorf("generated an invalid manifest (this is a qooq bug): %w", err)
	}

	t.GeneratedManifest = string(rendered)
	if err := w.db.Model(t).Update("generated_manifest", t.GeneratedManifest).Error; err != nil {
		// Not fatal: the build can proceed on the in-memory copy. It just
		// means the next sweep detects again, which is wasteful, not wrong.
		w.log.Warn("could not store the generated manifest",
			zap.String("target", t.Name), zap.Error(err))
	}
	w.log.Info("generated a manifest for a repo that has none",
		zap.String("target", t.Name), zap.String("detected", d.Kind),
		zap.Strings("notes", d.Notes))
	return parsed, nil
}
