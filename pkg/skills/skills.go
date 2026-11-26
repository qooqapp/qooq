// Package skills bundles the agent skills shipped with qooq and installs them
// for a coding agent.
//
// The embedded tree is the single source of truth: qooq-cli imports this
// package rather than carrying its own copy, so `qooq skill install` can never
// drift from what lives in the qooq repo. That holds across agents too —
// Claude Code, Codex and Cursor all read the same on-disk shape (a directory
// with SKILL.md carrying name/description frontmatter, plus scripts and
// reference material), so one tree is installed verbatim for each of them and
// there is no per-agent copy to keep in step.
package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed all:qooq-deploy
var files embed.FS

// Skill describes one installable skill.
type Skill struct {
	// Name is both the embedded directory name and the directory created
	// under the install root.
	Name string
	// Summary is a one-line description for `qooq skill list`.
	Summary string
}

// All returns every skill this binary can install, in stable order.
func All() []Skill {
	return []Skill{
		{
			Name: "qooq-deploy",
			// Kept in step with SKILL.md's own description, which is the one
			// the model reads; this line is only for `qooq skill ls`.
			Summary: "Detect a local project's type, write its qooq.yaml, deploy it, and hand back a URL that works",
		},
	}
}

// Lookup finds a skill by name.
func Lookup(name string) (Skill, bool) {
	for _, s := range All() {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// An Agent is a coding agent a skill can be installed for.
//
// They differ only in which directory they scan; the format is the same, so
// nothing below branches on the agent except the destination.
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
	AgentCursor Agent = "cursor"
	// AgentShared is the directory Codex and Cursor both scan, for one
	// installation that serves both instead of two copies to update.
	AgentShared Agent = "agents"
)

// DefaultAgent keeps `qooq skill install` with no flags doing exactly what it
// did before other agents were supported.
const DefaultAgent = AgentClaude

// KnownAgents lists the install targets in the order help text shows them.
func KnownAgents() []Agent {
	return []Agent{AgentClaude, AgentCodex, AgentCursor, AgentShared}
}

// AgentNames renders the known agents for help text and error messages.
func AgentNames() string {
	names := make([]string, 0, len(KnownAgents()))
	for _, a := range KnownAgents() {
		names = append(names, string(a))
	}
	return strings.Join(names, "|")
}

// DirFor returns the user-level skills directory an agent reads.
//
// CODEX_HOME is honoured because Codex's own bundled skill-installer installs
// to $CODEX_HOME/skills/<name> and its preinstalled skills live there —
// observed on a machine with Codex installed, not inferred from the docs.
func DirFor(a Agent) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	switch a {
	case AgentClaude:
		return filepath.Join(home, ".claude", "skills"), nil
	case AgentCodex:
		if h := os.Getenv("CODEX_HOME"); h != "" {
			return filepath.Join(h, "skills"), nil
		}
		return filepath.Join(home, ".codex", "skills"), nil
	case AgentCursor:
		return filepath.Join(home, ".cursor", "skills"), nil
	case AgentShared:
		return filepath.Join(home, ".agents", "skills"), nil
	}
	return "", fmt.Errorf("unknown agent %q (want one of %s)", a, AgentNames())
}

// DefaultDir is the directory the default agent reads. Kept as its own name
// because `qooq skill ls` and older callers ask for "the" skills directory.
func DefaultDir() (string, error) {
	return DirFor(DefaultAgent)
}

// canonicalSkillHome is the skills directory SKILL.md's own example commands
// are written against.
//
// The document has to give an absolute path when it tells the agent to run one
// of the skill's scripts: the agent's working directory is the user's project,
// so a path relative to the skill would not resolve there. Install rewrites
// this prefix to wherever the skill is actually being installed, which is how
// one source document yields runnable commands for every agent. For the
// default agent the rewrite is the identity, so that installation is
// byte-for-byte what it was before any of this existed.
const canonicalSkillHome = "~/.claude/skills"

// renderDocPaths points the documented script paths at destDir.
func renderDocPaths(data []byte, destDir string) []byte {
	if destDir == canonicalSkillHome {
		return data
	}
	return bytes.ReplaceAll(data,
		[]byte(canonicalSkillHome+"/"), []byte(destDir+"/"))
}

// tildeHome shortens a path inside the home directory back to ~/…
//
// Both for readability and because the rewritten commands are read by a model
// and may be copied by a person: ~ keeps working, a baked-in /Users/<someone>
// does not.
func tildeHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest := strings.TrimPrefix(p, home+string(filepath.Separator)); rest != p {
		return "~/" + filepath.ToSlash(rest)
	}
	return p
}

// Install writes skill into destDir/<skill name>, returning the relative paths
// written (sorted) for reporting.
//
// Without force, Install refuses to touch an existing directory rather than
// silently overwriting edits the user may have made to their copy. With force,
// existing files are overwritten in place; files the user added are left alone,
// since removing them could destroy work the caller never saw.
//
// Shell scripts are written executable: go:embed does not preserve the exec
// bit, and a skill whose scripts cannot run is worse than no skill at all.
func Install(skill Skill, destDir string, force bool) ([]string, error) {
	root := filepath.Join(destDir, skill.Name)

	if _, err := os.Stat(root); err == nil {
		if !force {
			return nil, fmt.Errorf("%s already exists (use --force to overwrite)", root)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}

	var written []string
	err := fs.WalkDir(files, skill.Name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// p is relative to the embed root and already starts with skill.Name,
		// so joining it onto destDir reproduces the tree verbatim.
		target := filepath.Join(destDir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := files.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		if strings.HasSuffix(p, ".md") {
			data = renderDocPaths(data, tildeHome(destDir))
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		// Chmod explicitly: WriteFile only applies mode when creating, so an
		// overwrite (--force) of a previously non-executable file would keep
		// the old permissions.
		if err := os.Chmod(target, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", target, err)
		}
		written = append(written, target)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(written)
	return written, nil
}
