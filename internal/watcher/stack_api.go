package watcher

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

func (w *Watcher) handleCreateStack(c *gin.Context) {
	if c.ContentType() != "application/json" {
		respondErr(c, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}
	var req CreateStackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		respondErr(c, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	// The stack name becomes a namespace, a network name and a container name
	// prefix, so it has to be a DNS label — and finding that out now is much
	// better than finding it out as a deploy failure on the first sweep.
	if !stackNameRe.MatchString(req.Name) {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("name %q: must be a lowercase DNS label (a-z, 0-9, -), 1-63 chars", req.Name))
		return
	}
	// "default" is the name of the empty namespace, so a stack called that
	// would deploy its projects alongside every un-namespaced component
	// instead of into a namespace of its own — and would collide with them by
	// name without the stack looking like the cause.
	if req.Name == client.DefaultNamespace {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("name %q is reserved: it names the namespace components land in when they have none", req.Name))
		return
	}
	if req.RepoURL == "" {
		respondErr(c, http.StatusBadRequest, errors.New("repo_url is required (the repository holding qooq.stack.yaml)"))
		return
	}
	if err := w.checkRepoURLAllowed(req.RepoURL); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if !validTargetPath(req.ManifestPath) {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("manifest_path: %w", errUnsafeTargetPath))
		return
	}
	if req.PollIntervalSeconds != 0 && req.PollIntervalSeconds < minPollIntervalSeconds {
		respondErr(c, http.StatusBadRequest, fmt.Errorf("poll_interval_seconds must be 0 (default %ds) or at least %ds", defaultPollIntervalSeconds, minPollIntervalSeconds))
		return
	}

	credential, err := encryptCredential(req.AuthCredential, w.secretKey)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("encrypt auth_credential: %w", err))
		return
	}

	stack := &Stack{
		Name:                req.Name,
		RepoURL:             req.RepoURL,
		Branch:              req.Branch,
		AuthType:            req.AuthType,
		AuthCredential:      credential,
		ManifestPath:        req.ManifestPath,
		PollIntervalSeconds: req.PollIntervalSeconds,
		AllowDuplicate:      req.AllowDuplicate,
	}
	if err := createStack(w.db, stack); err != nil {
		if errors.Is(err, ErrStackNameConflict) {
			respondErr(c, http.StatusConflict, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusCreated, stack.toResponse())
}

func (w *Watcher) handleListStacks(c *gin.Context) {
	ds, err := listStacks(w.db)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	out := make([]StackResponse, 0, len(ds))
	for i := range ds {
		out = append(out, ds[i].toResponse())
	}
	c.JSON(http.StatusOK, out)
}

// handleGetStack returns the stack with its projects expanded, so one request
// answers "what is in this stack and how did each part's last deploy go".
func (w *Watcher) handleGetStack(c *gin.Context) {
	stack, err := getStackByName(w.db, c.Param("name"))
	if err != nil {
		w.respondStackErr(c, err)
		return
	}
	resp := stack.toResponse()
	projects, err := w.stackProjects(stack)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	resp.Projects = projects
	c.JSON(http.StatusOK, resp)
}

// stackProjects lists the stack's projects in manifest order, with any orphans (deployed
// but no longer in the manifest) appended and flagged.
func (w *Watcher) stackProjects(stack *Stack) ([]StackProjectResponse, error) {
	targets, err := projectTargets(w.db, stack.Name)
	if err != nil {
		return nil, err
	}

	var r *stackManifest
	if stack.Manifest != "" {
		// A manifest stored on the stack parsed successfully when it was stored;
		// if it somehow doesn't now, report the projects without their manifest
		// detail rather than failing the whole request.
		if parsed, err := parseStackManifest([]byte(stack.Manifest)); err == nil {
			r = parsed
		} else {
			w.log.Warn("stack: stored manifest no longer parses", zap.String("stack", stack.Name), zap.Error(err))
		}
	}

	out := make([]StackProjectResponse, 0, len(targets))
	seen := map[string]bool{}
	if r != nil {
		for _, name := range sortedProjectNames(r.Projects) {
			project := r.Projects[name]
			seen[name] = true
			entry := StackProjectResponse{
				Name:       name,
				Source:     projectSourceDescription(stack, project),
				DependsOn:  describeAfter(project),
				TargetName: projectTargetName(stack.Name, name),
			}
			fillProjectStatus(&entry, targets[name])
			out = append(out, entry)
		}
	}
	for _, name := range sortedTargetProjects(targets) {
		if seen[name] {
			continue
		}
		entry := StackProjectResponse{
			Name:       name,
			Source:     targets[name].RepoURL,
			TargetName: targets[name].Name,
			Orphaned:   true,
		}
		fillProjectStatus(&entry, targets[name])
		out = append(out, entry)
	}
	return out, nil
}

func fillProjectStatus(entry *StackProjectResponse, t *Target) {
	if t == nil {
		// Declared in the manifest but not yet reconciled into a target: the
		// stack has not been deployed since that project was added.
		entry.LastBuildStatus = BuildStatusNever
		return
	}
	entry.LastCommit = t.LastCommit
	entry.LastBuildStatus = t.LastBuildStatus
	entry.LastBuildError = t.LastBuildError
	entry.ConsecutiveFailures = t.ConsecutiveFailures
}

func projectSourceDescription(stack *Stack, project stackProject) string {
	if project.source() == projectSourceRepo {
		return project.Repo
	}
	return fmt.Sprintf("%s (%s)", stack.RepoURL, project.Path)
}

func describeAfter(project stackProject) []string {
	if len(project.DependsOn) == 0 {
		return nil
	}
	out := make([]string, 0, len(project.DependsOn))
	for _, dep := range project.DependsOn {
		if dep.Wait == waitStarted {
			out = append(out, dep.Name+" (started)")
			continue
		}
		out = append(out, dep.Name)
	}
	return out
}

func sortedTargetProjects(targets map[string]*Target) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// handleDeleteStack removes the stack and the watch targets for its projects.
//
// The deployed components are left running unless ?prune=true, matching what
// `watch rm` does: forgetting how something is built is not the same decision
// as tearing it down, and a project may be holding data. prune removes the project
// components but still never touches their services or volumes — those are
// deleted by hand, on purpose.
func (w *Watcher) handleDeleteStack(c *gin.Context) {
	name := c.Param("name")
	prune := c.Query("prune") == "true" || c.Query("prune") == "1"

	if _, err := getStackByName(w.db, name); err != nil {
		w.respondStackErr(c, err)
		return
	}

	// Hold the stack lock so a sweep in progress finishes (and does not
	// recreate the project targets) before they are deleted.
	unlock := w.lockStack(name)
	defer unlock()

	targets, err := projectTargets(w.db, name)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	if prune {
		for project, t := range targets {
			// Same removal as a single project's, services included.
			ref := w.componentRefFor(t)
			if err := w.removeComponentFor(c.Request.Context(), t); err != nil {
				respondErr(c, http.StatusBadGateway, fmt.Errorf("remove component %s: %w", ref, err))
				return
			}
			w.log.Info("stack: pruned project component",
				zap.String("stack", name), zap.String("project", project), zap.String("component", ref.String()))
		}
	}

	if err := w.db.Where("stack_name = ?", name).Delete(&Target{}).Error; err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := deleteStackByName(w.db, name); err != nil {
		w.respondStackErr(c, err)
		return
	}
	if !prune && len(targets) > 0 {
		w.log.Info("stack removed; its components are still running (pass prune=true to remove them)",
			zap.String("stack", name), zap.Int("projects", len(targets)))
	}
	c.Status(http.StatusNoContent)
}

func (w *Watcher) handleTriggerStack(c *gin.Context) {
	if err := triggerStackNow(w.db, c.Param("name")); err != nil {
		w.respondStackErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "triggered"})
}

// handleResumeStack lifts a pause and gives every project a fresh failure budget.
func (w *Watcher) handleResumeStack(c *gin.Context) {
	name := c.Param("name")

	unlock := w.lockStack(name)
	defer unlock()

	stack, err := getStackByName(w.db, name)
	if err != nil {
		w.respondStackErr(c, err)
		return
	}
	if err := w.resumeStack(stack); err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, stack.toResponse())
}

// handleGetStackManifest returns the manifest text the watcher is actually working
// from — which is the last one that parsed, not necessarily what is on the
// branch right now.
func (w *Watcher) handleGetStackManifest(c *gin.Context) {
	stack, err := getStackByName(w.db, c.Param("name"))
	if err != nil {
		w.respondStackErr(c, err)
		return
	}
	if stack.Manifest == "" {
		respondErr(c, http.StatusNotFound, errors.New("no stack manifest has been fetched yet"))
		return
	}
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", []byte(stack.Manifest))
}

func (w *Watcher) respondStackErr(c *gin.Context, err error) {
	if errors.Is(err, ErrStackNotFound) {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	respondErr(c, http.StatusInternalServerError, err)
}
