package ingress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// fakeExecRuntime embeds a nil runtime.Runtime so it satisfies the full
// interface at compile time (unused methods are never called in these
// tests), and overrides only what reload needs.
type fakeExecRuntime struct {
	runtime.Runtime
	gotCmd []string
	code   int
	output []byte
	err    error
}

func (f *fakeExecRuntime) ExecOutput(ctx context.Context, id runtime.ContainerID, cmd []string) (int, []byte, error) {
	f.gotCmd = cmd
	return f.code, f.output, f.err
}

// fakeExecOnlyRuntime has no ExecOutput at all, exercising reload's fallback
// path for a runtime that only implements the plain Exec method.
type fakeExecOnlyRuntime struct {
	runtime.Runtime
	code int
	err  error
}

func (f *fakeExecOnlyRuntime) Exec(ctx context.Context, id runtime.ContainerID, cmd []string) (int, error) {
	return f.code, f.err
}

func TestAdminListensOnContainerLoopbackOnly(t *testing.T) {
	m := testManager(Config{HTTPPort: 80})
	cfg := m.desiredConfig(nil)
	admin, _ := cfg["admin"].(map[string]any)
	got, _ := admin["listen"].(string)
	if got != "127.0.0.1:2019" {
		t.Fatalf("admin.listen = %q, want 127.0.0.1:2019 (container loopback, never a published port)", got)
	}
}

func TestReloadRequiresAProxyContainer(t *testing.T) {
	m := testManager(Config{HTTPPort: 80})
	m.configPath = filepath.Join(t.TempDir(), "caddy.json")
	if err := m.reload(context.Background(), "", []byte("{}")); err == nil {
		t.Fatal("reload with no proxy container id should fail, not silently no-op")
	}
}

func TestReloadSurfacesCaddyErrorOutput(t *testing.T) {
	fake := &fakeExecRuntime{code: 1, output: []byte("invalid config: unknown module\n")}
	m := testManager(Config{HTTPPort: 80})
	m.rt = fake
	m.configPath = filepath.Join(t.TempDir(), "caddy.json")

	err := m.reload(context.Background(), "proxy-container-id", []byte(`{"apps":{}}`))
	if err == nil {
		t.Fatal("expected an error when caddy reload exits non-zero")
	}
	if !strings.Contains(err.Error(), "unknown module") {
		t.Errorf("error %q must surface caddy's own stdout/stderr, not swallow it", err.Error())
	}

	// The command run is the reload documented in the code, not an HTTP call.
	want := []string{"caddy", "reload", "--config", configMountPath}
	if strings.Join(fake.gotCmd, " ") != strings.Join(want, " ") {
		t.Errorf("exec cmd = %v, want %v", fake.gotCmd, want)
	}

	got, err := os.ReadFile(m.configPath)
	if err != nil || string(got) != `{"apps":{}}` {
		t.Errorf("reload must still write the new config to the bind-mounted file before reloading, got %q, err %v", got, err)
	}
}

func TestReloadSucceedsWithoutExecOutputter(t *testing.T) {
	fake := &fakeExecOnlyRuntime{code: 0}
	m := testManager(Config{HTTPPort: 80})
	m.rt = fake
	m.configPath = filepath.Join(t.TempDir(), "caddy.json")

	if err := m.reload(context.Background(), "proxy-container-id", []byte("{}")); err != nil {
		t.Errorf("reload should succeed via the plain Exec fallback, got %v", err)
	}
}

// fakeCaddy answers a reload with success and a read-back with whatever
// config it is "running".
type fakeCaddy struct {
	runtime.Runtime
	running string
}

func (f *fakeCaddy) ExecOutput(ctx context.Context, id runtime.ContainerID, cmd []string) (int, []byte, error) {
	if cmd[0] == "wget" {
		return 0, []byte(f.running), nil
	}
	return 0, nil, nil
}

// A reload that exits 0 is not proof: on Linux a single-file bind mount kept
// Caddy reading the first config, every reload "succeeded" with it, and no
// route ever reached the proxy (E1). The sync counts as done only when what
// Caddy runs is what was written — compared as JSON, not as bytes.
func TestReloadChecksWhatCaddyIsRunning(t *testing.T) {
	m := testManager(Config{HTTPPort: 80})
	m.configPath = filepath.Join(t.TempDir(), "caddy.json")
	want := []byte(`{"apps":{"http":{"servers":{"qooq":{"listen":[":80"],"routes":[{"match":[{"host":["web.acme--prod.localhost"]}]}]}}}}}`)

	m.rt = &fakeCaddy{running: `{"qooq":{"listen":[":80"],"routes":[]}}`}
	if err := m.reload(context.Background(), "proxy", want); err == nil || !strings.Contains(err.Error(), "not running the config just written") {
		t.Fatalf("a stale running config was accepted: %v", err)
	}

	// Same config, other key order and whitespace: that is a match.
	m.rt = &fakeCaddy{running: `{ "qooq": { "routes": [ {"match": [ {"host": ["web.acme--prod.localhost"]} ]} ], "listen": [":80"] } }`}
	if err := m.reload(context.Background(), "proxy", want); err != nil {
		t.Fatalf("the config just written was not recognised: %v", err)
	}
}

// The proxy mounts the state directory, never the config file alone: a
// file mount follows the inode the atomic rename replaces.
func TestTheConfigLivesInAMountedDirectory(t *testing.T) {
	if filepath.Dir(configMountPath) != configMountDir {
		t.Fatalf("configMountPath %q is not inside configMountDir %q", configMountPath, configMountDir)
	}
}

// The same routes go to a recreated proxy again: it started from whatever file
// it found, and after a docker restart that was an empty one (E1, D2). Before
// this the unchanged hash short-circuited and every user'stayed unreachable
// until qooqd was restarted.
func TestARecreatedProxyGetsTheConfigAgain(t *testing.T) {
	m := testManager(Config{HTTPPort: 80})
	h := [32]byte{1}
	if !m.needsPush(h, "proxy-a") {
		t.Fatal("a first config was not pushed")
	}
	m.lastHash, m.lastProxy = h, "proxy-a"
	if m.needsPush(h, "proxy-a") {
		t.Error("an unchanged config was pushed again to the same proxy")
	}
	if !m.needsPush(h, "proxy-b") {
		t.Error("a recreated proxy did not get the unchanged config")
	}
	if !m.needsPush([32]byte{2}, "proxy-a") {
		t.Error("a changed config was not pushed")
	}
}
