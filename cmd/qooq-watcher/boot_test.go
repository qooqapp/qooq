package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// watcherBinary builds this package once per test run: the check under test
// sits in the module's OnStart, so only the real composition root shows
// whether a refusal there actually stops the process.
func watcherBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := filepathTemp()
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "qooq-watcher")
		out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("%s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build qooq-watcher: %v", buildErr)
	}
	return builtBin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// bootWatcher starts the binary in its own directory and port, with the given
// settings, and reports either that it exited (with its output) or that the
// watcher module finished starting.
//
// Not /healthz: the http server listens before the watcher module's OnStart
// runs, so /healthz answers 200 in the moment before a refusal — under load
// that moment is long enough to be seen, and a watcher that was about to
// refuse read as one that started.
func bootWatcher(t *testing.T, env map[string]string) (exited bool, output string) {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	// Built before the clock starts: a cold build cache (a new toolchain, a
	// busy `make test`) can take longer than the whole budget, and the binary
	// was then killed before it could decide — which reads as "it exited
	// without saying why", the very failure this test is about.
	bin := watcherBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + dir, "PATH=/usr/bin:/bin",
		"QOOQ_WATCHER_HTTP_PORT=" + strconv.Itoa(port),
		"QOOQ_WATCHER_SQLITE_PATH=" + filepath.Join(dir, "watcher.db"),
		"QOOQ_WATCHER_MOUNTS_DIR=" + filepath.Join(dir, "mounts"),
		"QOOQ_WATCHER_ARTIFACTS_DIR=" + filepath.Join(dir, "artifacts"),
		// Nothing listens here: the watcher must decide without a qooqd.
		"QOOQ_WATCHER_QOOQD_ADDR=http://127.0.0.1:1",
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	for {
		select {
		case <-done:
			return true, out.String()
		case <-time.After(200 * time.Millisecond):
		}
		if strings.Contains(out.String(), "watcher module started") {
			cancel()
			<-done
			return false, out.String()
		}
		if ctx.Err() != nil {
			t.Fatalf("watcher neither exited nor answered in time:\n%s", out.String())
		}
	}
}

// With builds on another node and no registry, every deploy would fail with
// a pull error on the deploy node, far from the cause. The watcher refuses to
// start instead and names both settings.
func TestASeparateBuildNodeWithoutARegistryDoesNotStart(t *testing.T) {
	exited, out := bootWatcher(t, map[string]string{
		"QOOQ_WATCHER_QOOQD_BUILD_ADDR": "http://127.0.0.1:2",
	})
	if !exited {
		t.Fatalf("the watcher started:\n%s", out)
	}
	for _, want := range []string{"qooqd.build_addr", "registry.prefix"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %s:\n%s", want, out)
		}
	}
}

// The other side: the same build node with a registry starts, so the refusal
// above is about the missing registry and not about the build node.
func TestASeparateBuildNodeWithARegistryStarts(t *testing.T) {
	exited, out := bootWatcher(t, map[string]string{
		"QOOQ_WATCHER_QOOQD_BUILD_ADDR": "http://127.0.0.1:2",
		"QOOQ_WATCHER_REGISTRY_PREFIX":  "localhost:5000/qooq",
	})
	if exited {
		t.Fatalf("the watcher did not start:\n%s", out)
	}
}

func filepathTemp() (string, error) { return os.MkdirTemp("", "qooq-watcher-boot-") }

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

// lockedBuffer is the child's stdout and stderr, read while it is still being
// written: exec copies into it from its own goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
