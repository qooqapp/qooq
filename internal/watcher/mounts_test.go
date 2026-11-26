package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestParseMountEntry(t *testing.T) {
	tests := []struct {
		in      string
		want    manifestMount
		wantErr bool
	}{
		{in: "./config.toml:/app/config.toml", want: manifestMount{From: "./config.toml", To: "/app/config.toml"}},
		{in: "appdata:/app/data:rw", want: manifestMount{From: "appdata", To: "/app/data", Mode: "rw"}},
		{in: "/etc/app.env:/app/.env:ro", want: manifestMount{From: "/etc/app.env", To: "/app/.env", Mode: "ro"}},
		{in: "  spaced:/app/x  ", want: manifestMount{From: "spaced", To: "/app/x"}},
		{in: "onlyone", wantErr: true},
		{in: "a:/b:sideways", wantErr: true},
		{in: "a:/b:ro:extra", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseMountEntry(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMountEntry(%q) = %+v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMountEntry(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseMountEntry(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// The short string and the mapping must produce the same mount: the whole
// point of accepting both is that neither is second-class.
func TestManifestMountUnmarshalBothForms(t *testing.T) {
	var short []manifestMount
	if err := yaml.Unmarshal([]byte(`["./config.toml:/app/config.toml:ro"]`), &short); err != nil {
		t.Fatalf("short form: %v", err)
	}
	var long []manifestMount
	if err := yaml.Unmarshal([]byte("- from: ./config.toml\n  to: /app/config.toml\n  mode: ro\n"), &long); err != nil {
		t.Fatalf("long form: %v", err)
	}
	if len(short) != 1 || len(long) != 1 || short[0] != long[0] {
		t.Fatalf("forms disagree: short=%+v long=%+v", short, long)
	}
}

func TestClassifyMountSource(t *testing.T) {
	tests := map[string]mountSourceKind{
		"/etc/app.env":    mountSourceHost,
		"appdata":         mountSourceVolume,
		"db-data":         mountSourceVolume,
		"./config.toml":   mountSourceRepo,
		"config/app.toml": mountSourceRepo,
		"config.toml":     mountSourceRepo, // has a dot: a path, not a volume name
		"Uppercase":       mountSourceRepo,
		"deploy/sub/dir":  mountSourceRepo,
	}
	for in, want := range tests {
		if got := classifyMountSource(in); got != want {
			t.Errorf("classifyMountSource(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseMountsPolicy(t *testing.T) {
	if p, err := parseMountsPolicy(""); err != nil || p.repoOnly || p.allowPrefixes != nil {
		t.Errorf("empty policy should be open, got %+v err=%v", p, err)
	}
	if p, err := parseMountsPolicy("repo-only"); err != nil || !p.repoOnly {
		t.Errorf("repo-only: got %+v err=%v", p, err)
	}
	p, err := parseMountsPolicy("allowlist:/etc/qooq, /srv/data")
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	if len(p.allowPrefixes) != 2 {
		t.Fatalf("allowlist prefixes = %v", p.allowPrefixes)
	}
	if _, err := parseMountsPolicy("allowlist:"); err == nil {
		t.Error("empty allowlist should be rejected")
	}
	if _, err := parseMountsPolicy("whatever"); err == nil {
		t.Error("unknown policy should be rejected")
	}
}

func TestMountsPolicyCheckHostPath(t *testing.T) {
	open, _ := parseMountsPolicy("open")
	if err := open.checkHostPath("/anything"); err != nil {
		t.Errorf("open policy rejected a host path: %v", err)
	}

	repoOnly, _ := parseMountsPolicy("repo-only")
	if err := repoOnly.checkHostPath("/etc/passwd"); err == nil {
		t.Error("repo-only policy allowed a host path")
	}

	allow, _ := parseMountsPolicy("allowlist:/etc/qooq")
	if err := allow.checkHostPath("/etc/qooq/app.env"); err != nil {
		t.Errorf("allowlisted path rejected: %v", err)
	}
	if err := allow.checkHostPath("/etc/qooq"); err != nil {
		t.Errorf("allowlist prefix itself rejected: %v", err)
	}
	// A sibling that merely shares the prefix string must not slip through.
	if err := allow.checkHostPath("/etc/qooq-secrets/x"); err == nil {
		t.Error("allowlist matched on a string prefix instead of a path prefix")
	}
	if err := allow.checkHostPath("/etc/passwd"); err == nil {
		t.Error("non-allowlisted path accepted")
	}
}

// A repo-planted symlink inside an allowlisted directory must not let a
// mount escape to whatever the symlink actually points at — the string
// prefix check alone (filepath.Clean only) would let this through, defeating
// the whole point of the allowlist policy.
func TestMountsPolicyCheckHostPathSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	allowedDir := filepath.Join(dir, "allowed")
	outsideDir := filepath.Join(dir, "outside")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outsideDir, "secret")
	if err := os.WriteFile(secret, []byte("shh"), 0o644); err != nil {
		t.Fatal(err)
	}

	// allowedDir/escape -> outsideDir, entirely outside the allowlist.
	escape := filepath.Join(allowedDir, "escape")
	if err := os.Symlink(outsideDir, escape); err != nil {
		t.Fatal(err)
	}

	policy, err := parseMountsPolicy("allowlist:" + allowedDir)
	if err != nil {
		t.Fatalf("parseMountsPolicy: %v", err)
	}

	if err := policy.checkHostPath(filepath.Join(escape, "secret")); err == nil {
		t.Error("symlink escape via an allowlisted directory was allowed")
	}
	if err := policy.checkHostPath(escape); err == nil {
		t.Error("symlink itself (pointing outside the allowlist) was allowed")
	}

	// A normal, non-symlinked path inside the allowed directory must still
	// pass — the fix must not make the allowlist stricter than it was.
	normal := filepath.Join(allowedDir, "config.env")
	if err := os.WriteFile(normal, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := policy.checkHostPath(normal); err != nil {
		t.Errorf("normal allowlisted path rejected: %v", err)
	}
	// A path that doesn't exist yet under the allowed dir must still pass
	// (existing behavior for host paths qooq hasn't created yet).
	if err := policy.checkHostPath(filepath.Join(allowedDir, "not-yet-created", "x")); err != nil {
		t.Errorf("not-yet-created allowlisted path rejected: %v", err)
	}

	// The open policy must be completely unaffected: it must allow the very
	// same symlink without even looking at it.
	open, _ := parseMountsPolicy("open")
	if err := open.checkHostPath(escape); err != nil {
		t.Errorf("open policy rejected a path, should never police host paths: %v", err)
	}
}

// testWatcher builds the minimum Watcher materializeMounts needs.
func testWatcher(t *testing.T, policy string) *Watcher {
	t.Helper()
	p, err := parseMountsPolicy(policy)
	if err != nil {
		t.Fatalf("policy %q: %v", policy, err)
	}
	return &Watcher{
		log:          zap.NewNop(),
		mountsDir:    t.TempDir(),
		mountsPolicy: p,
		// Matches the production default (configAllowProcessEnv defaults to
		// true). Set explicitly because this fixture skips OnStart, and the
		// zero value is the opposite — deliberately, since a Watcher that
		// somehow never read its configuration should deny rather than allow.
		allowProcessEnv: true,
	}
}

func TestMaterializeMountsRepoFileAndVolume(t *testing.T) {
	w := testWatcher(t, "open")
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "config", "app.toml"), []byte("greeting = \"v1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := w.materializeMounts(demoRef, clone, "abc1234",
		[]manifestMount{{From: "config/app.toml", To: "/app/config.toml"}},
		[]manifestMount{{From: "appdata", To: "/app/data"}},
	)
	if err != nil {
		t.Fatalf("materializeMounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got), got)
	}

	file := got[0]
	if !file.ReadOnly {
		t.Error("run.files entry should default to read-only")
	}
	wantPath := filepath.Join(w.mountsDir, "demo", "files", "abc1234", "config", "app.toml")
	if file.HostPath != wantPath {
		t.Errorf("file host path = %q, want %q", file.HostPath, wantPath)
	}
	content, err := os.ReadFile(file.HostPath)
	if err != nil || string(content) != "greeting = \"v1\"\n" {
		t.Errorf("materialized content = %q err=%v", content, err)
	}

	vol := got[1]
	if vol.ReadOnly {
		t.Error("run.volumes entry should default to read-write")
	}
	wantVol := filepath.Join(w.mountsDir, "demo", "volumes", "appdata")
	if vol.HostPath != wantVol {
		t.Errorf("volume host path = %q, want %q", vol.HostPath, wantVol)
	}
	if info, err := os.Stat(wantVol); err != nil || !info.IsDir() {
		t.Errorf("volume dir not created: err=%v", err)
	}
}

func TestMaterializeMountsModeOverride(t *testing.T) {
	w := testWatcher(t, "open")
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, "app.toml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := w.materializeMounts(demoRef, clone, "sha", []manifestMount{
		{From: "./app.toml", To: "/app/app.toml", Mode: "rw"},
	}, nil)
	if err != nil {
		t.Fatalf("materializeMounts: %v", err)
	}
	if got[0].ReadOnly {
		t.Error("mode: rw should override the files default")
	}
}

func TestMaterializeMountsRejects(t *testing.T) {
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, "app.toml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		policy  string
		files   []manifestMount
		volumes []manifestMount
	}{
		{name: "relative target", policy: "open", files: []manifestMount{{From: "./app.toml", To: "app/config.toml"}}},
		{name: "missing repo source", policy: "open", files: []manifestMount{{From: "./nope.toml", To: "/app/x"}}},
		{name: "path traversal", policy: "open", files: []manifestMount{{From: "../outside", To: "/app/x"}}},
		{name: "host path under repo-only", policy: "repo-only", files: []manifestMount{{From: "/etc/passwd", To: "/app/x"}}},
		{name: "duplicate target", policy: "open", files: []manifestMount{
			{From: "./app.toml", To: "/app/x"},
			{From: "./app.toml", To: "/app/x"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := testWatcher(t, tc.policy)
			if _, err := w.materializeMounts(demoRef, clone, "sha", tc.files, tc.volumes); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

// A symlink pointing outside the clone must not become a way to copy host
// files into a container — the same escape resolveWithin blocks for build
// paths.
func TestMaterializeMountsBlocksSymlinkEscape(t *testing.T) {
	w := testWatcher(t, "open")
	clone := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(clone, "sneaky.toml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := w.materializeMounts(demoRef, clone, "sha",
		[]manifestMount{{From: "./sneaky.toml", To: "/app/config.toml"}}, nil); err == nil {
		t.Fatal("symlink escape was allowed")
	}
}

// Prune must keep current+previous and must never touch volumes: one is a
// cache of file copies, the other is the app's data.
func TestPruneOldFileMountsKeepsVolumes(t *testing.T) {
	w := testWatcher(t, "open")
	filesDir := w.filesDir(demoRef)
	for _, sha := range []string{"aaa", "bbb", "ccc", "ddd"} {
		if err := os.MkdirAll(filepath.Join(filesDir, sha), 0o755); err != nil {
			t.Fatal(err)
		}
		// Distinct mtimes so "newest two" is well-defined.
		mtime := filepath.Join(filesDir, sha)
		if err := os.Chtimes(mtime, testTime(sha), testTime(sha)); err != nil {
			t.Fatal(err)
		}
	}
	volDir := w.volumePath(demoRef, "appdata")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}

	w.pruneOldFileMounts(demoRef)

	entries, err := os.ReadDir(filesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != keepShaDirs {
		t.Fatalf("kept %d sha dirs, want %d", len(entries), keepShaDirs)
	}
	kept := map[string]bool{}
	for _, e := range entries {
		kept[e.Name()] = true
	}
	if !kept["ccc"] || !kept["ddd"] {
		t.Errorf("pruned the wrong dirs, kept %v", kept)
	}
	if _, err := os.Stat(volDir); err != nil {
		t.Errorf("prune removed a volume dir: %v", err)
	}
}

func TestCheckOperatorMountsIgnoresRepoOnly(t *testing.T) {
	// repo-only constrains what a repo's manifest may ask for, not what the
	// operator configured on the target itself.
	w := testWatcher(t, "repo-only")
	if err := w.checkOperatorMounts([]mountSpec{{HostPath: "/etc/qooq/app.env", ContainerPath: "/app/.env"}}); err != nil {
		t.Errorf("repo-only should not restrict operator mounts: %v", err)
	}

	w = testWatcher(t, "allowlist:/etc/qooq")
	if err := w.checkOperatorMounts([]mountSpec{{HostPath: "/etc/other", ContainerPath: "/app/.env"}}); err == nil {
		t.Error("allowlist should restrict operator mounts too")
	}
	if err := w.checkOperatorMounts([]mountSpec{{HostPath: "relative", ContainerPath: "/app/.env"}}); err == nil {
		t.Error("relative host_path should be rejected")
	}
}

// testTime gives each sha dir a distinct, ordered mtime (aaa < bbb < ...).
func testTime(sha string) time.Time {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(sha[0]) * time.Hour)
}

// demoRef is the unnamespaced component the mount tests operate on.
var demoRef = componentRef{name: "demo"}
