package watcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.uber.org/zap"
)

// defaultStackManifestPath is where a stack's composition lives in its repository.
const defaultStackManifestPath = "qooq.stack.yaml"

// decryptedSource returns the stack's repository as a Target with its
// credential decrypted, ready for the git helpers.
//
// The decryption lands on a copy, never on stack itself: stack.AuthCredential is
// propagated verbatim (still encrypted) onto every project target, whose own
// sweep decrypts it with the same key. Decrypting in place would quietly
// write the plaintext credential back out to the project rows.
func (w *Watcher) decryptedSource(stack *Stack) (*Target, error) {
	st := stack.sourceTarget()
	plain, err := decryptCredential(stack.AuthCredential, w.secretKey)
	if err != nil {
		return nil, fmt.Errorf("auth_credential: %w", err)
	}
	st.AuthCredential = plain
	return st, nil
}

// syncStackManifest returns the stack's current manifest, fetching it from the
// repository when the remote has moved since the last successful fetch.
//
// The parsed manifest is stored on the stack, so an unchanged repository costs
// one ls-remote per sweep rather than a clone. A manifest that fails to parse
// leaves the previously stored one untouched and in force: a broken commit to
// the composition layer must not tear down a running stack.
func (w *Watcher) syncStackManifest(stack *Stack) (*stackManifest, error) {
	src, err := w.decryptedSource(stack)
	if err != nil {
		return nil, err
	}

	head, err := ResolveRemoteHead(src, w.gitAuth())
	if err != nil {
		if stack.Manifest != "" {
			// A transient fetch failure (DNS blip, remote briefly down) must
			// not stop the stack from converging on the manifest it already has.
			w.log.Warn("stack: could not reach the stack repo, using the last manifest that parsed",
				zap.String("stack", stack.Name),
				zap.String("err", maskRepoCredentials(err.Error(), stack.RepoURL)))
			return parseStackManifest([]byte(stack.Manifest))
		}
		return nil, fmt.Errorf("resolve stack repo head: %w", err)
	}

	if head == stack.LastManifestCommit && stack.Manifest != "" {
		return parseStackManifest([]byte(stack.Manifest))
	}

	dir, err := mkTempCloneDir("stack-" + stack.Name)
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	hash, err := cloneBranch(src, dir, w.gitAuth())
	if err != nil {
		return nil, fmt.Errorf("clone stack repo: %w", err)
	}

	data, err := readStackManifestFile(dir, stack.ManifestPath)
	if err != nil {
		return nil, err
	}
	r, err := parseStackManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stack.ManifestPath, err)
	}
	if r.Stack != stack.Name {
		// The manifest names the stack it makes; deploying it under a different
		// name would put the components in a namespace the manifest's own
		// ${...} references and docs don't describe.
		return nil, fmt.Errorf("%s: stack is %q but this stack is named %q", stack.ManifestPath, r.Stack, stack.Name)
	}

	stack.Manifest = string(data)
	stack.LastManifestCommit = hash
	if err := w.db.Model(&Stack{}).Where("id = ?", stack.ID).Updates(map[string]any{
		"manifest":             stack.Manifest,
		"last_manifest_commit": stack.LastManifestCommit,
	}).Error; err != nil {
		return nil, fmt.Errorf("persist manifest: %w", err)
	}
	w.log.Info("stack: manifest updated",
		zap.String("stack", stack.Name), zap.String("commit", shortSHA(hash)), zap.Int("projects", len(r.Projects)))
	return r, nil
}

// readStackManifestFile reads the manifest out of a fresh clone, with the same
// path and size guards an app manifest gets.
func readStackManifestFile(cloneDir, path string) ([]byte, error) {
	if path == "" {
		path = defaultStackManifestPath
	}
	if !validTargetPath(path) {
		return nil, fmt.Errorf("manifest_path %q escapes the repo root", path)
	}
	full, err := resolveWithin(cloneDir, path)
	if err != nil {
		return nil, fmt.Errorf("manifest_path %q: %w", path, err)
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s not found in the repo", path)
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Size() > maxManifestBytes {
		return nil, fmt.Errorf("%s is %d bytes, exceeds the %d byte limit", path, fi.Size(), maxManifestBytes)
	}
	return os.ReadFile(full)
}

// reconcileProjects makes the stack's project targets match the manifest,
// creating what is new and updating what changed. It returns the project targets keyed by project
// name, plus the names of orphaned projects — targets this stack owns that the
// manifest no longer mentions.
//
// Orphans are reported, never deleted: a project that disappears from a manifest is
// usually a rename or an edit in progress, and its component may hold data.
// The same stance `watch rm` takes toward services and volumes.
func (w *Watcher) reconcileProjects(stack *Stack, r *stackManifest) (map[string]*Target, []string, error) {
	existing, err := projectTargets(w.db, stack.Name)
	if err != nil {
		return nil, nil, err
	}

	scope := w.stackScope(stack, r)

	out := make(map[string]*Target, len(r.Projects))
	for _, name := range sortedProjectNames(r.Projects) {
		desired, err := w.desiredProjectTarget(stack, name, r.Projects[name], scope)
		if err != nil {
			return nil, nil, fmt.Errorf("projects.%s: %w", name, err)
		}

		current, ok := existing[name]
		if !ok {
			// A target may exist under this name without belonging to the stack
			// (an operator created it by hand first). Adopting it silently
			// would move someone else's deployment into this namespace.
			if clash, err := getTargetByName(w.db, desired.Name); err == nil && clash.StackName != stack.Name {
				return nil, nil, fmt.Errorf("projects.%s: target %q already exists and is not part of this stack", name, desired.Name)
			}
			if err := createTarget(w.db, desired); err != nil {
				return nil, nil, fmt.Errorf("projects.%s: create target: %w", name, err)
			}
			w.log.Info("stack: project added", zap.String("stack", stack.Name), zap.String("project", name))
			out[name] = desired
			continue
		}

		if updates := projectTargetUpdates(current, desired); len(updates) > 0 {
			// Deploy inputs changed without the project's own repo necessarily
			// moving — the same situation an edited generated manifest creates,
			// and the same remedy.
			updates["force_rebuild"] = true
			current.ForceRebuild = true
			if err := w.db.Model(&Target{}).Where("id = ?", current.ID).Updates(updates).Error; err != nil {
				return nil, nil, fmt.Errorf("projects.%s: update target: %w", name, err)
			}
			w.log.Info("stack: project definition changed, rebuilding",
				zap.String("stack", stack.Name), zap.String("project", name), zap.Any("changed", keysOf(updates)))
		}
		out[name] = current
	}

	var orphans []string
	for name := range existing {
		if _, ok := r.Projects[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	return out, orphans, nil
}

// desiredProjectTarget renders one manifest project into the Target that
// builds it.
//
// This is the whole of the project -> target mapping: a path: project is a target on
// the stack's own repository narrowed to a subdirectory, and a repo: project is a
// target on someone else's repository. Everything downstream (manifest
// resolution, generation, services, mounts) then applies unchanged.
func (w *Watcher) desiredProjectTarget(stack *Stack, name string, project stackProject, scope *interpolationScope) (*Target, error) {
	env, err := scope.expandMap(project.Env)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}

	spec := deploySpecFields{
		// The stack owns the namespace and the network: mergeDeploySpec treats
		// these as the operator layer, so they win over whatever the project's own
		// qooq.yaml says. A project is addressed by its short name inside the
		// stack's network, which is what lets one repo be deployed into several
		// stacks without editing its manifest.
		Namespace: stack.Name,
		Network:   stack.network(),
		Env:       env,
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	t := &Target{
		Name:                projectTargetName(stack.Name, name),
		DeployMode:          DeployModeSource,
		ComponentName:       name,
		StackName:           stack.Name,
		ProjectName:         name,
		AllowDuplicate:      stack.AllowDuplicate,
		DeploySpec:          string(encoded),
		PollIntervalSeconds: stack.PollIntervalSeconds,
	}

	manifestPath := project.Manifest
	if manifestPath == "" {
		manifestPath = "qooq.yaml"
	}

	switch project.source() {
	case projectSourcePath:
		sub := filepath.Clean(project.Path)
		if sub == "." {
			sub = ""
		}
		if !validTargetPath(sub) {
			return nil, fmt.Errorf("path %q must stay inside the stack's own repo", project.Path)
		}
		t.RepoURL = stack.RepoURL
		t.Branch = firstNonEmpty(project.Branch, stack.Branch)
		t.AuthType = stack.AuthType
		// Still encrypted at rest; the project's own sweep decrypts it.
		t.AuthCredential = stack.AuthCredential
		t.ContextSubdir = sub
		if manifestPath == "-" {
			t.ManifestPath = "-"
		} else {
			t.ManifestPath = filepath.Join(sub, manifestPath)
		}
	case projectSourceRepo:
		// No credential is forwarded to an external repository, deliberately:
		// the stack's token belongs to the stack's git host, and handing it to
		// whatever host a project names would leak it to a third party. Private
		// project repositories are a follow-up (they need their own credential
		// field in the manifest).
		// The stack manifest lives in the repository, so this URL is as
		// untrusted as the repository is.
		if err := w.checkRepoURLAllowed(project.Repo); err != nil {
			return nil, err
		}
		t.RepoURL = project.Repo
		t.Branch = firstNonEmpty(project.Branch, "main")
		t.AuthType = AuthTypeNone
		t.ManifestPath = manifestPath
	}
	return t, nil
}

// stackScope is what a manifest's env: values may reference. It is the same
// mechanism a manifest's ${...} uses, with one namespace of its own:
//
//	${project.<name>.host}   another project's address inside the stack's network
//	${stack} / ${network}  this stack
//	${env.NAME}           the watcher's environment (a secret, typically)
//
// Only .host exists on a project, because only .host is knowable here: a project's
// ports live in its own qooq.yaml, in a repository this layer has not cloned
// and deliberately does not read. Referencing ${project.x.port} therefore fails
// with the list of what does resolve, rather than guessing.
//
// The address is just the project's short name, so `http://${project.api.host}:8080`
// and `http://api:8080` are the same string. Writing the reference buys a typo
// check: a misspelled project name is refused here instead of becoming a
// connection error inside a container at 3am.
func (w *Watcher) stackScope(stack *Stack, r *stackManifest) *interpolationScope {
	// A stack's name is its namespace, so its secrets are that namespace's.
	// Without this, ${secret.*} and ${env.*} stop resolving inside a stack
	// manifest — which for an existing offline stack is a silent regression,
	// not a security improvement.
	scope := newInterpolationScope(w.newSecretResolver(stack.Name))
	scope.set("stack", stack.Name)
	scope.set("namespace", stack.Name)
	scope.set("network", stack.network())
	for name := range r.Projects {
		scope.set("project."+name+".host", name)
	}
	return scope
}

// projectTargetUpdates returns the columns where the desired project target differs
// from the stored one. Only fields the manifest owns are compared — anything the
// pipeline maintains (last commit, build status, generated manifest) is the
// target's own state and must survive a manifest edit.
func projectTargetUpdates(current, desired *Target) map[string]any {
	updates := map[string]any{}
	if current.RepoURL != desired.RepoURL {
		updates["repo_url"] = desired.RepoURL
	}
	if current.Branch != desired.Branch {
		updates["branch"] = desired.Branch
	}
	if current.AuthType != desired.AuthType {
		updates["auth_type"] = desired.AuthType
	}
	if current.AuthCredential != desired.AuthCredential {
		updates["auth_credential"] = desired.AuthCredential
	}
	if current.ContextSubdir != desired.ContextSubdir {
		updates["context_subdir"] = desired.ContextSubdir
	}
	if current.ManifestPath != desired.ManifestPath {
		updates["manifest_path"] = desired.ManifestPath
	}
	if current.ComponentName != desired.ComponentName {
		updates["component_name"] = desired.ComponentName
	}
	if current.DeploySpec != desired.DeploySpec {
		updates["deploy_spec"] = desired.DeploySpec
	}
	if current.PollIntervalSeconds != desired.PollIntervalSeconds {
		updates["poll_interval_seconds"] = desired.PollIntervalSeconds
	}
	return updates
}

// touchStack persists the outcome of one deploy pass.
func (w *Watcher) touchStack(stack *Stack, status, lastErr, pausedReason string) {
	stack.Status = status
	stack.LastError = lastErr
	stack.PausedReason = pausedReason
	stack.LastSyncAt = time.Now()
	if err := w.db.Model(&Stack{}).Where("id = ?", stack.ID).Updates(map[string]any{
		"status":        stack.Status,
		"last_error":    stack.LastError,
		"paused_reason": stack.PausedReason,
		"last_sync_at":  stack.LastSyncAt,
	}).Error; err != nil {
		w.log.Warn("stack: persist status failed", zap.String("stack", stack.Name), zap.Error(err))
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
