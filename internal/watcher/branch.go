package watcher

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-git/go-git/v5/plumbing"
	"go.uber.org/zap"
)

// ErrTargetStackOwned is returned when a change is asked of a target that a
// stack derives from its manifest. The next stack sync would write the stack's
// value back over it, so the change would look accepted and then quietly not
// be — refusing says where the value actually lives.
var ErrTargetStackOwned = errors.New("this target belongs to a stack")

// ErrTargetHasNoBranch is returned for an "image" mode target: it polls a
// registry ref and never clones, so there is no branch to change.
var ErrTargetHasNoBranch = errors.New("an image-mode target has no branch; it follows image_ref")

// ErrInvalidBranch is returned for a branch name git would not accept.
var ErrInvalidBranch = errors.New("invalid branch name")

// validateBranch rejects names that cannot be a branch before they are stored.
// The pipeline turns the name into refs/heads/<name> (git.go), so a bad one
// would otherwise surface minutes later as a failed build, far from the
// request that caused it.
//
// A full ref ("refs/heads/x", "refs/tags/v1") is refused rather than unwrapped:
// prefixing it again gives refs/heads/refs/tags/v1, which is valid and wrong.
func validateBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("%w: branch is required", ErrInvalidBranch)
	}
	if len(branch) >= len("refs/") && branch[:len("refs/")] == "refs/" {
		return fmt.Errorf("%w: %q — give the branch name alone, without refs/", ErrInvalidBranch, branch)
	}
	if err := plumbing.NewBranchReferenceName(branch).Validate(); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidBranch, branch)
	}
	return nil
}

// setTargetBranch points an existing target at another branch and schedules a
// rebuild from that branch's HEAD.
//
// Held under the target's lock with the row re-read inside it, the same shape
// as the sweep and handleDeployTarget: an in-flight build of the old branch
// finishes first, and its recordSuccess (which clears force_rebuild) lands
// before this sets it, instead of after — otherwise the switch would be
// silently undone whenever the two heads happened to match.
//
// LastCommit is left alone. force_rebuild already makes the next sweep build
// whatever the new branch's head is, and LastCommit is what the pipeline
// reads as the previous commit when it prunes images: keeping it keeps the
// old branch's image as the rollback target.
//
// Returns changed=false when the branch is already the requested one; nothing
// is rebuilt in that case — a form submitted twice is not a request to restart
// the app.
func (w *Watcher) setTargetBranch(name, branch string) (t *Target, changed bool, err error) {
	if err := validateBranch(branch); err != nil {
		return nil, false, err
	}

	unlock := w.lockTarget(name)
	defer unlock()

	t, err = getTargetByName(w.db, name)
	if err != nil {
		return nil, false, err
	}
	if t.StackName != "" {
		return nil, false, fmt.Errorf("%w %q: change the branch in its stack manifest (project %q) instead",
			ErrTargetStackOwned, t.StackName, t.ProjectName)
	}
	if t.DeployMode == DeployModeImage || t.DeployMode == DeployModeUpload {
		return nil, false, ErrTargetHasNoBranch
	}
	if t.Branch == branch {
		return t, false, nil
	}

	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"branch":        branch,
		"force_rebuild": true,
	}).Error; err != nil {
		return nil, false, err
	}
	// triggerNow also lifts a give-up pause (buildgiveup.go): switching to a
	// branch that builds is one of the ways a paused target gets fixed.
	if err := triggerNow(w.db, name); err != nil {
		return nil, false, err
	}
	t, err = getTargetByName(w.db, name)
	return t, true, err
}

var errAuthChangeRefused = errors.New("auth_type can only change from none to " + AuthTypeGitHubApp)

// useAppAuth moves an unauthenticated git target to GitHub App auth.
//
// The one auth change allowed after creation, because it is the one a target
// can need without anybody having made a mistake: the external manager used to
// pick "none" for a github.com repository whose account had not installed the
// App yet, and that target then cloned anonymously forever — a private
// repository failing with git's "Repository not found" however the install
// went afterwards. Nothing carrying a credential can be set here, and nothing
// can be downgraded.
func (w *Watcher) useAppAuth(name, authType string) (*Target, error) {
	if authType != AuthTypeGitHubApp {
		return nil, errAuthChangeRefused
	}
	if w.control == nil {
		return nil, fmt.Errorf("%w: %v", errAuthChangeRefused, errNoControlPlane)
	}
	t, err := getTargetByName(w.db, name)
	if err != nil {
		return nil, err
	}
	switch {
	case t.AuthType == AuthTypeGitHubApp:
		return t, nil
	case t.StackName != "":
		// The stack writes its own auth back on the next sync.
		return nil, ErrTargetStackOwned
	case t.AuthType != "" && t.AuthType != AuthTypeNone, t.DeployMode == DeployModeImage, t.RepoURL == "":
		return nil, errAuthChangeRefused
	}
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).
		Update("auth_type", AuthTypeGitHubApp).Error; err != nil {
		return nil, err
	}
	w.log.Info("target moved to GitHub App auth", zap.String("target", name))
	return getTargetByName(w.db, name)
}

// handleUpdateTarget handles PUT /v1/targets/:name. Only the branch, the auth
// type and the domain can change; see UpdateTargetRequest.
func (w *Watcher) handleUpdateTarget(c *gin.Context) {
	if c.ContentType() != "application/json" {
		respondErr(c, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}
	var req UpdateTargetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Branch == nil && req.AuthType == nil && req.Domain == nil {
		respondErr(c, http.StatusBadRequest, errors.New("nothing to update: branch, auth_type and domain are the fields that can change"))
		return
	}

	name := c.Param("name")
	if req.AuthType != nil {
		t, err := w.useAppAuth(name, *req.AuthType)
		switch {
		case errors.Is(err, ErrTargetNotFound):
			respondErr(c, http.StatusNotFound, err)
			return
		case errors.Is(err, errAuthChangeRefused):
			respondErr(c, http.StatusBadRequest, err)
			return
		case errors.Is(err, ErrTargetStackOwned):
			respondErr(c, http.StatusConflict, err)
			return
		case err != nil:
			w.log.Error("update target auth", zap.String("target", name), zap.Error(err))
			respondErr(c, http.StatusInternalServerError, errors.New("could not update the target"))
			return
		}
		if req.Branch == nil && req.Domain == nil {
			c.JSON(http.StatusOK, t.toResponse())
			return
		}
	}
	if req.Domain != nil {
		t, changed, err := w.setTargetDomain(name, *req.Domain)
		switch {
		case errors.Is(err, ErrTargetNotFound):
			respondErr(c, http.StatusNotFound, err)
			return
		case errors.Is(err, ErrInvalidDomain):
			respondErr(c, http.StatusBadRequest, err)
			return
		case errors.Is(err, ErrTargetStackOwned):
			respondErr(c, http.StatusConflict, err)
			return
		case err != nil:
			w.log.Error("update target domain", zap.String("target", name), zap.Error(err))
			respondErr(c, http.StatusInternalServerError, errors.New("could not update the target"))
			return
		}
		if changed {
			w.log.Info("target domain changed by operator, rebuilding",
				zap.String("target", name), zap.String("domain", *req.Domain))
		}
		if req.Branch == nil {
			c.JSON(http.StatusOK, t.toResponse())
			return
		}
	}
	t, changed, err := w.setTargetBranch(name, *req.Branch)
	switch {
	case errors.Is(err, ErrTargetNotFound):
		respondErr(c, http.StatusNotFound, err)
		return
	case errors.Is(err, ErrInvalidBranch), errors.Is(err, ErrTargetHasNoBranch):
		respondErr(c, http.StatusBadRequest, err)
		return
	case errors.Is(err, ErrTargetStackOwned):
		respondErr(c, http.StatusConflict, err)
		return
	case err != nil:
		w.log.Error("update target", zap.String("target", name), zap.Error(err))
		respondErr(c, http.StatusInternalServerError, errors.New("could not update the target"))
		return
	}
	if changed {
		w.log.Info("target branch changed by operator, rebuilding",
			zap.String("target", name), zap.String("branch", t.Branch))
	}
	c.JSON(http.StatusOK, t.toResponse())
}
