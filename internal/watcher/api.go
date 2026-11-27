package watcher

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// defaultAllowedHosts derives the Host-header allowlist from the bound port:
// the three canonical localhost forms qooq-watcher is reachable at.
func defaultAllowedHosts(port int) []string {
	p := strconv.Itoa(port)
	return []string{
		"127.0.0.1:" + p,
		"localhost:" + p,
		"[::1]:" + p,
	}
}

// errUnsafeTargetPath is returned when ContextSubdir or Dockerfile attempts to
// escape the cloned repo root (absolute path or a path containing "..").
// filepath.IsLocal rejects both, plus other traversal edge cases (Windows
// volume names, empty-after-clean, etc.) without qooq having to enumerate
// them itself. An empty string is always allowed (it means "use the default").
var errUnsafeTargetPath = errors.New("must be a relative path inside the repo (no absolute path or \"..\")")

func validTargetPath(p string) bool {
	if p == "" {
		return true
	}
	return filepath.IsLocal(p)
}

// validManifestPath allows everything validTargetPath does, plus the "-"
// sentinel that opts a target out of M14 manifest parsing entirely.
func validManifestPath(p string) bool {
	if p == "-" {
		return true
	}
	return validTargetPath(p)
}

// registerRoutes wires the targets CRUD + trigger endpoints onto the shared
// gin engine (the same *gin.Engine the http_server module already listens
// on for /healthz etc).
//
// Auth (M18): when w.httpToken is non-empty, bearerAuth(w.httpToken) is
// prepended as the first handler on every /v1/targets* route below. It is
// attached per-route (not via r.Use on the shared engine) so it can never
// leak onto /healthz/:ready (registered by a different module entirely) or
// onto /v1/webhook/:name, which is deliberately exempt: external git hosts
// (GitHub/GitLab) call that endpoint directly and authenticate via their own
// HMAC signature (see webhook.go) — they have no way to attach this bearer
// token.
func (w *Watcher) registerRoutes(r *gin.Engine) {
	protect := w.protectChain()

	// gin's HandlersChain must be appended to a fresh slice each time so the
	// per-route final handler doesn't clobber a shared backing array.
	chain := func(final gin.HandlerFunc) []gin.HandlerFunc {
		out := make([]gin.HandlerFunc, 0, len(protect)+1)
		out = append(out, protect...)
		return append(out, final)
	}

	r.POST("/v1/targets", chain(w.handleCreateTarget)...)
	r.GET("/v1/targets", chain(w.handleListTargets)...)
	r.GET("/v1/targets/:name", chain(w.handleGetTarget)...)
	r.PUT("/v1/targets/:name", chain(w.handleUpdateTarget)...)
	r.DELETE("/v1/targets/:name", chain(w.handleDeleteTarget)...)
	r.DELETE("/v1/targets/:name/services/:service", chain(w.handleDeleteDroppedService)...)
	r.POST("/v1/targets/:name/trigger", chain(w.handleTriggerTarget)...)
	// The three things you can do to a deployment that are not "build it
	// again". See lifecycle.go for why they are three verbs and not one.
	r.POST("/v1/targets/:name/stop", chain(w.handleStopTarget)...)
	r.POST("/v1/targets/:name/rollback", chain(w.handleRollbackTarget)...)
	r.POST("/v1/targets/:name/start", chain(w.handleStartTarget)...)
	r.POST("/v1/targets/:name/pause", chain(w.handlePauseTarget)...)
	r.POST("/v1/targets/:name/resume", chain(w.handleResumeTarget)...)

	// Per-namespace secrets (M3). Behind the same auth as everything else: a
	// write here sets a value a deploy will resolve, so it is at least as
	// sensitive as creating a target.
	//
	// There is no endpoint that returns a value. Listing gives names only —
	// a store that can read its own secrets back is one request away from
	// being a credential dump, which is the same stance CreateAPIToken takes
	// in the external manager.
	r.GET("/v1/namespaces/:ns/secrets", chain(w.handleListSecrets)...)
	r.PUT("/v1/namespaces/:ns/secrets/:name", chain(w.handlePutSecret)...)
	r.DELETE("/v1/namespaces/:ns/secrets/:name", chain(w.handleDeleteSecret)...)
	r.POST("/v1/targets/:name/deploy", chain(w.handleDeployTarget)...)
	r.GET("/v1/targets/:name/manifest", chain(w.handleGetManifest)...)
	r.GET("/v1/targets/:name/build-log", chain(w.handleGetBuildLog)...)
	r.GET("/v1/targets/:name/logs", chain(w.handleAppLogs)...)
	r.GET("/v1/usage/disk", chain(w.handleDiskUsage)...)
	r.GET("/v1/usage/components", chain(w.handleComponentUsage)...)
	r.PUT("/v1/targets/:name/quota-hold", chain(w.handleQuotaHold)...)
	r.PUT("/v1/targets/:name/manifest", chain(w.handlePutManifest)...)
	// The body is a tar.gz stream, not JSON, so the JSON-only Content-Type
	// rule the other writes enforce does not apply (upload.go checks its own).
	r.PUT("/v1/targets/:name/source", chain(w.handleUploadSource)...)
	r.PUT("/v1/targets/:name/inputs", chain(w.handleSetInputs)...)
	r.POST("/v1/stacks", chain(w.handleCreateStack)...)
	r.GET("/v1/stacks", chain(w.handleListStacks)...)
	r.GET("/v1/stacks/:name", chain(w.handleGetStack)...)
	r.DELETE("/v1/stacks/:name", chain(w.handleDeleteStack)...)
	r.GET("/v1/stacks/:name/manifest", chain(w.handleGetStackManifest)...)
	r.POST("/v1/stacks/:name/trigger", chain(w.handleTriggerStack)...)
	r.POST("/v1/stacks/:name/resume", chain(w.handleResumeStack)...)

	r.GET("/v1/backup", chain(w.handleBackup)...)
	r.POST("/v1/webhook/:name", w.handleWebhook) // exempt, see doc comment above

	// Docs are unauthenticated (like /healthz): they describe API shape, not data.
	r.GET("/openapi.yaml", w.handleOpenAPIYAML)
	r.GET("/docs", w.handleDocsUI)
}

// protectChain returns the (possibly empty) gin.HandlersChain prepended to
// every /v1/targets* and /v1/backup route. When w.httpToken is set, that's
// bearer auth. When it's unset (no auth, the pre-M18 default), it's instead a
// Host-header guard against browser drive-by requests to localhost (bearer
// auth already covers that case, so the two are mutually exclusive). The
// webhook endpoint is deliberately not in this chain — it authenticates via
// its own HMAC signature and external git hosts send arbitrary Host headers.
func (w *Watcher) protectChain() gin.HandlersChain {
	if w.httpToken != "" {
		return gin.HandlersChain{bearerAuth(w.httpToken)}
	}
	if len(w.allowedHosts) > 0 {
		return gin.HandlersChain{hostGuard(w.allowedHosts)}
	}
	return nil
}

// hostGuard rejects any request whose Host header is not an exact allowlist
// match. Mirrors qooqd's internal/api hostGuard; only mounted when no bearer
// token is configured (see protectChain).
func hostGuard(allowed []string) gin.HandlerFunc {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		if a != "" {
			set[a] = struct{}{}
		}
	}
	return func(c *gin.Context) {
		if _, ok := set[c.Request.Host]; !ok {
			respondErr(c, http.StatusForbidden, errors.New("Host header not allowed; if you are accessing qooq-watcher through a proxy or non-localhost name, set http.allowed_hosts/QOOQ_WATCHER_HTTP_ALLOWED_HOSTS or enable http.token auth"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// bearerAuth rejects any request whose Authorization header is not exactly
// "Bearer <token>", using a constant-time comparison on the token itself —
// mirrors qooqd's internal/api/router.go bearerAuth.
func bearerAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		const prefix = "Bearer "
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, prefix) {
			respondErr(c, http.StatusUnauthorized, errors.New("missing bearer token"))
			c.Abort()
			return
		}
		got := strings.TrimPrefix(h, prefix)
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			respondErr(c, http.StatusUnauthorized, errors.New("invalid bearer token"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func respondErr(c *gin.Context, status int, err error) {
	c.JSON(status, errorResponse{Error: err.Error()})
}

func (w *Watcher) handleCreateTarget(c *gin.Context) {
	// Content-Type must be application/json (c.ContentType strips any
	// "; charset=..." suffix). Blocks browser no-cors POSTs, which can't set
	// this header to application/json.
	if c.ContentType() != "application/json" {
		respondErr(c, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}
	var req CreateTargetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		respondErr(c, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	if req.ComponentName == "" {
		respondErr(c, http.StatusBadRequest, errors.New("component_name is required"))
		return
	}

	deployMode := req.DeployMode
	if deployMode == "" {
		deployMode = DeployModeSource
	}
	if deployMode != DeployModeSource && deployMode != DeployModeImage && deployMode != DeployModeUpload {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("deploy_mode must be %q, %q or %q", DeployModeSource, DeployModeImage, DeployModeUpload))
		return
	}

	// M20a: the two modes' fields are mutually exclusive. "image" mode has no
	// repo to clone/build from (image_ref replaces it); "source" mode has no
	// registry ref to poll.
	if deployMode == DeployModeUpload {
		// The source arrives over PUT /v1/targets/{name}/source, so there is
		// nothing to clone and nothing to authenticate to. Refusing these
		// fields rather than ignoring them: a target that says it watches a
		// repository and never looks at it is a lie in the API.
		if req.RepoURL != "" || req.Branch != "" || req.ImageRef != "" ||
			(req.AuthType != "" && req.AuthType != AuthTypeNone) || req.AuthCredential != "" {
			respondErr(c, http.StatusBadRequest, errors.New("repo_url/branch/image_ref/auth_type/auth_credential must not be set when deploy_mode is \"upload\""))
			return
		}
		if !validUploadTargetName(req.Name) {
			respondErr(c, http.StatusBadRequest, fmt.Errorf("name %q: an upload target's name must be letters, digits, '.', '_' or '-' (it names a directory)", req.Name))
			return
		}
	} else if deployMode == DeployModeImage {
		if req.ImageRef == "" {
			respondErr(c, http.StatusBadRequest, errors.New("image_ref is required when deploy_mode is \"image\""))
			return
		}
		if req.RepoURL != "" || req.Branch != "" || req.Dockerfile != "" || req.ContextSubdir != "" || req.ManifestPath != "" {
			respondErr(c, http.StatusBadRequest, errors.New("repo_url/branch/dockerfile/context_subdir/manifest_path must not be set when deploy_mode is \"image\""))
			return
		}
	} else {
		if req.RepoURL == "" {
			respondErr(c, http.StatusBadRequest, errors.New("repo_url is required"))
			return
		}
		if err := w.checkRepoURLAllowed(req.RepoURL); err != nil {
			respondErr(c, http.StatusBadRequest, err)
			return
		}
		if req.ImageRef != "" {
			respondErr(c, http.StatusBadRequest, errors.New("image_ref must not be set when deploy_mode is \"source\""))
			return
		}
		if req.Branch != "" {
			if err := validateBranch(req.Branch); err != nil {
				respondErr(c, http.StatusBadRequest, err)
				return
			}
		}
	}

	if !validTargetPath(req.ContextSubdir) {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("context_subdir: %w", errUnsafeTargetPath))
		return
	}
	if !validTargetPath(req.Dockerfile) {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("dockerfile: %w", errUnsafeTargetPath))
		return
	}
	if !validManifestPath(req.ManifestPath) {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("manifest_path: %w", errUnsafeTargetPath))
		return
	}
	if len(req.DeploySpec) > 0 {
		spec, err := parseDeploySpec(string(req.DeploySpec))
		if err != nil {
			respondErr(c, http.StatusBadRequest, errors.New("deploy_spec is not valid JSON: "+err.Error()))
			return
		}
		// Fail here rather than at the first sweep: a rejected mount is an
		// operator typo/policy problem, and finding out now beats finding out
		// in last_build_error several minutes later.
		if err := w.checkOperatorMounts(spec.Mounts); err != nil {
			respondErr(c, http.StatusBadRequest, fmt.Errorf("deploy_spec.mounts: %w", err))
			return
		}
	}
	if req.PollIntervalSeconds != 0 && req.PollIntervalSeconds < minPollIntervalSeconds {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("poll_interval_seconds must be 0 (default %ds) or at least %ds", defaultPollIntervalSeconds, minPollIntervalSeconds))
		return
	}

	// Refused here rather than at the first build. Nothing about a target row
	// reveals that the credential it needs can never be fetched, so without
	// this the mistake surfaces as a clone failure minutes later, with an
	// error about a external manager the operator did not know was involved.
	if req.AuthType == AuthTypeGitHubApp && w.control == nil {
		respondErr(c, http.StatusBadRequest, fmt.Errorf(
			"auth_type=%s needs a control plane; set %s (this watcher has none, so use auth_type=token or ssh-key)",
			AuthTypeGitHubApp, configControlAddr))
		return
	}

	// M18: encrypt at rest when a secret key is configured; encryptCredential
	// is a no-op passthrough when w.secretKey is empty (v1 plaintext stance).
	authCredential, err := encryptCredential(req.AuthCredential, w.secretKey)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("encrypt auth_credential: %w", err))
		return
	}
	webhookSecret, err := encryptCredential(req.WebhookSecret, w.secretKey)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("encrypt webhook_secret: %w", err))
		return
	}

	// Inputs supplied up front. The manifest has not been read yet (that needs
	// a clone), so nothing is known about which of these are secret —
	// storeInputValues encrypts undeclared values for exactly this case.
	inputValues, err := w.storeInputValues(req.Inputs, nil)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("encrypt inputs: %w", err))
		return
	}

	t := &Target{
		Name:                 req.Name,
		DeployMode:           deployMode,
		RepoURL:              req.RepoURL,
		Branch:               req.Branch,
		AuthType:             req.AuthType,
		AuthCredential:       authCredential,
		Dockerfile:           req.Dockerfile,
		ContextSubdir:        req.ContextSubdir,
		ManifestPath:         req.ManifestPath,
		ComponentName:        req.ComponentName,
		AllowDuplicate:       req.AllowDuplicate,
		PublishDetectedPorts: req.PublishDetectedPorts,
		BuildGroup:           req.BuildGroup,
		ImageRef:             req.ImageRef,
		DeploySpec:           string(req.DeploySpec),
		PollIntervalSeconds:  req.PollIntervalSeconds,
		WebhookSecret:        webhookSecret,
		InputValues:          inputValues,
	}

	if err := createTarget(w.db, t); err != nil {
		if errors.Is(err, ErrTargetNameConflict) || errors.Is(err, ErrComponentClaimed) ||
			errors.Is(err, ErrComponentDuplicate) {
			respondErr(c, http.StatusConflict, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusCreated, t.toResponse())
}

func (w *Watcher) handleListTargets(c *gin.Context) {
	ts, err := listTargets(w.db)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	out := make([]TargetResponse, 0, len(ts))
	for i := range ts {
		out = append(out, ts[i].toResponse())
	}
	w.attachRuntimeState(c.Request.Context(), out)
	c.JSON(http.StatusOK, out)
}

func (w *Watcher) handleGetTarget(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	resp := t.toResponse()
	one := []TargetResponse{resp}
	w.attachRuntimeState(c.Request.Context(), one)
	c.JSON(http.StatusOK, one[0])
}

// handleDeleteTarget removes the target AND what it deployed.
//
// It used to remove only the row, which left the container running, the address
// answering, and the project absent from every listing — so the deployment
// nobody wanted was also one nobody could find again. `?keep_component=1`
// asks for the old behaviour, for the case it was right for: moving a
// deployment under something else's management without taking it down.
//
// The component goes first. If the daemon cannot be reached the row survives,
// so the project is still listed and the removal can be retried — the opposite
// order is how the orphan appears in the first place.
func (w *Watcher) handleDeleteTarget(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if c.Query("keep_component") != "1" {
		if err := w.removeComponentFor(c.Request.Context(), t); err != nil {
			respondErr(c, http.StatusBadGateway, err)
			return
		}
	}
	if err := deleteTargetByName(w.db, c.Param("name")); err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	// After the row, for the same reason the component goes before it: the
	// order that can fail must not leave something nobody can find. A stored
	// archive with no target is exactly that.
	if t.DeployMode == DeployModeUpload {
		w.removeStoredUploads(t.Name)
	}
	w.releaseReservation(c.Request.Context(), t)
	c.Status(http.StatusNoContent)
}

// handleStopTarget and handleStartTarget act on the process, leaving everything
// else — the target, its settings, its images — exactly where it was.
func (w *Watcher) handleStopTarget(c *gin.Context) {
	w.lifecycleAction(c, (*Watcher).stopComponentFor)
}

func (w *Watcher) handleStartTarget(c *gin.Context) {
	w.lifecycleAction(c, (*Watcher).startComponentFor)
}

// handleRollbackTarget swaps the running version for the one before it.
//
// It does not wait for the target's lock: that is held for the whole of a
// build, and a request that hangs for minutes reads as a broken button.
func (w *Watcher) handleRollbackTarget(c *gin.Context) {
	unlock, ok := w.tryLockTarget(c.Param("name"))
	if !ok {
		respondErr(c, http.StatusLocked, errDeployInProgress)
		return
	}
	defer unlock()
	w.lifecycleAction(c, (*Watcher).rollbackComponentFor)
}

// lifecycleAction is the shared shape of stop and start: find the target, do
// the thing, and say which kind of failure it was.
//
// errNoComponent is a 409 rather than a 404: the target is there, and 404 would
// read as "no such project" to a caller who just listed it.
func (w *Watcher) lifecycleAction(c *gin.Context, do func(*Watcher, context.Context, *Target) error) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := do(w, c.Request.Context(), t); err != nil {
		switch {
		case errors.Is(err, errNoComponent), errors.Is(err, errNothingToRollBack):
			respondErr(c, http.StatusConflict, err)
		case errors.Is(err, errNoDaemon):
			respondErr(c, http.StatusServiceUnavailable, err)
		default:
			respondErr(c, http.StatusBadGateway, err)
		}
		return
	}
	c.Status(http.StatusNoContent)
}

// handlePauseTarget and handleResumeTarget stop and restart the WATCHING. What
// is running stays running; see the field comment on Target.DeployPaused.
func (w *Watcher) handlePauseTarget(c *gin.Context) {
	w.setDeployPaused(c, true)
}

func (w *Watcher) handleResumeTarget(c *gin.Context) {
	w.setDeployPaused(c, false)
}

func (w *Watcher) setDeployPaused(c *gin.Context, paused bool) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	updates := map[string]any{"deploy_paused": paused}
	if !paused {
		// Resuming checks now rather than at the end of the poll interval: the
		// reason somebody resumes is that they want what they pushed while it
		// was paused, and waiting out the interval looks like it did not work.
		updates["next_check_at"] = time.Now()
	}
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(updates).Error; err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleDeployTarget handles POST /v1/targets/:name/deploy (M20b): a CI
// pipeline (or any caller holding the watcher's bearer token) tells the
// watcher to deploy a specific image right now, bypassing the poll/build
// pipeline (git polling for "source" targets, digest polling for "image"
// targets) entirely. Works for targets of either DeployMode — a "source"
// target can be deployed an out-of-band image without touching its git
// pipeline state (LastCommit is left alone).
func (w *Watcher) handleDeployTarget(c *gin.Context) {
	if c.ContentType() != "application/json" {
		respondErr(c, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}
	var req DeployTargetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Image == "" {
		respondErr(c, http.StatusBadRequest, errors.New("image is required"))
		return
	}

	// Serialize with the sweep goroutine (see Watcher.targetLocks): taken
	// before the row read so the whole read→pull→upsert→record sequence acts
	// on a row no concurrent sweep is mid-flight on. If a sweep is currently
	// building/deploying this target, this request blocks until it finishes.
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
	if t.QuotaHold != "" {
		respondErr(c, http.StatusConflict, errQuotaHold{t.QuotaHold})
		return
	}
	if err := w.decryptTarget(t); err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	ctx := c.Request.Context()

	// A deploy like any other: the generation moves and the revision names
	// what is going out, so anything waiting on build_generation (the CLI's
	// --wait, wait-and-verify) sees it, and `-o json` does not keep describing
	// the previous version. The image ref stands in for the revision until the
	// digest is known below.
	w.setBuilding(t, req.Image)

	// Registry credentials only apply to "image" mode (AuthCredential in
	// "source" mode is a git token/SSH key, not a registry username:password
	// — pulling with it would be nonsensical). "source" mode targets always
	// pull anonymously here.
	var username, password string
	if t.DeployMode == DeployModeImage {
		username, password = splitRegistryCredential(t.AuthCredential)
	}

	// Pull failure is not fatal: the image may already be present locally
	// (matches the same "warn, continue" stance qooqd's own Deploy takes —
	// see internal/core/service.go's PullImage-before-create comment).
	if err := w.client.PullImageAuth(ctx, req.Image, username, password); err != nil {
		w.log.Warn("deploy: pull image failed (continuing, image may be local)", zap.String("target", t.Name), zap.String("image", req.Image), zap.Error(err))
	}

	deployReq, err := w.buildDeployRequest(t, req.Image, deploySpecFields{})
	if err != nil {
		// Recorded, or the row would say "building" forever.
		w.recordFailure(t, err)
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	comp, err := w.client.Upsert(ctx, deployReq)
	if err != nil {
		msg := maskRepoCredentials(err.Error(), t.RepoURL)
		w.recordFailure(t, err)
		respondErr(c, http.StatusBadGateway, fmt.Errorf("upsert component (image %s): %s", req.Image, msg))
		return
	}

	// For "image" mode targets, this deploy's image becomes the new baseline
	// the target polls (see recordImageSuccess's doc comment for why: without
	// updating ImageRef, the next sweep tick would immediately detect the
	// *old* ImageRef's unchanged digest as "different" from what was just
	// recorded and revert this deploy). Digest resolution is best-effort — a
	// failure is not fatal (matches M20a's sweep-side stance), it just leaves
	// LastImageDigest at its prior value until the next successful check.
	digest := ""
	newImageRef := ""
	if t.DeployMode == DeployModeImage {
		newImageRef = req.Image
		if d, derr := w.client.ImageDigest(ctx, req.Image, username, password); derr != nil {
			w.log.Warn("deploy: resolve digest after deploy failed (best-effort)", zap.String("target", t.Name), zap.Error(derr))
		} else {
			digest = d
		}
	}

	// A version CI picks that does not start is rolled back like any other.
	// The answer is still 200 with the target, whose status is failed and
	// whose error says what happened: the request was carried out, and the
	// caller (CI, through the CLI) reads the outcome from the status.
	// ImageRef stays the old one, so polling keeps following what runs.
	if verr := w.verifyOrRollBackPlan(ctx, t, comp, imageRollbackPlan(req.Image, digest, t.LastImageDigest)); verr != nil {
		w.recordFailure(t, verr)
		c.JSON(http.StatusOK, t.toResponse())
		return
	}

	if err := w.recordImageSuccess(t, newImageRef, digest); err != nil {
		w.log.Warn("deploy: persist success failed", zap.String("target", t.Name), zap.Error(err))
	}

	c.JSON(http.StatusOK, t.toResponse())
}

// handleTriggerTarget schedules a target for the next sweep tick.
//
// ?force=1 also rebuilds when the remote has not moved. A plain trigger only
// says "look now", and looking finds the same commit — which is right for
// "did I miss a push?" and useless for the case where the commit is fine and
// something around it changed: an input, the machine's address. Without the
// flag the only way to rebuild an unchanged commit was to push an empty one.
func (w *Watcher) handleTriggerTarget(c *gin.Context) {
	if t, err := getTargetByName(w.db, c.Param("name")); err == nil && t.QuotaHold != "" {
		// Accepting it would queue a deploy that dueTargets never releases:
		// the button would do nothing, silently.
		respondErr(c, http.StatusConflict, errQuotaHold{t.QuotaHold})
		return
	}
	if c.Query("force") == "1" {
		t, err := getTargetByName(w.db, c.Param("name"))
		if err != nil {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		if err := w.db.Model(t).Update("force_rebuild", true).Error; err != nil {
			respondErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	if err := triggerNow(w.db, c.Param("name")); err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Status(http.StatusAccepted)
}

// handleGetManifest returns the manifest qooq generated for a repo that has
// none, as YAML. 404 when the target has one of its own (there is nothing
// generated to show) or has not been built yet.
func (w *Watcher) handleGetManifest(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	if strings.TrimSpace(t.GeneratedManifest) == "" {
		respondErr(c, http.StatusNotFound, errors.New("no generated manifest: this repo either has its own qooq.yaml or has not been built yet"))
		return
	}
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", []byte(t.GeneratedManifest))
}

// handleGetBuildLog returns the most recent build's output.
//
// Plain text rather than JSON: it is a log, the caller is going to show it to a
// person, and wrapping it in a string field only means both sides doing escape
// processing on something that is already exactly what should be displayed.
//
// 404 with an explanation when there is none, because "no log" has two very
// different causes — nothing has been built yet, or this daemon is older than
// the feature — and an empty 200 would look like a build that printed nothing.
func (w *Watcher) handleGetBuildLog(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	if strings.TrimSpace(t.LastBuildLog) == "" {
		respondErr(c, http.StatusNotFound, errors.New(
			"no build log: this target has not been built yet, or it was last built by a qooqd that did not return one"))
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(t.LastBuildLog))
}

// handleAppLogs streams the running app's own output: what the target's
// component writes, read from qooqd. Before this the build output was the only
// output anyone above the operator could see — an app that started and then
// misbehaved left its owner with nothing to go on.
//
// tail is capped: the container's log is up to 100 MB, and "all of it" is
// never what somebody asking "what is it saying" wants.
func (w *Watcher) handleAppLogs(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	tail := 200
	if v, err := strconv.Atoi(c.Query("tail")); err == nil && v > 0 {
		tail = min(v, maxAppLogTail)
	}
	follow := c.Query("follow") == "1"

	comp, err := w.componentFor(c.Request.Context(), t)
	switch {
	case errors.Is(err, errNoDaemon):
		respondErr(c, http.StatusServiceUnavailable, err)
		return
	case err != nil:
		respondErr(c, http.StatusBadGateway, err)
		return
	case comp == nil:
		respondErr(c, http.StatusConflict, errNoComponent)
		return
	}
	rc, err := w.client.Logs(c.Request.Context(), comp.ID, follow, tail, false)
	if err != nil {
		respondErr(c, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				return // the reader went away
			}
			c.Writer.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// maxAppLogTail bounds how many lines one request may ask for.
const maxAppLogTail = 5000

// handlePutManifest replaces the generated manifest. This is the correction
// path: detection guessed, the operator fixes it, and the fix survives every
// later sweep because generation only ever runs when the stored copy is empty.
//
// The body is validated before it is stored — accepting a manifest that cannot
// parse would turn one bad edit into a target that fails every sweep with an
// error pointing at itself.
// handleSetInputs handles PUT /v1/targets/:name/inputs: supply the values a
// manifest declared but the repo cannot know.
//
// Values merge into whatever is already stored rather than replacing the set,
// so supplying one input does not silently drop the others; an empty value
// clears that one input. Supplying a value triggers an immediate re-deploy —
// the point of setting it is to unblock a deployment that is stuck now, and
// making the operator wait for the poll interval (or push an unrelated commit)
// would make the correction loop useless. Same reasoning as handlePutManifest.
func (w *Watcher) handleSetInputs(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	var req SetInputsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if len(req.Values) == 0 {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("values is required"))
		return
	}

	current, err := w.loadInputValues(t)
	if err != nil {
		// The stored values cannot be read (corrupt, or encrypted under a key
		// that has since changed). Refusing here rather than starting from an
		// empty map means an operator setting one input can't quietly wipe the
		// ones they can no longer see.
		respondErr(c, http.StatusConflict, err)
		return
	}
	for name, v := range req.Values {
		if v == "" {
			delete(current, name)
			continue
		}
		current[name] = v
	}

	declared, err := decodeDeclaredInputs(t.DeclaredInputs)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	encoded, err := w.storeInputValues(current, declared)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	if err := w.db.Model(t).Updates(map[string]any{
		"input_values":  encoded,
		"force_rebuild": true,
	}).Error; err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := triggerNow(w.db, t.Name); err != nil {
		w.log.Warn("could not schedule the rebuild", zap.String("target", t.Name), zap.Error(err))
	}

	fresh, err := getTargetByName(w.db, t.Name)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	names := make([]string, 0, len(req.Values))
	for name := range req.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	w.log.Info("inputs set by operator, rebuilding",
		zap.String("target", t.Name), zap.Strings("inputs", names))
	c.JSON(http.StatusOK, fresh.toResponse())
}

func (w *Watcher) handlePutManifest(c *gin.Context) {
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxManifestBytes))
	if err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if _, err := parseManifest(body); err != nil {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("manifest is not valid: %w", err))
		return
	}
	// Rebuild without waiting for a commit: the point of editing this is to
	// fix a deploy that is wrong now, and requiring an unrelated push first
	// would make the correction loop useless.
	if err := w.db.Model(t).Updates(map[string]any{
		"generated_manifest": string(body),
		"force_rebuild":      true,
	}).Error; err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := triggerNow(w.db, t.Name); err != nil {
		w.log.Warn("could not schedule the rebuild", zap.String("target", t.Name), zap.Error(err))
	}
	w.log.Info("generated manifest replaced by operator, rebuilding", zap.String("target", t.Name))
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", body)
}

// --- per-namespace secrets (M3) ---

type putSecretRequest struct {
	Value string `json:"value"`
}

// handlePutSecret sets or replaces one namespace's secret.
func (w *Watcher) handlePutSecret(c *gin.Context) {
	ns := c.Param("ns")
	name := c.Param("name")

	var req putSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {\"value\": \"...\"}"})
		return
	}
	if err := w.putSecret(ns, name, req.Value); err != nil {
		w.log.Error("put secret", zap.String("namespace", ns), zap.String("name", name), zap.Error(err))
		// The underlying error can name columns and values; the caller gets the
		// generic form and the detail goes to the log, matching this project's
		// 5xx stance.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not store the secret"})
		return
	}
	// No value in the response, not even the one just sent: a response body is
	// the easiest thing in the world to end up in a log or a CI transcript.
	c.JSON(http.StatusOK, gin.H{"namespace": ns, "name": name})
}

// handleListSecrets returns the names a namespace has. Never the values.
func (w *Watcher) handleListSecrets(c *gin.Context) {
	ns := c.Param("ns")
	names, err := w.listSecretNames(ns)
	if err != nil {
		w.log.Error("list secrets", zap.String("namespace", ns), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not list secrets"})
		return
	}
	if names == nil {
		names = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"namespace": ns, "names": names})
}

func (w *Watcher) handleDeleteSecret(c *gin.Context) {
	ns := c.Param("ns")
	name := c.Param("name")
	if err := w.deleteSecret(ns, name); err != nil {
		if errors.Is(err, errSecretNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no such secret in this namespace"})
			return
		}
		w.log.Error("delete secret", zap.String("namespace", ns), zap.String("name", name), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not delete the secret"})
		return
	}
	c.Status(http.StatusNoContent)
}
