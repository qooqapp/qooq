package watcher

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The leak this whole milestone exists for, pinned.
//
// Before build isolation, build.command ran with os.Environ(), so a qooq.yaml
// of `build: {command: ["sh","-c","env"]}` in any repository you do not control read
// QOOQ_WATCHER_SECRET_KEY — the passphrase that decrypts every user's git
// credentials and secrets — along with every QOOQ_SECRET_*. It defeated M3
// completely: the interpolation layer stopped handing out the environment while
// the build layer handed out the same thing wholesale.
func TestBuildCommandCannotReadQooqsOwnSecrets(t *testing.T) {
	t.Setenv("QOOQ_WATCHER_SECRET_KEY", "the-passphrase-that-decrypts-every-user")
	t.Setenv("QOOQ_SECRET_STRIPE", "sk_live_someone_elses")
	t.Setenv("QOOQ_WATCHER_HTTP_TOKEN", "the-watchers-api-token")
	t.Setenv("ORDINARY_VALUE", "a-toolchain-cache-path")

	w := &Watcher{log: zap.NewNop()}
	dir := t.TempDir()
	out := dir + "/captured.txt"

	// Exactly what a hostile manifest would run.
	if err := w.runBuildCommand(context.Background(), &Target{Name: "attacker"}, dir,
		[]string{"sh", "-c", "env > " + out}); err != nil {
		t.Fatalf("build command: %v", err)
	}

	captured, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read captured env: %v", err)
	}
	got := string(captured)

	for _, secret := range []string{
		"the-passphrase-that-decrypts-every-user",
		"sk_live_someone_elses",
		"the-watchers-api-token",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("build.command could read %q from the watcher's environment", secret)
		}
	}

	// Everything else is still inherited: a build legitimately needs the
	// machine's environment, and stripping it to a fixed allowlist would break
	// whichever toolchain nobody thought of.
	if !strings.Contains(got, "a-toolchain-cache-path") {
		t.Error("the ordinary environment was stripped too; builds that need it will break")
	}
}

func TestBuildCommandEnvDropsEveryQooqVariable(t *testing.T) {
	t.Setenv("QOOQ_ANYTHING_AT_ALL", "x")
	t.Setenv("QOOQ_", "y")
	t.Setenv("NOT_QOOQ_PREFIXED", "z")

	for _, kv := range buildCommandEnv() {
		if strings.HasPrefix(kv, "QOOQ_") {
			t.Errorf("a QOOQ_ variable survived the filter: %q", kv)
		}
	}
	// And the filter is a prefix match on the name, not a substring match on
	// the whole entry: a variable whose *value* mentions qooq must survive.
	t.Setenv("HOMEPAGE", "https://qooq.example")
	var found bool
	for _, kv := range buildCommandEnv() {
		if strings.HasPrefix(kv, "HOMEPAGE=") {
			found = true
		}
	}
	if !found {
		t.Error("a variable was dropped because its value mentioned qooq")
	}
}

// For an untrusted repository the environment filter is not enough: the command
// still runs as the watcher, with its filesystem and its network. It is refused.
func TestUntrustedRepositoriesCannotRunBuildCommandAtAll(t *testing.T) {
	trusted := &Watcher{log: zap.NewNop(), untrustedRepos: false}
	if err := trusted.checkBuildCommandAllowed(); err != nil {
		t.Fatalf("a trusted watcher refused build.command: %v", err)
	}

	untrusted := &Watcher{log: zap.NewNop(), untrustedRepos: true}
	err := untrusted.checkBuildCommandAllowed()
	if err == nil {
		t.Fatal("an untrusted watcher allowed build.command")
	}
	// The message has to say what to do instead, or the author's next move is
	// to look for the bug in their own manifest.
	for _, want := range []string{"Dockerfile", "build.template"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s: %v", want, err)
		}
	}
}

// The second host-reaching path: a manifest binding any host path into its
// container. "open" is the right default for the offline product and wrong for
// an untrusted one, so it is narrowed rather than left to a second setting
// somebody has to remember.
func TestUntrustedRepositoriesCannotMountArbitraryHostPaths(t *testing.T) {
	open, err := parseMountsPolicy(mountsPolicyOpen)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	trusted := &Watcher{untrustedRepos: false}
	if got := trusted.effectiveMountsPolicy(open); got.raw != mountsPolicyOpen {
		t.Errorf("a trusted watcher had its policy narrowed to %q", got.raw)
	}

	untrusted := &Watcher{untrustedRepos: true}
	if got := untrusted.effectiveMountsPolicy(open); got.raw != mountsPolicyRepoOnly {
		t.Errorf("an untrusted watcher kept policy %q, want %q", got.raw, mountsPolicyRepoOnly)
	}
}

// A policy already stricter than repo-only must not be loosened by the
// narrowing.
func TestNarrowingNeverLoosensAStricterPolicy(t *testing.T) {
	strict, err := parseMountsPolicy(mountsPolicyRepoOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	untrusted := &Watcher{untrustedRepos: true}
	if got := untrusted.effectiveMountsPolicy(strict); got.raw != mountsPolicyRepoOnly {
		t.Errorf("policy became %q", got.raw)
	}
}
