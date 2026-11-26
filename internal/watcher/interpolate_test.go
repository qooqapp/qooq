package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterpolateExpand(t *testing.T) {
	s := newInterpolationScope(processEnvOnlyResolver())
	s.set("app", "todo")
	s.set("service.db.host", "todo-db")
	s.set("service.db.port", "5432")

	tests := []struct {
		in   string
		want string
	}{
		{in: "plain", want: "plain"},
		{in: "${app}", want: "todo"},
		{in: "postgres://${service.db.host}:${service.db.port}/x", want: "postgres://todo-db:5432/x"},
		{in: "no $ {app} substitution", want: "no $ {app} substitution"},
	}
	for _, tc := range tests {
		got, err := s.expand(tc.in)
		if err != nil {
			t.Errorf("expand(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A typo must fail loudly. Expanding to "" produces an app that starts and
// then misbehaves for reasons nothing in the logs explains.
func TestInterpolateUnknownReferenceIsAnError(t *testing.T) {
	s := newInterpolationScope(processEnvOnlyResolver())
	s.set("service.db.host", "x")

	_, err := s.expand("${service.db.hst}")
	if err == nil {
		t.Fatal("unknown reference silently expanded")
	}
	// The message should help find the typo.
	if !strings.Contains(err.Error(), "service.db.host") {
		t.Errorf("error does not list available keys: %v", err)
	}
}

func TestInterpolateEnvNamespace(t *testing.T) {
	t.Setenv("QOOQ_TEST_SECRET", "s3cr3t")
	s := newInterpolationScope(processEnvOnlyResolver())

	got, err := s.expand("${env.QOOQ_TEST_SECRET}")
	if err != nil || got != "s3cr3t" {
		t.Fatalf("expand env = %q err=%v", got, err)
	}
	if _, err := s.expand("${env.QOOQ_TEST_DEFINITELY_UNSET}"); err == nil {
		t.Error("unset env var should be an error, not an empty string")
	}
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# a comment

FOO=bar
export EXPORTED=yes
QUOTED="has spaces"
SINGLE='single'
EMPTY=
URL=postgres://user:pw@host:5432/db?sslmode=disable
NOT_EXPANDED=${service.db.host}
  SPACED = padded
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	want := map[string]string{
		"FOO":      "bar",
		"EXPORTED": "yes",
		"QUOTED":   "has spaces",
		"SINGLE":   "single",
		"EMPTY":    "",
		"URL":      "postgres://user:pw@host:5432/db?sslmode=disable",
		// An env file is data: ${...} inside it reaches the app verbatim.
		"NOT_EXPANDED": "${service.db.host}",
		"SPACED":       "padded",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vars, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseEnvFileRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("this line has no equals sign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseEnvFile(path); err == nil {
		t.Fatal("a line without = should be rejected")
	}
}

// Later files win, matching how repeated shell assignments behave.
func TestResolveEnvFilesOrdering(t *testing.T) {
	w := testWatcher(t, "open")
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, "base.env"), []byte("A=1\nB=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "override.env"), []byte("B=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := w.resolveEnvFiles(demoRef, clone, []string{"base.env", "override.env"})
	if err != nil {
		t.Fatalf("resolveEnvFiles: %v", err)
	}
	if got["A"] != "1" || got["B"] != "2" {
		t.Errorf("got %v, want A=1 B=2", got)
	}
}

// The "volume:/path" form is the reason env files reuse the mount source
// classification: an operator-managed secrets file living in a volume.
func TestResolveEnvFileFromVolume(t *testing.T) {
	w := testWatcher(t, "open")
	volDir := w.volumePath(demoRef, "appdata")
	if err := os.MkdirAll(filepath.Join(volDir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "config", "app.env"), []byte("FROM_VOLUME=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := w.resolveEnvFiles(demoRef, t.TempDir(), []string{"appdata:/config/app.env"})
	if err != nil {
		t.Fatalf("resolveEnvFiles: %v", err)
	}
	if got["FROM_VOLUME"] != "yes" {
		t.Errorf("got %v, want FROM_VOLUME=yes", got)
	}
}

func TestResolveEnvFileRejects(t *testing.T) {
	w := testWatcher(t, "repo-only")
	clone := t.TempDir()

	if _, err := w.resolveEnvFiles(demoRef, clone, []string{"/etc/passwd"}); err == nil {
		t.Error("repo-only policy should reject an absolute env_file path")
	}
	// A bare name is a volume; a volume is a directory, not a file.
	if _, err := w.resolveEnvFiles(demoRef, clone, []string{"appdata"}); err == nil {
		t.Error("bare volume name should be rejected with a hint")
	}
	if _, err := w.resolveEnvFiles(demoRef, clone, []string{"../outside.env"}); err == nil {
		t.Error("path traversal should be rejected")
	}
}

// Precedence: env_file loses to run.env, and both are visible to ${...}
// consumers by the time the deploy request is built.
func TestApplyRunConfigEnvPrecedence(t *testing.T) {
	w := testWatcher(t, "open")
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, ".env"), []byte("SHARED=from-file\nONLY_FILE=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QOOQ_TEST_PW", "hunter2")

	mf := &manifest{
		App: "demo",
		Run: manifestRun{
			EnvFile: []string{".env"},
			Env: map[string]string{
				"SHARED":   "from-run-env",
				"PASSWORD": "${env.QOOQ_TEST_PW}",
				"SELF":     "${component}@${commit}",
			},
			Volumes: []manifestMount{{From: "appdata", To: "/app/data"}},
		},
	}
	target := &Target{Name: "demo", ComponentName: "demo"}

	spec, err := w.applyRunConfig(context.Background(), target, mf, deploySpecFields{Network: "qooq-demo"}, clone, "abc1234")
	if err != nil {
		t.Fatalf("applyRunConfig: %v", err)
	}
	if spec.Env["SHARED"] != "from-run-env" {
		t.Errorf("run.env should win over env_file, got %q", spec.Env["SHARED"])
	}
	if spec.Env["ONLY_FILE"] != "yes" {
		t.Errorf("env_file value missing: %v", spec.Env)
	}
	if spec.Env["PASSWORD"] != "hunter2" {
		t.Errorf("${env.*} not expanded: %q", spec.Env["PASSWORD"])
	}
	if spec.Env["SELF"] != "demo@abc1234" {
		t.Errorf("deploy-context refs not expanded: %q", spec.Env["SELF"])
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].ContainerPath != "/app/data" {
		t.Fatalf("volume not materialized: %+v", spec.Mounts)
	}
}

// A volume's host path is only knowable after materialization, so it must be
// resolvable from run.env — that is what makes it useful to an app that needs
// to be told where its data lives.
func TestApplyRunConfigVolumeReference(t *testing.T) {
	w := testWatcher(t, "open")
	mf := &manifest{
		App: "demo",
		Run: manifestRun{
			Volumes: []manifestMount{{From: "appdata", To: "/app/data"}},
			Env:     map[string]string{"DATA_DIR": "${volume.appdata.target}"},
		},
	}
	spec, err := w.applyRunConfig(context.Background(), &Target{Name: "demo", ComponentName: "demo"}, mf, deploySpecFields{}, t.TempDir(), "sha")
	if err != nil {
		t.Fatalf("applyRunConfig: %v", err)
	}
	if spec.Env["DATA_DIR"] != "/app/data" {
		t.Errorf("DATA_DIR = %q, want /app/data", spec.Env["DATA_DIR"])
	}
}
