package watcher

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// mountSourceKind is how a mount's source string is meant to be read. The form
// of the string decides, so a manifest never has to say which kind it means.
type mountSourceKind int

const (
	// mountSourceRepo is a path inside the repo ("./config.toml",
	// "config/app.toml"): its content is versioned with the code, so the
	// watcher copies it out of the clone into a stable per-commit directory.
	mountSourceRepo mountSourceKind = iota
	// mountSourceHost is an absolute host path ("/etc/qooq/app.env"):
	// operator-owned, mounted as-is.
	mountSourceHost
	// mountSourceVolume is a bare name ("appdata"): a watcher-managed
	// directory that outlives every deploy.
	mountSourceVolume
)

// volumeNameRe is what tells a named volume from a repo-relative file. A name
// with no separator and no dot is a volume (compose's convention); anything
// else — "./x", "config/x", "config.toml" — is a path. Writing "./config.toml"
// is always the unambiguous way to mean "the file in my repo root".
var volumeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func classifyMountSource(from string) mountSourceKind {
	switch {
	case filepath.IsAbs(from):
		return mountSourceHost
	case volumeNameRe.MatchString(from):
		return mountSourceVolume
	default:
		return mountSourceRepo
	}
}

// mountsPolicy decides which mount sources a manifest may use. It exists
// because a manifest is repo-controlled: allowing an arbitrary absolute host
// path lets the repo pick any file on the machine (or the docker socket, which
// is a container escape). The default stays permissive — qooq's existing
// stance is that creating a target means trusting that repo's Dockerfile —
// but an operator hosting repos they don't own can tighten it.
type mountsPolicy struct {
	raw string
	// allowPrefixes is non-nil only for the "allowlist:" form.
	allowPrefixes []string
	repoOnly      bool
}

const (
	mountsPolicyOpen     = "open"
	mountsPolicyRepoOnly = "repo-only"
	mountsPolicyPrefix   = "allowlist:"
)

func parseMountsPolicy(raw string) (mountsPolicy, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "", raw == mountsPolicyOpen:
		return mountsPolicy{raw: mountsPolicyOpen}, nil
	case raw == mountsPolicyRepoOnly:
		return mountsPolicy{raw: raw, repoOnly: true}, nil
	case strings.HasPrefix(raw, mountsPolicyPrefix):
		var prefixes []string
		for _, p := range strings.Split(strings.TrimPrefix(raw, mountsPolicyPrefix), ",") {
			if p = strings.TrimSpace(p); p != "" {
				prefixes = append(prefixes, filepath.Clean(p))
			}
		}
		if len(prefixes) == 0 {
			return mountsPolicy{}, fmt.Errorf("mounts.policy %q: allowlist: needs at least one path prefix", raw)
		}
		return mountsPolicy{raw: raw, allowPrefixes: prefixes}, nil
	default:
		return mountsPolicy{}, fmt.Errorf("mounts.policy %q: must be %q, %q or %q<prefix>[,<prefix>...]",
			raw, mountsPolicyOpen, mountsPolicyRepoOnly, mountsPolicyPrefix)
	}
}

// checkHostPath reports whether an absolute host path may be mounted. Only
// host paths are policed: repo and volume sources resolve to directories the
// watcher itself owns, so the repo cannot reach outside them.
//
// The allowlist branch resolves symlinks before comparing (same reasoning as
// resolveWithin in pipeline.go): a plain string-prefix check on filepath.Clean
// only guards the *name* of the path, not what it actually points at. A
// manifest or operator mount that plants e.g. "<allowed-dir>/x -> /etc" would
// otherwise sail through the string check and have docker mount /etc into the
// container, making the allowlist policy pure theater. repo-only and open are
// unaffected — repo-only never reaches this branch, and open never calls it.
func (p mountsPolicy) checkHostPath(path string) error {
	switch {
	case p.repoOnly:
		return fmt.Errorf("host path %q is not allowed under mounts.policy=%s (use a repo-relative path or a named volume)", path, mountsPolicyRepoOnly)
	case p.allowPrefixes != nil:
		resolved, err := resolveSymlinksTolerant(path)
		if err != nil {
			// Cannot verify what this path actually resolves to: never fall
			// back to allowing it just because resolution failed.
			return fmt.Errorf("host path %q: cannot resolve for allowlist check: %w", path, err)
		}
		for _, prefix := range p.allowPrefixes {
			resolvedPrefix, err := resolveSymlinksTolerant(prefix)
			if err != nil {
				return fmt.Errorf("mounts.policy allowlist prefix %q: cannot resolve: %w", prefix, err)
			}
			if resolved == resolvedPrefix || strings.HasPrefix(resolved, resolvedPrefix+string(filepath.Separator)) {
				return nil
			}
		}
		return fmt.Errorf("host path %q is outside mounts.policy allowlist (%s)", path, strings.Join(p.allowPrefixes, ", "))
	default:
		return nil
	}
}

// resolveSymlinksTolerant resolves path following symlinks, the same way
// resolveWithin (pipeline.go) resolves a repo-relative path: when a segment
// doesn't exist, it can't be a symlink, so resolution walks up to the nearest
// existing ancestor, resolves that, and reattaches the non-existent suffix
// unchanged. This matters here specifically because, unlike a cloned repo's
// root, a host mount's target directory tree is not guaranteed to exist yet
// (see the existing allowlist tests using paths like "/etc/qooq/app.env").
// Any resolution failure that isn't "doesn't exist" (e.g. permission denied)
// is returned as an error, and the root "/" always exists, so this only
// errors on a genuine inability to determine the real path — it never
// silently treats "couldn't check" as "allowed".
func resolveSymlinksTolerant(path string) (string, error) {
	clean := filepath.Clean(path)
	suffix := ""
	cur := clean
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if suffix == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root and even it errored: give up
			// rather than loop forever.
			return "", err
		}
		rel, rerr := filepath.Rel(parent, cur)
		if rerr != nil {
			return "", rerr
		}
		if suffix != "" {
			rel = filepath.Join(rel, suffix)
		}
		suffix = rel
		cur = parent
	}
}

// checkOperatorMounts validates the mounts an operator put in a target's
// deploy_spec. Unlike a manifest's, these are not repo-controlled — the person
// who created the target wrote them — so "repo-only" does not apply to them.
// An explicit allowlist does: that one exists to fence off parts of the host
// filesystem entirely, whoever is asking.
func (m *Watcher) checkOperatorMounts(mounts []mountSpec) error {
	for _, mnt := range mounts {
		if mnt.HostPath == "" || mnt.ContainerPath == "" {
			return fmt.Errorf("host_path and container_path are both required")
		}
		if !filepath.IsAbs(mnt.HostPath) || !filepath.IsAbs(mnt.ContainerPath) {
			return fmt.Errorf("host_path %q and container_path %q must both be absolute", mnt.HostPath, mnt.ContainerPath)
		}
		if m.mountsPolicy.allowPrefixes != nil {
			if err := m.mountsPolicy.checkHostPath(mnt.HostPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// materializeMounts turns a manifest's declarative run.files/run.volumes into
// concrete host→container bindings. It runs after the image is built and
// before the deploy request is assembled, because that is the first moment
// both the commit sha (which versions the file copies) and the final clone
// contents are known — and the last moment before the clone dir is discarded.
//
// files are versioned per commit: the same path re-materialized under each
// sha7 rather than overwritten in place. Overwriting would be visible
// instantly to the container still running the previous version (a bind mount
// is live), and would make a rollback serve the newer config.
//
// volumes are not versioned and never pruned: they are the app's data.
func (m *Watcher) materializeMounts(ref componentRef, cloneDir, sha7 string, files, volumes []manifestMount) ([]mountSpec, error) {
	var out []mountSpec

	add := func(decl manifestMount, defaultReadOnly bool, isVolume bool) error {
		if decl.From == "" || decl.To == "" {
			return fmt.Errorf("mount: both source and target are required (got %q -> %q)", decl.From, decl.To)
		}
		if !filepath.IsAbs(decl.To) {
			return fmt.Errorf("mount target %q must be an absolute container path", decl.To)
		}
		readOnly := defaultReadOnly
		switch decl.Mode {
		case "ro":
			readOnly = true
		case "rw":
			readOnly = false
		case "":
		default:
			return fmt.Errorf("mount %q: mode must be \"ro\" or \"rw\" (got %q)", decl.To, decl.Mode)
		}

		var hostPath string
		switch classifyMountSource(decl.From) {
		case mountSourceHost:
			if err := m.mountsPolicy.checkHostPath(decl.From); err != nil {
				return err
			}
			hostPath = filepath.Clean(decl.From)
			if isVolume {
				// A host directory used as a volume is created on demand,
				// like `docker compose up` does — the operator named a place
				// for data, not an existing artifact.
				if err := os.MkdirAll(hostPath, 0o755); err != nil {
					return fmt.Errorf("mount %s: create host dir: %w", decl.To, err)
				}
			} else if _, err := os.Stat(hostPath); err != nil {
				return fmt.Errorf("mount %s: host source %s: %w", decl.To, hostPath, err)
			}

		case mountSourceVolume:
			hostPath = m.volumePath(ref, decl.From)
			if err := os.MkdirAll(hostPath, 0o755); err != nil {
				return fmt.Errorf("volume %s: %w", decl.From, err)
			}

		case mountSourceRepo:
			// Same defense in depth the build paths get: IsLocal rejects the
			// string forms of an escape, resolveWithin then rejects a symlink
			// pointing out of the clone.
			if !validTargetPath(decl.From) {
				return fmt.Errorf("mount source %q must stay inside the repo", decl.From)
			}
			src, err := resolveWithin(cloneDir, decl.From)
			if err != nil {
				return fmt.Errorf("mount source %q: %w", decl.From, err)
			}
			info, err := os.Stat(src)
			if err != nil {
				return fmt.Errorf("mount source %q: %w", decl.From, err)
			}
			dest := filepath.Join(m.filesDir(ref), sha7, filepath.Clean(decl.From))
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("mount source %q: %w", decl.From, err)
			}
			if info.IsDir() {
				if err := copyTree(src, dest); err != nil {
					return fmt.Errorf("mount source %q: %w", decl.From, err)
				}
			} else if err := copyDataFile(src, dest); err != nil {
				return fmt.Errorf("mount source %q: %w", decl.From, err)
			}
			hostPath = dest
		}

		out = append(out, mountSpec{HostPath: hostPath, ContainerPath: decl.To, ReadOnly: readOnly})
		return nil
	}

	// files default to read-only: they are configuration the image consumes,
	// and a writable bind lets a container running as root leave root-owned
	// files on the host that the watcher then cannot prune.
	for _, f := range files {
		if err := add(f, true, false); err != nil {
			return nil, fmt.Errorf("run.files: %w", err)
		}
	}
	for _, v := range volumes {
		if err := add(v, false, true); err != nil {
			return nil, fmt.Errorf("run.volumes: %w", err)
		}
	}

	// Reject duplicate container paths here rather than letting qooqd's
	// validateMounts do it, so the error names the manifest key at fault.
	seen := make(map[string]struct{}, len(out))
	for _, mnt := range out {
		clean := filepath.Clean(mnt.ContainerPath)
		if _, dup := seen[clean]; dup {
			return nil, fmt.Errorf("mount target %q is declared twice", mnt.ContainerPath)
		}
		seen[clean] = struct{}{}
	}
	return out, nil
}

// componentRef identifies the component being deployed: its namespace and its
// short name. The pair travels as one value rather than two adjacent string
// parameters, which are easy to pass in the wrong order and produce a
// plausible-looking wrong path.
type componentRef struct {
	namespace string
	name      string
}

// String renders the ref the way qooq addresses a namespaced component
// ("stack/project"), so log lines and errors say which one they mean.
func (r componentRef) String() string {
	if r.namespace == "" {
		return r.name
	}
	return r.namespace + "/" + r.name
}

// componentMountsDir/filesDir/volumePath lay out the per-component tree:
//
//	<mounts.dir>/<ns>/<component>/files/<sha7>/<repo-relative path>   (pruned)
//	<mounts.dir>/<ns>/<component>/volumes/<name>/                     (never pruned)
//
// An empty namespace collapses the extra level away (filepath.Join skips it),
// so everything materialized before namespaces existed keeps its path.
func (m *Watcher) componentMountsDir(ref componentRef) string {
	return filepath.Join(m.mountsDir, ref.namespace, ref.name)
}

func (m *Watcher) filesDir(ref componentRef) string {
	return filepath.Join(m.componentMountsDir(ref), "files")
}

func (m *Watcher) volumePath(ref componentRef, name string) string {
	return filepath.Join(m.componentMountsDir(ref), "volumes", name)
}

// pruneOldFileMounts keeps only the newest keepShaDirs per-commit copies of a
// component's mounted files, mirroring pruneOldArtifacts. It is deliberately
// scoped to files/: volumes/ holds data and must never be swept.
func (m *Watcher) pruneOldFileMounts(ref componentRef) {
	removed, err := pruneOldShaDirs(m.filesDir(ref), keepShaDirs)
	if err != nil {
		m.log.Warn("prune old file mounts", zap.String("component", ref.name), zap.Error(err))
		return
	}
	for _, path := range removed {
		m.log.Info("pruned old file mounts", zap.String("component", ref.name), zap.String("dir", path))
	}
}

// keepShaDirs is how many per-commit directories survive a prune: the current
// one and its predecessor, matching Status.PreviousImage's "one step back"
// semantics. Do not lower this to 1 — a rollback needs the previous commit's
// files to still be on disk.
const keepShaDirs = 2

// pruneOldShaDirs removes all but the newest keep subdirectories of dir
// (ordered by mtime) and returns the paths it removed. A missing dir is not an
// error: nothing has been materialized yet.
func pruneOldShaDirs(dir string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type dirInfo struct {
		name    string
		modTime int64
	}
	var dirs []dirInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, dirInfo{name: e.Name(), modTime: info.ModTime().UnixNano()})
	}
	if len(dirs) <= keep {
		return nil, nil
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].modTime > dirs[j].modTime })

	var removed []string
	for _, d := range dirs[keep:] {
		path := filepath.Join(dir, d.name)
		if err := os.RemoveAll(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// copyDataFile copies a config/data file preserving nothing but its contents
// (0644). Executables are the artifact path's business, not a mount's.
func copyDataFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o644)
}

// copyTree copies a directory recursively. Symlinks are skipped rather than
// followed: the clone was already checked for escapes, and copying a link's
// target would reintroduce the very path traversal resolveWithin prevents.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			return nil
		default:
			return copyDataFile(path, target)
		}
	})
}
