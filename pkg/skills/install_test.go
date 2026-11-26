package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The skill tells the agent to run its scripts by absolute path, because the
// agent's working directory is the user's project. Installing it somewhere
// other than the default therefore has to rewrite those paths, or every
// command in the document points at a directory this agent never populated —
// and the failure looks like a missing script rather than a wrong install.
func TestInstallPointsTheDocumentedPathsAtTheInstallDirectory(t *testing.T) {
	dest := t.TempDir()
	skill, ok := Lookup("qooq-deploy")
	if !ok {
		t.Fatal("qooq-deploy is not bundled")
	}
	if _, err := Install(skill, dest, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc, err := os.ReadFile(filepath.Join(dest, skill.Name, "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed SKILL.md: %v", err)
	}
	got := string(doc)

	if strings.Contains(got, canonicalSkillHome+"/") {
		t.Errorf("installed SKILL.md still points at %s, so its commands run the wrong copy", canonicalSkillHome)
	}
	want := filepath.Join(dest, skill.Name, "scripts")
	if !strings.Contains(got, want) {
		t.Errorf("installed SKILL.md never mentions %s", want)
	}
}

// The default agent's installation predates every other one. Rewriting is the
// identity for it, and this pins that: a change that quietly altered what
// Claude Code receives would be a regression for every existing user.
func TestDefaultAgentInstallIsUnchanged(t *testing.T) {
	dir, err := DirFor(DefaultAgent)
	if err != nil {
		t.Fatalf("default dir: %v", err)
	}
	embedded, err := files.ReadFile("qooq-deploy/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	rendered := renderDocPaths(embedded, tildeHome(dir))
	if string(rendered) != string(embedded) {
		t.Error("rendering changed SKILL.md for the default agent; its installation must stay byte-for-byte")
	}
}

// Rendering is for prose, not for code: a script rewritten by a string
// substitution is a script nobody reviewed.
func TestScriptsAreInstalledVerbatimAndExecutable(t *testing.T) {
	dest := t.TempDir()
	skill, _ := Lookup("qooq-deploy")
	if _, err := Install(skill, dest, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	const script = "qooq-deploy/scripts/resolve-qooq.sh"
	embedded, err := files.ReadFile(script)
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	onDisk := filepath.Join(dest, filepath.FromSlash(script))
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if string(got) != string(embedded) {
		t.Error("a shell script was modified on the way in")
	}
	info, err := os.Stat(onDisk)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (%v)", onDisk, info.Mode().Perm())
	}
}

// Every agent must resolve to its own directory. A shared fallback would put
// two agents in one place and make "installed" mean different things.
func TestEachAgentHasItsOwnDirectory(t *testing.T) {
	seen := map[string]Agent{}
	for _, a := range KnownAgents() {
		dir, err := DirFor(a)
		if err != nil {
			t.Fatalf("DirFor(%s): %v", a, err)
		}
		if other, dup := seen[dir]; dup {
			t.Errorf("%s and %s both install to %s", a, other, dir)
		}
		seen[dir] = a
	}
	if _, err := DirFor(Agent("nope")); err == nil {
		t.Error("an unknown agent must be an error, not a silent default")
	}
}

// CODEX_HOME is how Codex itself relocates its home; ignoring it installs into
// a directory that instance does not read.
func TestCodexHomeIsHonoured(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/codex-elsewhere")
	dir, err := DirFor(AgentCodex)
	if err != nil {
		t.Fatalf("DirFor: %v", err)
	}
	if dir != "/tmp/codex-elsewhere/skills" {
		t.Errorf("got %s, want /tmp/codex-elsewhere/skills", dir)
	}
}
