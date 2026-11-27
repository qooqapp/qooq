package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/buildctx"
	"github.com/qooqapp/qooq/pkg/client"
)

// processBuildTimeout bounds how long a manifest's build.command may run
// (M16). Ten minutes is generous for a `go build`/`npm run build`-class
// command without letting a runaway build wedge the sweep loop forever.
const processBuildTimeout = 10 * time.Minute

// maskRepoCredentials redacts any embedded userinfo (user:token@ or user@) from
// t.RepoURL wherever it appears verbatim inside msg — clone/build errors often
// echo the failing URL back, and RepoURL may carry an auth token
// (https://user:token@host/...) that must not end up in LastBuildError or logs.
func maskRepoCredentials(msg, repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.User == nil {
		return msg
	}
	userinfo := u.User.String() // percent-encoded "user" or "user:pass"
	if userinfo != "" {
		msg = strings.ReplaceAll(msg, userinfo+"@", "***@")
	}

	// F7 (M17 review): url.Parse leaves userinfo percent-encoded, but some
	// error paths (raw git stderr, os/exec output from build.command) echo
	// the percent-*decoded* credential instead, which the replacement above
	// never matches. Scrub the decoded password (alone, and combined with
	// the username) too. Guarded by a length floor so a short password
	// fragment can't accidentally redact unrelated, coincidental substrings
	// in build output.
	if password, ok := u.User.Password(); ok && len(password) >= 4 {
		if username := u.User.Username(); username != "" {
			msg = strings.ReplaceAll(msg, username+":"+password, "***")
		}
		msg = strings.ReplaceAll(msg, password, "***")
	}

	return msg
}

// maxManifestBytes caps how large a qooq.yaml the watcher will parse (F9,
// M17 review). Manifests are small hand-written config files; without a
// cap a malicious or corrupted repo could hand the watcher an arbitrarily
// large file and burn memory/CPU on yaml.Unmarshal for every sweep tick.
const maxManifestBytes = 1 << 20 // 1 MiB

// resolveWithin resolves rel (already string-validated as repo-relative and
// non-escaping by filepath.IsLocal/validTargetPath) against root, following
// any symlinks along the way, and confirms the fully-resolved path still
// lives inside root (F3, M17 review). filepath.IsLocal only rejects ".."
// and absolute paths in the *string* — it says nothing about where the path
// resolves to once symlinks are followed, so a repo can plant e.g.
// `bin/app -> /etc/passwd` and have build.artifact "bin/app" read (and, for
// process-runtime deploys, execute) an arbitrary host file. The same trick
// works for context_subdir/dockerfile to pull host files into a docker
// build context.
//
// EvalSymlinks requires its target to exist. When the leaf (rel's final
// path component) doesn't exist yet — e.g. a Dockerfile whose existence the
// caller checks separately — the parent directory is resolved instead and
// the leaf name is reattached, so a symlinked *parent* directory still
// can't be used to escape.
//
// root itself is resolved too (not just joined+rel): on macOS the default
// TMPDIR (where clones land, see mkTempCloneDir) is itself a symlink
// (/tmp -> /private/tmp), so comparing an unresolved root against a fully
// resolved child path would false-positive on every single call.
func resolveWithin(root, rel string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}

	joined := filepath.Join(root, rel)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %q: %w", rel, err)
		}
		parent, perr := filepath.EvalSymlinks(filepath.Dir(joined))
		if perr != nil {
			return "", fmt.Errorf("resolve %q: %w", rel, perr)
		}
		resolved = filepath.Join(parent, filepath.Base(joined))
	}

	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q escapes the repo root via symlink", rel)
	}
	return resolved, nil
}

// targetNamespace reports which namespace this target's components belong to.
//
// The operator's deploy_spec is where it comes from — including for a stack's
// projects, whose deploy_spec the stack layer writes (see desiredProjectTarget).
// Callers that need the namespace before buildDeployRequest has parsed the spec
// go through here — a malformed
// deploy_spec is reported by buildDeployRequest itself, so a parse failure is
// treated as "no namespace" rather than duplicated into a second error.
func (w *Watcher) targetNamespace(t *Target) string {
	spec, err := parseDeploySpec(t.DeploySpec)
	if err != nil {
		return client.DefaultNamespace
	}
	return spec.Namespace
}

// componentRefFor is the (namespace, name) pair identifying this target's
// component.
func (w *Watcher) componentRefFor(t *Target) componentRef {
	return componentRef{namespace: w.targetNamespace(t), name: t.ComponentName}
}

// shortSHA returns the first 7 characters of a git commit hash, matching the
// short-sha convention used for image tags.
func shortSHA(hash string) string {
	if len(hash) <= 7 {
		return hash
	}
	return hash[:7]
}

// checkAndMaybeBuild resolves the remote HEAD for t's branch and, if it
// differs from t.LastCommit, runs the full clone -> tar -> build -> upsert
// pipeline. Any failure along the way records LastBuildStatus=failed and
// LastBuildError without touching LastCommit, so the next sweep retries.
func (w *Watcher) checkAndMaybeBuild(ctx context.Context, t *Target) {
	log := w.log.With(zap.String("target", t.Name))

	var hash string
	if t.DeployMode == DeployModeUpload {
		// No remote to ask: the revision is whatever was uploaded last. From
		// here on an upload is treated exactly like a commit — the same
		// archive again is "nothing changed", a rejected one is not retried.
		hash = t.UploadRevision
		if hash == "" {
			// Nothing uploaded yet. Not a failure; there is nothing to build.
			return
		}
	} else {
		var err error
		hash, err = ResolveRemoteHead(t, w.gitAuth())
		if err != nil {
			log.Warn("resolve remote head failed", zap.String("err", maskRepoCredentials(err.Error(), t.RepoURL)))
			w.recordFailure(t, fmt.Errorf("resolve remote head: %w", err))
			return
		}
	}

	if hash == t.LastCommit && !t.ForceRebuild {
		// No new commit — but "nothing changed upstream" is not the same as
		// "what I deployed is still running". If the component is gone from
		// qooqd, fall through and deploy it again (componentIsGone).
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
	// A commit whose version did not start, and was rolled back, is not
	// retried by polling: each retry would take the service down again.
	// The failure stays on the target until a new commit or a forced deploy.
	if hash == t.RejectedCommit && !t.ForceRebuild {
		return
	}
	// The same revision just failed to build: wait longer before each retry
	// (retryBackoff). Only the retry waits — a new commit is built as soon as
	// polling sees it, because the person who just pushed a fix is the one
	// waiting.
	if wait := retryWait(t, hash, time.Now()); wait > 0 && !t.ForceRebuild {
		log.Debug("not retrying a failed revision yet", zap.String("revision", hash), zap.Duration("wait", wait))
		return
	}
	if t.ForceRebuild {
		log.Info("rebuilding at the operator's request (deploy inputs changed, not the code)")
	}

	log.Info("new commit detected", zap.String("commit", hash), zap.String("previous", t.LastCommit))
	w.setBuilding(t, hash)

	if err := w.runPipeline(ctx, t); err != nil {
		// A deploy blocked only on missing input values is not a failure to
		// report as one: nothing is broken, something is unanswered. It is
		// still recorded (and still stops the deploy), but under a status the
		// CLI can turn into a question.
		var needs errNeedsInput
		if errors.As(err, &needs) {
			log.Info("deploy is waiting on input values", zap.String("err", err.Error()))
			w.recordNeedsInput(t, err.Error(), needs.waitingOn)
			return
		}
		log.Error("pipeline failed", zap.String("err", maskRepoCredentials(err.Error(), t.RepoURL)))
		w.recordFailure(t, err)
		return
	}

	log.Info("pipeline succeeded", zap.String("commit", t.LastCommit))
}

// fetchSource puts the source a build starts from into a fresh temporary
// directory and reports the revision it is: for a git target, a depth-1
// clone of the branch and the commit it landed on.
//
// It is the only step of the pipeline that knows where source comes from.
// Everything after it reads a directory and a revision string, so a second
// kind of source is a second branch here and nothing anywhere else — which is
// the point: two pipelines would drift apart, and the one nobody was looking
// at would be the broken one.
//
// cleanup removes the directory and must be called once the caller is done
// with it; it is nil whenever err is not.
//
// An upload target's source is its stored archive (upload.go), extracted
// fresh into the same kind of temp directory a clone would get.
func (w *Watcher) fetchSource(t *Target) (dir, revision string, cleanup func(), err error) {
	dir, err = mkTempCloneDir(t.Name)
	if err != nil {
		return "", "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	if t.DeployMode == DeployModeUpload {
		revision, err = w.extractStoredUpload(t, dir)
		if err != nil {
			cleanup()
			return "", "", nil, err
		}
		return dir, revision, cleanup, nil
	}

	revision, err = cloneBranch(t, dir, w.gitAuth())
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("clone: %w", err)
	}
	return dir, revision, cleanup, nil
}

// runPipeline fetches the source (fetchSource), tars the (sub)directory as a
// build context, builds an image via qooqd, and upserts the component. On
// success it updates t in place and persists it with LastCommit set to the
// revision that was fetched.
func (w *Watcher) runPipeline(ctx context.Context, t *Target) error {
	dir, headHash, cleanup, err := w.fetchSource(t)
	if err != nil {
		return err
	}
	defer cleanup()

	// Defensive re-validation: these fields are also checked at target-create
	// time (handleCreateTarget), but a record persisted before that check
	// existed, or written directly to the DB, must not be allowed to make the
	// build pack up an arbitrary host directory.
	if !validTargetPath(t.ContextSubdir) {
		return fmt.Errorf("context_subdir %q escapes the repo root", t.ContextSubdir)
	}
	if !validTargetPath(t.Dockerfile) {
		return fmt.Errorf("dockerfile %q escapes the repo root", t.Dockerfile)
	}

	dockerfile := t.Dockerfile
	contextSubdir := t.ContextSubdir
	var manifestSpec deploySpecFields

	mf, err := w.resolveManifest(t, dir)
	if err != nil {
		return fmt.Errorf("qooq.yaml: %w", err)
	}
	if err := checkTenantManifestAddress(t, mf); err != nil {
		return fmt.Errorf("qooq.yaml: %w", err)
	}
	if mf != nil {
		// Inputs are checked before anything else the manifest asks for, and
		// well before any build: a deploy that cannot succeed for want of a
		// value should say so in seconds, not after a five-minute image build.
		// The declarations are cached first, so the very response that says
		// "STRIPE_KEY is missing" can also describe what STRIPE_KEY is for.
		w.recordDeclaredInputs(t, mf.Inputs)
		inputVals, err := w.loadInputValues(t)
		if err != nil {
			return fmt.Errorf("qooq.yaml: inputs: %w", err)
		}
		if missing := missingInputs(mf.Inputs, inputVals); len(missing) > 0 {
			return needsInput(missingInputsError(t.Name, mf.Inputs, missing))
		}

		wantRuntime := mf.Requests.Runtime
		if wantRuntime == "" {
			wantRuntime = "container"
		}
		if wantRuntime != "container" && wantRuntime != "process" {
			return fmt.Errorf("qooq.yaml: requests.runtime %q: must be \"container\" or \"process\"", wantRuntime)
		}
		if mf.Requests.Disk != "" {
			w.log.Info("qooq.yaml requests.disk is not validated in v1", zap.String("target", t.Name), zap.String("disk", mf.Requests.Disk))
		}
		if mf.Requests.GPU != "" {
			w.log.Info("qooq.yaml requests.gpu is not validated in v1", zap.String("target", t.Name), zap.String("gpu", mf.Requests.GPU))
		}
		// Runtime + cpu/memory/arch validation happens right after the
		// manifest is parsed and before any build work (docker build or
		// build.command) starts — cheapest point after the unavoidable clone
		// (the manifest itself lives in the repo, so there is no way to read
		// requests.runtime without cloning first).
		if err := w.checkRequests(ctx, t, mf.Requests); err != nil {
			return fmt.Errorf("qooq.yaml: %w", err)
		}
		manifestSpec, err = manifestToDeploySpec(mf, w.limitsMultiplier)
		if err != nil {
			return fmt.Errorf("qooq.yaml: %w", err)
		}
		// The maximum is checked again on the final request; this is the
		// early copy, so a qooq.yaml asking for too much fails before a build
		// it could never deploy is paid for (measured: it built the whole
		// image first, then refused).
		if _, _, err := w.limits.apply(manifestSpec.CPULimit, manifestSpec.MemLimitBytes); err != nil {
			return fmt.Errorf("qooq.yaml: %w", err)
		}
		// An app with a manifest always lands on a network — its own private
		// one unless it asked to join a shared one. Targets without a
		// manifest keep the engine default, so nothing that worked before
		// M23 moves networks underneath its operator.
		//
		// Both branches are namespace-scoped (see tenancy.go). The explicit one
		// especially: without it, "network: shared" in a repo joins whatever
		// other namespace already has a network by that name, which is a
		// tenancy escape and not a feature.
		ns := namespaceOf(t)
		if manifestSpec.Network == "" {
			manifestSpec.Network = defaultAppNetwork(ns, t.ComponentName)
		} else {
			manifestSpec.Network = namespaceQualified(ns, manifestSpec.Network)
		}

		// Services are containers; a process-runtime qooqd has no engine to
		// run them on. Failing here, before any build work, beats deploying
		// an app whose database silently never existed.
		if wantRuntime == "process" && len(mf.Services) > 0 {
			return fmt.Errorf("qooq.yaml: services: requires requests.runtime \"container\" (this target is process runtime)")
		}

		if wantRuntime == "process" {
			return w.runProcessPipeline(ctx, t, dir, headHash, mf, manifestSpec)
		}

		if mf.Build.Dockerfile != "" {
			if !validTargetPath(mf.Build.Dockerfile) {
				return fmt.Errorf("qooq.yaml: build.dockerfile %q escapes the repo root", mf.Build.Dockerfile)
			}
			dockerfile = mf.Build.Dockerfile
		}
		if mf.Build.Context != "" {
			if !validTargetPath(mf.Build.Context) {
				return fmt.Errorf("qooq.yaml: build.context %q escapes the repo root", mf.Build.Context)
			}
			contextSubdir = mf.Build.Context
		}
	}

	contextDir := dir
	if contextSubdir != "" {
		resolved, err := resolveWithin(dir, contextSubdir)
		if err != nil {
			return fmt.Errorf("context_subdir %q: %w", contextSubdir, err)
		}
		contextDir = resolved
	}
	if info, statErr := os.Stat(contextDir); statErr != nil || !info.IsDir() {
		return fmt.Errorf("context_subdir %q not found in repo", contextSubdir)
	}
	if dockerfile != "" {
		if _, err := resolveWithin(dir, dockerfile); err != nil {
			return fmt.Errorf("dockerfile %q: %w", dockerfile, err)
		}
	}

	// M21: build.template generates a Dockerfile from a built-in template
	// instead of requiring one committed to the repo. parseManifest already
	// rejects a manifest that sets both build.template and build.dockerfile,
	// so this never fights with the mf.Build.Dockerfile override above.
	// The generated file is written into contextDir (not the clone root):
	// docker's dockerfile path is resolved relative to the build context, so
	// writing here is correct whether or not build.context narrowed the
	// context to a subdirectory. TarDir packs the context directory fresh
	// right after this, so the generated file is naturally included. The
	// filename is fixed and watcher-chosen (not user input), so it doesn't
	// need resolveWithin.
	if mf != nil && mf.Build.Template != "" {
		content, err := renderDockerfile(mf.Build.Template, mf.Build.TemplateVars, dir)
		if err != nil {
			return fmt.Errorf("qooq.yaml: build.template: %w", err)
		}
		if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile.qooq-generated"), []byte(content), 0o644); err != nil {
			return fmt.Errorf("qooq.yaml: write generated Dockerfile: %w", err)
		}
		dockerfile = "Dockerfile.qooq-generated"
	}

	tag := fmt.Sprintf("%s:%s", imageRepo(namespaceOf(t), t.ComponentName), shortSHA(headHash))

	w.refreshBaseImages(ctx, filepath.Join(contextDir, dockerfile))

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(buildctx.TarDir(contextDir, pw))
	}()

	// One build, every name: the local tag always, plus the registry ref when
	// registry mode is on.
	buildResp, err := w.buildNode().BuildImageTags(ctx, pr, dockerfile, w.buildTags(tag))
	if err != nil {
		// Recorded here rather than threaded through the error return: this is
		// the only place the output exists, and a failed build's output is the
		// answer to the question the failure raises. LastBuildError keeps its
		// one-line summary; this is the rest of it.
		w.recordBuildLog(t, client.BuildLogOf(err))
		return buildFailure(notTheDaemon(err))
	}
	// Kept on success too. "It built, but what did it actually do?" is a real
	// question — a stale layer cache, a dependency resolving to something
	// unexpected — and it cannot be answered afterwards from anywhere else.
	w.recordBuildLog(t, buildResp.Log)

	// deployRef is what the daemon is asked to run. Identical to the local tag
	// unless a registry is configured, so single-machine behaviour is
	// untouched — it deploys exactly the image it just built, with no push and
	// no pull.
	deployRef, err := w.pushBuiltImage(ctx, tag)
	if err != nil {
		return err
	}

	// The run: section is resolved here — after the build, before the deploy
	// request — because this is the first point where the commit sha that
	// versions file copies is settled, and the last point where the clone
	// (which repo-relative sources are read out of) still exists. Writing
	// into manifestSpec keeps operator precedence intact: mergeDeploySpec
	// below still lets deploy_spec win field by field.
	// A rebuild without services or references must not inherit the old ones.
	t.Topology = ""
	w.reserveAddress(ctx, t, deployRef, manifestSpec)
	if mf != nil {
		manifestSpec, err = w.applyRunConfig(ctx, t, mf, manifestSpec, dir, shortSHA(headHash))
		if err != nil {
			return fmt.Errorf("qooq.yaml: %w", err)
		}
	}

	req, err := w.buildDeployRequest(t, deployRef, manifestSpec)
	if err != nil {
		return err
	}

	// Capture the pre-upgrade commit *before* recordSuccess overwrites
	// t.LastCommit, so pruneOldImages knows exactly which tag was "previous"
	// (see its doc comment for why this can't be inferred from ListImages'
	// Created timestamps).
	previousCommit := t.LastCommit

	comp, err := w.client.Upsert(ctx, req)
	if err != nil {
		return fmt.Errorf("upsert component (built image %s, id=%s): %w", tag, buildResp.ImageID, notTheDaemon(err))
	}
	if err := w.verifyOrRollBack(ctx, t, comp, headHash); err != nil {
		return err
	}

	if err := w.recordSuccess(t, headHash); err != nil {
		return err
	}

	previousCommit = replacedVersion(comp, previousCommit)

	// The same repo name the tag was built under: pruning must claim exactly
	// the images this target produced, and nothing another namespace's target
	// produced under the same component name.
	localRepo := imageRepo(namespaceOf(t), t.ComponentName)
	// The local tag only ever exists where the build ran.
	w.pruneOldImages(ctx, imageHost{c: w.buildNode(), label: "build"}, localRepo, tag, previousCommit)
	if w.registry.enabled() {
		// The registry ref is a second tag on the same image, under a
		// different repository name, so the local prune above does not match
		// it — left alone it would accumulate on this machine forever, one
		// copy per build. Pruning the registry's own copies is a different
		// job (registry garbage collection) and not this one.
		// The registry name is a second tag on the build node AND the name the
		// deploy node pulled, so every host that can hold it gets a pass —
		// one host when they are the same machine, which is the old behaviour
		// unchanged.
		for _, h := range w.imageHosts() {
			w.pruneOldImages(ctx, h, w.registry.remoteRef(localRepo),
				w.registry.remoteRef(tag), previousCommit)
		}
	}
	w.pruneOldFileMounts(w.componentRefFor(t))
	return nil
}

// buildFailure words a failed image build for the person who owns the repo.
//
// lowDiskFigures is the numbers out of qooqd's low-disk message — "20% free,
// builds need 25%" — without the sentence around them, which the watcher's
// own wording already says (it read twice, E-group on Linux). A message of
// another shape is kept whole: losing the figures is worse than repeating.
func lowDiskFigures(msg string) string {
	if m := trailingParens.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return msg
}

var trailingParens = regexp.MustCompile(`\(([^()]*)\)\s*$`)

// The client's error reads "POST /v1/images/build?dockerfile=…&tag=…: <what
// went wrong> (500)" — an internal route and an encoded tag in front of the
// only part that means anything to them (measured on a timed-out build). The
// daemon's own message is kept; the route is dropped. A timeout gets a sentence
// of its own, because "504" says nothing about what to look for. The original
// stays in the chain, so status checks still see it.
func buildFailure(err error) error {
	var api *client.APIError
	if !errors.As(err, &api) {
		return fmt.Errorf("build image: %w", err)
	}
	if api.StatusCode == http.StatusInsufficientStorage {
		// The machine is full, not the repository broken: marked so it is
		// neither counted toward a pause nor backed off, and retried as soon
		// as space comes back.
		return notTheRepository(&wordedError{err: err,
			msg: "this server is low on disk space, so builds are paused for everyone; it will build again on its own once space is freed (" + lowDiskFigures(api.Message) + ")"})
	}
	if api.StatusCode == http.StatusGatewayTimeout {
		limit := ""
		if i := strings.LastIndex(api.Message, "after "); i >= 0 {
			limit = " (" + strings.TrimSpace(api.Message[i+len("after "):]) + ")"
		}
		return &wordedError{err: err, msg: "the build was stopped: it ran longer than this server allows" + limit +
			". Look for a step that never finishes — a server started in a RUN line, or a command waiting for input; the build output shows the last step that ran"}
	}
	// A 5xx reaches us as "internal error" (the daemon masks its 5xx), and
	// the reason is the last thing the build printed — "The command … returned
	// a non-zero code: 3" — so that line is the message (measured: the
	// summary said only "build failed: internal error").
	if last := lastLine(api.BuildLog); api.StatusCode >= 500 && last != "" {
		// 137 is SIGKILL, and in a build that is almost always the memory
		// limit — but the line only says "code: 137" (measured with
		// -build-memory 512m: killed at 490 MB with nothing else said).
		if strings.HasSuffix(last, "non-zero code: 137") {
			last += " — 137 means the step was killed, most often for using more memory than this server allows a build"
		}
		return &wordedError{err: err, msg: "build failed: " + last}
	}
	return &wordedError{err: err, msg: "build failed: " + api.Message}
}

// ansiEscape matches terminal colour codes, which a build's output has.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// lastLine is the last non-blank line of s, colour codes removed.
func lastLine(s string) string {
	lines := strings.Split(ansiEscape.ReplaceAllString(s, ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				l = l[:300] + "…"
			}
			return l
		}
	}
	return ""
}

// wordedError replaces an error's text and keeps it underneath.
type wordedError struct {
	err error
	msg string
}

func (e *wordedError) Error() string { return e.msg }
func (e *wordedError) Unwrap() error { return e.err }

// replacedVersion is the version to keep beside the one just deployed: what
// ran until this upsert. After a manual rollback that is not the last commit
// but the one rolled back to, and keeping the last commit instead deletes the
// image this deploy's own rollback would need. qooqd records it as
// PreviousImage; lastCommit is the fallback for a first deploy.
func replacedVersion(comp *client.ComponentResponse, lastCommit string) string {
	if comp != nil && comp.PreviousImage != "" {
		if v := versionOf(comp.PreviousImage); v != "" {
			return v
		}
	}
	return lastCommit
}

// pruneOldImages deletes every `<componentName>:*` tagged image known to
// qooqd except currentTag (the one this build just produced) and the tag for
// previousCommit (the target's commit before this build, i.e. what the
// component was running until this upsert replaced it) — mirroring
// pruneOldArtifacts' current+previous retention semantics for the M16
// process-runtime pipeline.
//
// This deliberately does NOT sort ListImages results by Created and keep the
// newest two: Docker reuses an existing image's Created timestamp whenever a
// build's layers are fully cache-hit (identical Dockerfile + build context
// content across commits), so a stale tag can carry a newer Created time than
// the tag that actually superseded it. Knowing current/previous explicitly
// from the pipeline's own state is the only reliable way to identify them.
//
// Best-effort: a delete failure (e.g. the image is still in use by a
// container qooqd hasn't torn down yet) is logged and skipped, never fails
// the pipeline — the upsert this build just performed has already succeeded.
func (w *Watcher) pruneOldImages(ctx context.Context, host imageHost, componentName, currentTag, previousCommit string) {
	images, err := host.c.ListImages(ctx)
	if err != nil {
		w.log.Warn("prune old images: list images failed", zap.String("component", componentName), zap.String("host", host.label), zap.Error(err))
		return
	}

	keep := map[string]bool{currentTag: true}
	if previousCommit != "" {
		keep[componentName+":"+shortSHA(previousCommit)] = true
	}

	prefix := componentName + ":"
	for _, img := range images {
		for _, tag := range img.Tags {
			if !strings.HasPrefix(tag, prefix) || keep[tag] {
				continue
			}
			if err := host.c.RemoveImage(ctx, tag); err != nil {
				w.log.Warn("prune old image failed (leaving in place)", zap.String("component", componentName), zap.String("tag", tag), zap.String("host", host.label), zap.Error(err))
				continue
			}
			w.log.Info("pruned old image", zap.String("component", componentName), zap.String("tag", tag), zap.String("host", host.label))
		}
	}
}

// buildDeployRequest merges t.DeploySpec (the operator's config) over
// manifestSpec (the manifest's defaults, zero value if there is no manifest)
// and assembles the client.DeployRequest — shared by both the container
// branch (image = built tag) and the M16 process branch (image = artifact
// path).
func (w *Watcher) buildDeployRequest(t *Target, image string, manifestSpec deploySpecFields) (client.DeployRequest, error) {
	spec, err := parseDeploySpec(t.DeploySpec)
	if err != nil {
		return client.DeployRequest{}, fmt.Errorf("parse deploy_spec: %w", err)
	}
	// Re-check the operator's mounts before the merge: the create-time check
	// in handleCreateTarget cannot see rows written before that check existed
	// (or edited straight in the DB), and this is the last point where a bad
	// mount is still just a failed build rather than a running container.
	if err := w.checkOperatorMounts(spec.Mounts); err != nil {
		return client.DeployRequest{}, fmt.Errorf("deploy_spec.mounts: %w", err)
	}
	spec = mergeDeploySpec(manifestSpec, spec)

	req := client.DeployRequest{
		Namespace:     spec.Namespace,
		Name:          t.ComponentName,
		Image:         image,
		Command:       spec.Command,
		Env:           spec.Env,
		Network:       spec.Network,
		GroupID:       spec.GroupID,
		RestartPolicy: spec.RestartPolicy,
		CPULimit:      spec.CPULimit,
		MemLimitBytes: spec.MemLimitBytes,
		Domain:        spec.Domain,
	}
	// A user's component always gets a network of its own, manifest or
	// not: without one it lands on the engine's default bridge, which every
	// other such container shares. Only account-qualified namespaces —
	// single-user installs keep the engine default, as they always have.
	if req.Network == "" && isTenantNamespace(req.Namespace) {
		req.Network = defaultAppNetwork(req.Namespace, req.Name)
	}
	// Last, on the merged spec: the server's default fills in what nothing
	// asked for, and its maximum binds manifest and operator alike.
	if req.CPULimit, req.MemLimitBytes, err = w.limits.apply(req.CPULimit, req.MemLimitBytes); err != nil {
		return client.DeployRequest{}, err
	}
	if spec.Ingress != nil {
		req.Ingress = &client.IngressSpec{
			Hosts:     spec.Ingress.Hosts,
			Port:      spec.Ingress.Port,
			Path:      spec.Ingress.Path,
			StripPath: spec.Ingress.StripPath,
			TLS:       spec.Ingress.TLS,
		}
	}
	for _, p := range spec.Ports {
		req.Ports = append(req.Ports, client.PortSpec{
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			Protocol:      p.Protocol,
			AutoHost:      p.AutoHost,
		})
	}
	for _, m := range spec.Mounts {
		req.Mounts = append(req.Mounts, client.MountSpec{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		})
	}
	if spec.HealthCheck != nil {
		req.HealthCheck = &client.HealthCheckSpec{
			Type:                spec.HealthCheck.Type,
			HTTPPath:            spec.HealthCheck.HTTPPath,
			HTTPPort:            spec.HealthCheck.HTTPPort,
			ExecCommand:         spec.HealthCheck.ExecCommand,
			IntervalSeconds:     spec.HealthCheck.IntervalSeconds,
			TimeoutSeconds:      spec.HealthCheck.TimeoutSeconds,
			FailureThreshold:    spec.HealthCheck.FailureThreshold,
			InitialDelaySeconds: spec.HealthCheck.InitialDelaySeconds,
		}
	}
	return req, nil
}

// recordSuccess marks t's build as successful at headHash — shared tail of
// both the container and process pipelines.
func (w *Watcher) recordSuccess(t *Target, headHash string) error {
	now := time.Now()
	t.LastCommit = headHash
	t.RejectedCommit = ""
	t.LastBuildStatus = BuildStatusSuccess
	t.LastBuildError = ""
	t.LastBuildAt = now
	t.ForceRebuild = false
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"last_commit":       t.LastCommit,
		"rejected_commit":   "",
		"last_build_status": t.LastBuildStatus,
		"last_build_error":  t.LastBuildError,
		"last_build_at":     t.LastBuildAt,
		"force_rebuild":     false,
		"topology":          t.Topology,
	}).Error; err != nil {
		return err
	}
	// A target that just built is no longer the broken one the give-up
	// mechanism exists to stop.
	if t.StackName == "" {
		w.clearBuildFailures(t)
	}
	return nil
}

// clearStaleFailure retires a lingering LastBuildStatus=failed once the remote
// has been re-checked successfully and reports nothing new to deploy.
//
// Reaching an "unchanged" branch with status=failed can only mean the failure
// came from the *detect* phase (resolving the remote HEAD or the image digest),
// never from the build/deploy phase: recordFailure deliberately leaves
// LastCommit/LastImageDigest untouched so the next sweep retries, which makes
// the remote compare as *changed* and routes back into the pipeline instead of
// here. So the failure being cleared is always a transient detect blip (a DNS
// hiccup, a brief registry outage) that a later sweep has since disproved —
// and the deployed component matches the current remote. Leaving it would pin
// that blip on the target until the next real commit, reading as "broken"
// while everything is in fact healthy.
//
// LastBuildAt is left alone: no build ran, so advancing it would imply one did.
// It keeps pointing at the transient failure's timestamp because recordFailure
// already overwrote the last successful build's time — that value is gone and
// cannot be honestly reconstructed here.
func (w *Watcher) clearStaleFailure(t *Target) {
	if t.LastBuildStatus != BuildStatusFailed {
		return
	}
	t.LastBuildStatus = BuildStatusSuccess
	t.LastBuildError = ""
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"last_build_status": t.LastBuildStatus,
		"last_build_error":  t.LastBuildError,
	}).Error; err != nil {
		w.log.Warn("failed to clear stale failure", zap.String("target", t.Name), zap.Error(err))
		return
	}
	// And the count of those failures, for the same reason the status goes:
	// what this function has just established is that they were blips, and the
	// give-up mechanism counts failures *in a row* — a successful check is the
	// end of the row. Leaving the number behind would let nine disproved
	// hiccups pause the target on the tenth, months later, under a sentence
	// telling its owner to fix a repository that has been building fine.
	w.clearBuildFailures(t)
	w.log.Info("cleared stale build failure after successful re-check", zap.String("target", t.Name))
}

// runProcessPipeline is the M16 build path for requests.runtime: process. It
// runs the manifest's build.command (if any) in the clone dir, verifies the
// resulting artifact, copies it into a versioned slot under artifacts.dir,
// and upserts a process-runtime component whose image is that artifact's
// absolute path.
//
// Security note (v1 stance): build.command is a
// repo-controlled, unsandboxed host command.
//
// That used to be described here as "the same trust decision as building its
// Dockerfile, just executed on the host instead of inside a container". It is
// not the same decision, and saying so was the mistake underneath the whole
// blocker: docker sandboxes a build, the host does not. Creating a target whose
// repository you do not control means that repository runs as the watcher.
// untrusted_repos refuses it outright — see trust.go.
func (w *Watcher) runProcessPipeline(ctx context.Context, t *Target, dir, headHash string, mf *manifest, manifestSpec deploySpecFields) error {
	if len(mf.Build.Command) > 0 {
		// Checked before running, not inside runBuildCommand, so the refusal
		// happens at the one place that decides to run a repository's command
		// at all — and so a future second caller has to make the same decision
		// explicitly rather than inherit it.
		if err := w.checkBuildCommandAllowed(); err != nil {
			return err
		}
		if err := w.runBuildCommand(ctx, t, dir, mf.Build.Command); err != nil {
			return fmt.Errorf("build.command: %w", err)
		}
	}

	artifactRel := mf.Build.Artifact
	if artifactRel == "" {
		return fmt.Errorf("qooq.yaml: build.artifact is required when requests.runtime is \"process\"")
	}
	if !validTargetPath(artifactRel) {
		return fmt.Errorf("qooq.yaml: build.artifact %q escapes the repo root", artifactRel)
	}
	srcPath, err := resolveWithin(dir, artifactRel)
	if err != nil {
		return fmt.Errorf("build.artifact %q: %w", artifactRel, err)
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("build.artifact %q not found after build: %w", artifactRel, err)
	}
	if info.Mode()&0o111 == 0 {
		if err := os.Chmod(srcPath, info.Mode()|0o755); err != nil {
			return fmt.Errorf("build.artifact %q is not executable and chmod +x failed: %w", artifactRel, err)
		}
	}

	artifactsDir := w.artifactsDir
	if artifactsDir == "" {
		artifactsDir = defaultArtifactsDir
	}
	absArtifactsDir, err := filepath.Abs(artifactsDir)
	if err != nil {
		return fmt.Errorf("resolve artifacts.dir %q: %w", artifactsDir, err)
	}

	componentDir := filepath.Join(absArtifactsDir, artifactDirName(namespaceOf(t), t.ComponentName))
	destDir := filepath.Join(componentDir, shortSHA(headHash))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create artifact dir %s: %w", destDir, err)
	}
	destPath := filepath.Join(destDir, filepath.Base(artifactRel))
	if err := copyExecutable(srcPath, destPath); err != nil {
		return fmt.Errorf("copy artifact to %s: %w", destPath, err)
	}

	if len(mf.Run.Ports) > 0 {
		w.log.Info("qooq.yaml run.ports has no effect under the process runtime, ignoring", zap.String("target", t.Name))
	}

	// Same resolution as the container branch. The process adapter has
	// nothing to bind-mount, so it copies/links the mounts into the
	// instance's working directory instead (internal/runtime/process).
	t.Topology = ""
	manifestSpec, err = w.applyRunConfig(ctx, t, mf, manifestSpec, dir, shortSHA(headHash))
	if err != nil {
		return fmt.Errorf("qooq.yaml: %w", err)
	}

	req, err := w.buildDeployRequest(t, destPath, manifestSpec)
	if err != nil {
		return err
	}

	comp, err := w.client.Upsert(ctx, req)
	if err != nil {
		return fmt.Errorf("upsert component (artifact %s): %w", destPath, notTheDaemon(err))
	}
	if err := w.verifyOrRollBack(ctx, t, comp, headHash); err != nil {
		return err
	}

	if err := w.recordSuccess(t, headHash); err != nil {
		return err
	}

	w.pruneOldArtifacts(componentDir, t.Name)
	w.pruneOldFileMounts(w.componentRefFor(t))
	return nil
}

// runBuildCommand runs command in dir with a 10-minute ceiling, inheriting
// the watcher process's own environment (HOME/PATH etc. pass through
// unmodified — this is a host command, not a container, so there is no
// separate env to construct). stdout+stderr are combined; on success only a
// short log line is emitted, on failure the last ~500 bytes of combined
// output (credential-masked) become the error, which the caller wraps into
// LastBuildError.
func (w *Watcher) runBuildCommand(ctx context.Context, t *Target, dir string, command []string) error {
	buildCtx, cancel := context.WithTimeout(ctx, processBuildTimeout)
	defer cancel()

	cmd := exec.CommandContext(buildCtx, command[0], command[1:]...)
	cmd.Dir = dir
	// Not os.Environ(): that handed this repository's own command
	// QOOQ_WATCHER_SECRET_KEY — the passphrase decrypting every user's
	// credentials — and every QOOQ_SECRET_*. See trust.go.
	cmd.Env = buildCommandEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	output := out.String()
	if err != nil {
		tail := output
		if len(tail) > 500 {
			tail = tail[len(tail)-500:]
		}
		tail = maskRepoCredentials(tail, t.RepoURL)
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(tail))
	}
	w.log.Info("build.command succeeded",
		zap.String("target", t.Name),
		zap.Strings("command", command),
		zap.Int("output_bytes", len(output)))
	return nil
}

// copyExecutable copies src to dst (creating/truncating dst) and marks it
// executable (0755), matching the mode the artifact must have to be started
// as a process runtime component.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o755)
}

// pruneOldArtifacts keeps only the two most-recently-modified sha
// subdirectories of componentDir (current + previous, mirroring the
// PreviousImage retention semantics elsewhere in qooq) and removes the rest.
// Best-effort: failures are logged, not returned, since the deploy this
// build just performed has already succeeded.
func (w *Watcher) pruneOldArtifacts(componentDir, targetName string) {
	removed, err := pruneOldShaDirs(componentDir, keepShaDirs)
	if err != nil {
		w.log.Warn("prune old artifacts failed", zap.String("target", targetName), zap.Error(err))
	}
	for _, path := range removed {
		w.log.Info("pruned old artifact dir", zap.String("target", targetName), zap.String("dir", path))
	}
}

// loadManifest reads and parses the M14 app manifest for t out of the
// freshly cloned repo at cloneDir. Returns (nil, nil) when manifest parsing
// is disabled (t.ManifestPath == "-") or the file simply doesn't exist —
// both cases behave exactly like M12 (no manifest, deploy_spec is the only
// input). A non-nil error means the manifest exists but is invalid, which
// the caller turns into a pipeline failure.
func (w *Watcher) loadManifest(cloneDir string, t *Target) (*manifest, error) {
	if t.ManifestPath == "-" {
		return nil, nil
	}
	p := t.ManifestPath
	if p == "" {
		p = "qooq.yaml"
	}
	if !filepath.IsLocal(p) {
		return nil, fmt.Errorf("manifest_path %q escapes the repo root", p)
	}
	full := filepath.Join(cloneDir, p)
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", p, err)
	}
	// F9 (M17 review): cap manifest size before reading it — yaml.Unmarshal
	// on an attacker-controlled repo's multi-hundred-MB "qooq.yaml" would
	// otherwise burn memory/CPU every sweep tick for what is meant to be a
	// small hand-written config file.
	if fi.Size() > maxManifestBytes {
		return nil, fmt.Errorf("%s is %d bytes, exceeds the %d byte (1MiB) manifest size limit", p, fi.Size(), maxManifestBytes)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	m, err := parseManifest(data)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// checkRequests validates a manifest's requests section (including, since
// M16, requests.runtime) against the host capacity reported by qooqd's GET
// /v1/system/capacity. If the capacity call itself fails (e.g. an older
// qooqd that predates M14), this only warns and skips validation — a
// capacity-unaware qooqd must not block every deploy.
func (w *Watcher) checkRequests(ctx context.Context, t *Target, req manifestRequests) error {
	capResp, err := w.client.Capacity(ctx)
	if err != nil {
		// F8 (M17 review): a manifest with an explicit requests.runtime is
		// making a hard claim about which qooqd runtime it needs
		// (container vs process, which are not interchangeable — a process
		// artifact will not run under -runtime docker and vice versa). If
		// the capacity check itself fails we have no way to confirm that
		// claim, so silently skipping validation here would let a
		// process-runtime target build/deploy against a plain-container
		// qooqd (or the reverse) with a confusing failure much later. Only
		// cpu/mem/arch requirements keep the old warn+skip (they're
		// independent of API surface changes, so a capacity call failing
		// against an older qooqd is a legitimate "can't check" case there).
		if req.Runtime != "" {
			return fmt.Errorf("requests.runtime %q set but qooqd runtime could not be confirmed: %w", req.Runtime, notTheDaemon(err))
		}
		w.log.Warn("qooq.yaml requests: capacity check unavailable, skipping validation", zap.String("target", t.Name), zap.Error(err))
		return nil
	}

	// requests.runtime: "" and "container" both mean "qooqd must be running
	// the docker runtime"; "process" means qooqd must be -runtime process.
	// capResp.Runtime == "" means an older qooqd that predates M16 — skip
	// this one check (defensive; in practice a qooqd new enough to answer
	// /v1/system/capacity at all is new enough to set Runtime too) rather
	// than treating "unknown" as a mismatch.
	if capResp.Runtime != "" {
		wantRuntime := req.Runtime
		if wantRuntime == "" {
			wantRuntime = "container"
		}
		gotRuntime := "container"
		if capResp.Runtime == "process" {
			gotRuntime = "process"
		}
		if wantRuntime != gotRuntime {
			return fmt.Errorf("requests.runtime %q, qooqd is running runtime %q", wantRuntime, capResp.Runtime)
		}
	}

	if req.CPU > 0 && req.CPU > float64(capResp.CPUCores) {
		return fmt.Errorf("requests requires %g cpu cores, host has %d", req.CPU, capResp.CPUCores)
	}
	if req.Memory != "" {
		want, err := parseManifestMemBytes(req.Memory)
		if err != nil {
			return fmt.Errorf("requests.memory: %w", err)
		}
		if want > capResp.MemoryBytes {
			return fmt.Errorf("requests requires %s memory (%d bytes), host has %d bytes", req.Memory, want, capResp.MemoryBytes)
		}
	}
	arch := effectiveArch(req.Arch, w.defaultArch)
	if len(arch) > 0 {
		ok := false
		hostArch := normalizeArch(capResp.Arch)
		for _, a := range arch {
			if normalizeArch(a) == hostArch {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("requests requires arch %v, host is %s", arch, capResp.Arch)
		}
	}
	return nil
}

// effectiveArch resolves the arch list actually enforced by checkRequests:
// the manifest's requests.arch wins whenever it's non-empty; otherwise, if
// the watcher has a configured defaultArch (requests.default_arch, see
// module.go), that single value is enforced instead. An empty defaultArch
// (the v1 default) means no fallback — a manifest with no requests.arch and
// no configured default enforces nothing, matching the pre-M21 behavior.
func effectiveArch(reqArch []string, defaultArch string) []string {
	if len(reqArch) > 0 || defaultArch == "" {
		return reqArch
	}
	return []string{defaultArch}
}

// normalizeArch maps common architecture spellings onto Go's GOARCH names
// (F1, M17 review) so a manifest written with the uname -m style names
// (x86_64, aarch64, armv7l, ...) isn't wrongly rejected against a host
// reporting Go's runtime.GOARCH (amd64, arm64, arm, ...) — checkRequests
// previously compared the two case-insensitively but without any alias
// table, so e.g. requests.arch: [aarch64] always failed on an arm64 host.
// Unrecognized values pass through unchanged (lowercased) so an exact match
// still works and an unknown value still reports a clear mismatch instead
// of being silently accepted.
func normalizeArch(a string) string {
	switch strings.ToLower(a) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "x86", "i386", "386":
		return "386"
	case "armv7l", "arm":
		return "arm"
	default:
		return strings.ToLower(a)
	}
}

func parseDeploySpec(raw string) (deploySpecFields, error) {
	var spec deploySpecFields
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			return spec, err
		}
	}
	// Every read of a stored deploy spec goes through here, so filling in the
	// default at this one point is what keeps the watcher's idea of a
	// component's identity identical to the daemon's. They have to agree
	// exactly: findComponent matches on (namespace, name), and a watcher
	// holding "" against a daemon holding "default" would fail to recognize
	// its own component and upsert it again on every sweep.
	spec.Namespace = client.NormalizeNamespace(spec.Namespace)
	return spec, nil
}

// setBuilding marks a target as currently building at revision (the commit
// hash for a source target, the image digest for an image one), best-effort
// (errors are logged, not fatal — the pipeline result update is what matters).
//
// Clearing LastBuildError here is the point. It used to be left alone, so a
// target that had failed kept serving the old error text for the whole of the
// next build — which reads exactly like the new build failing the same way,
// and is worse than no error at all because it is specific and wrong. The
// error belongs to a finished attempt; starting another one retires it.
func (w *Watcher) setBuilding(t *Target, revision string) {
	t.LastBuildStatus = BuildStatusBuilding
	t.LastBuildError = ""
	t.LastBuildRevision = revision
	t.BuildGeneration++
	// Stamped at the start, which is the only moment it can be. LastBuildAt is
	// written when a build ENDS, so during one it still describes the previous
	// build — measuring elapsed time from it would confidently report the idle
	// hours since then.
	t.BuildStartedAt = time.Now()
	// The previous build's output goes with it. A build that fails before
	// building (qooq.yaml over the limits, a missing input) would otherwise
	// show the last build's Step lines — read as "it built again, then
	// failed" (measured: seven stale lines under a refusal that built nothing).
	t.LastBuildLog = ""
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"last_build_status":   t.LastBuildStatus,
		"last_build_error":    t.LastBuildError,
		"last_build_revision": t.LastBuildRevision,
		"build_generation":    t.BuildGeneration,
		"build_started_at":    t.BuildStartedAt,
		"last_build_log":      "",
	}).Error; err != nil {
		w.log.Warn("failed to mark target building", zap.String("target", t.Name), zap.Error(err))
	}
}

// collapseRepeatedPrefix drops a "segment: " that repeats the one right before
// it. Errors are wrapped layer by layer and two layers sometimes say the same
// thing — "build failed: build failed:", "qooq.yaml: qooq.yaml:" — and each
// time one was fixed at its source another appeared (three so far). This is
// where every message reaches the user, so it is enforced once, here.
func collapseRepeatedPrefix(msg string) string {
	parts := strings.Split(msg, ": ")
	out := parts[:1]
	for _, p := range parts[1:] {
		if p == out[len(out)-1] {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ": ")
}

// recordNeedsInput persists a deploy blocked on missing input values. Like
// recordFailure it leaves LastCommit alone so the next sweep re-evaluates —
// which is what makes supplying the value take effect without the repo having
// to change. LastBuildAt is not touched either: no build ran.
func (w *Watcher) recordNeedsInput(t *Target, msg string, waitingOn []string) {
	msg = collapseRepeatedPrefix(msg) // guarded by TestARepeatedPrefixIsSaidOnce's round trip
	t.LastBuildStatus = BuildStatusNeedsInput
	t.LastBuildError = msg
	t.WaitingOn = strings.Join(waitingOn, ",")
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"last_build_status": BuildStatusNeedsInput,
		"last_build_error":  msg,
		"waiting_on":        t.WaitingOn,
	}).Error; err != nil {
		w.log.Warn("failed to persist needs-input state", zap.String("target", t.Name), zap.Error(err))
	}
}

// recordDeclaredInputs caches the manifest's inputs: block on the target, so
// the API can report what this deployment needs without cloning the repo. It
// runs on every successful parse, so removing an input from the manifest
// removes it here too.
func (w *Watcher) recordDeclaredInputs(t *Target, inputs map[string]manifestInput) {
	var encoded string
	if len(inputs) > 0 {
		data, err := json.Marshal(inputs)
		if err != nil {
			w.log.Warn("failed to encode declared inputs", zap.String("target", t.Name), zap.Error(err))
			return
		}
		encoded = string(data)
	}
	if encoded == t.DeclaredInputs {
		return
	}
	t.DeclaredInputs = encoded
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Update("declared_inputs", encoded).Error; err != nil {
		w.log.Warn("failed to persist declared inputs", zap.String("target", t.Name), zap.Error(err))
	}
}

// recordFailure persists a failed build/detect outcome without touching
// LastCommit, so the next sweep retries the same commit. The message is
// scrubbed of any credential embedded in t.RepoURL before it is persisted or
// logged.
//
// It takes an error rather than a string so the cause survives as far as the
// one decision that needs more than its text: whether this failure counts
// toward giving up on the repository. Callers wrap with %w for the same reason
// — flattening to a string at the call site is how pkg/client and
// pkg/watcherclient both ended up matching on message text, and the correction
// was the same one both times.
func (w *Watcher) recordFailure(t *Target, cause error) {
	msg := collapseRepeatedPrefix(maskRepoCredentials(cause.Error(), t.RepoURL)) // guarded by a round trip, as above
	now := time.Now()
	t.LastBuildStatus = BuildStatusFailed
	t.LastBuildError = msg
	t.LastBuildAt = now
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"last_build_status": BuildStatusFailed,
		"last_build_error":  msg,
		"last_build_at":     now,
	}).Error; err != nil {
		w.log.Warn("failed to persist failure", zap.String("target", t.Name), zap.Error(err))
	}

	// Stack-owned targets are counted and paused by the stack layer, which
	// pauses the whole stack rather than one project. Two mechanisms acting on
	// the same counter would fight over it.
	if t.StackName != "" {
		return
	}
	// A failure one of qooq's own dependencies caused is recorded, but is not
	// evidence about this repository and must not march it toward a pause that
	// blames it. See errNotTheRepository.
	if isNotTheRepository(cause) {
		return
	}
	if reason := w.noteBuildFailure(t); reason != "" {
		w.log.Warn("target paused after repeated build failures",
			zap.String("target", t.Name), zap.String("reason", reason))
	}
}

// recordBuildLog stores the most recent build's output for this target.
//
// Only the most recent. A history would need a retention policy, a store that
// is not this row, and a way to browse it — and the question people actually
// have is about the build that just happened. When history is wanted, it can be
// added behind the same endpoint without changing what a caller sees.
//
// Failures to persist are logged and swallowed: the build's own outcome must
// not depend on whether its log could be written down.
func (w *Watcher) recordBuildLog(t *Target, log string) {
	if log == "" {
		return
	}
	log = maskRepoCredentials(log, t.RepoURL)
	t.LastBuildLog = log
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).
		Update("last_build_log", log).Error; err != nil {
		w.log.Warn("could not store the build log",
			zap.String("target", t.Name), zap.Error(err))
	}
}

// componentIsGone reports whether the component this target deploys has
// disappeared from qooqd.
//
// This is the "desired state flows down" half of the storage design,
// and the half that is already wrong on a single machine. Both unchanged branches — source mode comparing the remote
// HEAD, image mode comparing the digest — decide what to do purely from
// "has the source changed", and never from "is what I deployed still there".
// So if qooqd loses its bbolt (a replaced node, a lost disk, a stray
// `qooq component rm`), the watcher concludes there is no new commit and does
// nothing, for ever, while the target keeps reporting last_build_status =
// success. The app is simply gone and no field anywhere says so.
//
// The three-way answer from findComponent is what makes this safe to act on:
//
//	comp != nil             — it is there; nothing to do
//	comp == nil, err == nil — genuinely absent; the deployment must be restored
//	err != nil              — qooqd could not be asked; say nothing
//
// The last case is the important one. Treating "cannot reach qooqd" as "the
// component is gone" would rebuild every target on this watcher the moment the
// daemon restarts — the same best-effort stance runtimestate.go takes for the
// same reason.
//
// A component the operator stopped on purpose is *present* with
// DesiredState=Stopped, so it is not touched here. Only outright absence
// counts: this restores a deployment, it does not overrule anybody.
func (w *Watcher) componentIsGone(ctx context.Context, t *Target) bool {
	// Reaching an unchanged branch with a non-success status means the failure
	// came from the detect phase (recordFailure leaves LastCommit alone, so a
	// build-phase failure compares as changed and never lands here). Nothing
	// was deployed on this pass, so absence says nothing about drift.
	// A stack's project is deployed by the stack walk, in the order its
	// manifest declares — which is why dueTargets excludes stack-owned rows at
	// all. A project that redeployed itself the moment its component went
	// missing would do so out of that order, which is the same disorder that
	// exclusion exists to prevent. Restoring a stack's projects is the stack
	// layer's job; it is not done yet, and saying so is better than doing it
	// here in a way that fights the ordering.
	if t.StackName != "" {
		return false
	}
	if t.LastBuildStatus != BuildStatusSuccess {
		return false
	}
	// No client configured means there is nobody to ask, which is the same
	// answer as a daemon that will not reply: say nothing. This is not only a
	// test concern — a Watcher assembled without qooqd.addr would otherwise
	// panic here, inside the sweep goroutine, and take the process with it.
	if w.client == nil {
		return false
	}
	comp, err := w.findComponent(ctx, w.componentRefFor(t))
	if err != nil {
		return false
	}
	return comp == nil
}
