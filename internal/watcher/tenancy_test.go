package watcher

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The default namespace must produce exactly the names it produced before
// namespaces reached networks and groups.
//
// This is the regression guard for the offline product, and it is the reason the
// qualification is conditional at all: changing a running component's network or
// group is not free, and every single-namespace install in existence is on the
// bare names.
func TestDefaultNamespaceNamesAreUnchanged(t *testing.T) {
	if got := defaultAppNetwork("default", "api"); got != "qooq-api" {
		t.Errorf("defaultAppNetwork(default, api) = %q, want qooq-api", got)
	}
	// An empty namespace normalizes to default and must behave identically:
	// older rows were written before the field existed.
	if got := defaultAppNetwork("", "api"); got != "qooq-api" {
		t.Errorf("defaultAppNetwork(\"\", api) = %q, want qooq-api", got)
	}
	if got := groupName("default", "api"); got != "api" {
		t.Errorf("groupName(default, api) = %q, want api", got)
	}
	if got := groupName("", "api"); got != "api" {
		t.Errorf("groupName(\"\", api) = %q, want api", got)
	}
	if got := namespaceQualified("default", "shared"); got != "shared" {
		t.Errorf("namespaceQualified(default, shared) = %q, want shared", got)
	}
}

func TestNonDefaultNamespaceScopesNetworkAndGroup(t *testing.T) {
	if got := defaultAppNetwork("acme--prod", "api"); got != "qooq-acme--prod-api" {
		t.Errorf("defaultAppNetwork = %q", got)
	}
	if got := groupName("acme--prod", "api"); got != "acme--prod-api" {
		t.Errorf("groupName = %q", got)
	}
}

// The escape this exists to close: two users whose manifests both say
// "network: shared" must not end up on one network.
func TestExplicitSharedNetworkCannotEscapeItsNamespace(t *testing.T) {
	a := namespaceQualified("alice--prod", "shared")
	b := namespaceQualified("bob--prod", "shared")
	if a == b {
		t.Fatalf("two namespaces both asking for \"shared\" got the same network %q", a)
	}
	if a != "alice--prod-shared" || b != "bob--prod-shared" {
		t.Fatalf("unexpected names: %q and %q", a, b)
	}
}

// Two namespaces with a component of the same name must not share a group.
// This is a bug that exists today, before any shared use: two stacks each
// holding a project called "api" share one group, so `qooq group stop api`
// stops both.
func TestTwoNamespacesWithTheSameComponentDoNotShareAGroup(t *testing.T) {
	if groupName("stack-a", "api") == groupName("stack-b", "api") {
		t.Fatal("two namespaces' api groups collide; `group stop` would hit both")
	}
}

// Applying it twice must not change the answer: the same manifest value is
// re-derived on every sweep.
func TestNamespaceQualifiedIsIdempotent(t *testing.T) {
	once := namespaceQualified("acme--prod", "shared")
	twice := namespaceQualified("acme--prod", once)
	if once != twice {
		t.Fatalf("not idempotent: %q then %q", once, twice)
	}
}

// A stack's network is built from the namespace alone, because a stack's name IS
// its namespace. Passing it through namespaceQualified instead would produce
// "acme-prod-qooq-acme-prod" — the mistake this shape exists to avoid.
func TestStackNetworkIsNotDoublePrefixed(t *testing.T) {
	got := stackNetwork("acme-prod")
	if got != "qooq-acme-prod" {
		t.Fatalf("stackNetwork = %q, want qooq-acme-prod", got)
	}
	if namespaceQualified("acme-prod", got) != "acme-prod-"+got {
		t.Skip("documenting only: this is why the stack network is not passed through namespaceQualified")
	}
}

// Every derived name has to remain a legal DNS label, because a network name and
// a group name both end up addressable.
func TestDerivedNamesStayWithinALabel(t *testing.T) {
	// The external manager bounds a qualified namespace to 61 characters plus the
	// separator; a component name is a DNS label of its own. The longest
	// realistic network name is therefore well inside docker's limits, but the
	// shape is asserted so a future change of separator is noticed here.
	ns := "averyverylongaccountslugthatispossible--production"
	got := defaultAppNetwork(ns, "api")
	if len(got) > 128 {
		t.Fatalf("network name is %d characters: %q", len(got), got)
	}
	for _, r := range got {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			t.Fatalf("network name %q contains %q, which a network name may not hold", got, r)
		}
	}
}

// Images live in one flat namespace on a shared daemon, and pruning deletes
// everything under "<name>:". With bare component names, one user's deploy
// deletes another user's image — and the victim only finds out when a restart
// tries to recreate a container from an image that is gone.
func TestTwoNamespacesDoNotShareAnImageRepo(t *testing.T) {
	a := imageRepo("alice--prod", "api")
	b := imageRepo("bob--prod", "api")
	if a == b {
		t.Fatalf("two namespaces share the image repo %q; one's prune deletes the other's images", a)
	}
	// The prune prefix is derived from the same value, so the isolation holds
	// for deletion and not only for naming.
	if strings.HasPrefix(b+":sha", a+":") || strings.HasPrefix(a+":sha", b+":") {
		t.Fatalf("one repo's prune prefix still matches the other's tags: %q vs %q", a, b)
	}
}

func TestTwoNamespacesDoNotShareAnArtifactDir(t *testing.T) {
	if artifactDirName("alice--prod", "api") == artifactDirName("bob--prod", "api") {
		t.Fatal("two namespaces share an artifact directory; pruning deletes each other's builds")
	}
}

// Offline keeps the exact image names it has: renaming would orphan every image
// already built, and the prune that follows would not recognise them.
func TestDefaultNamespaceKeepsBareImageAndArtifactNames(t *testing.T) {
	if got := imageRepo("default", "api"); got != "api" {
		t.Errorf("imageRepo(default, api) = %q, want api", got)
	}
	if got := imageRepo("", "api"); got != "api" {
		t.Errorf("imageRepo(\"\", api) = %q, want api", got)
	}
	if got := artifactDirName("default", "api"); got != "api" {
		t.Errorf("artifactDirName(default, api) = %q, want api", got)
	}
}

// A qualified namespace carries a double hyphen, which has to be legal in a
// docker reference. Verified against a real daemon when this was written; this
// pins the shape so a future separator change is noticed here rather than at a
// a user's first build.
func TestImageRepoIsALegalDockerReference(t *testing.T) {
	repo := imageRepo("alice--prod", "api")
	if !dockerRepoRe.MatchString(repo) {
		t.Fatalf("%q is not a legal docker repository name", repo)
	}
}

var dockerRepoRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-*)[a-z0-9]+)*$`)

// --- registry mode ---

// Registry mode off is the single-machine default: the image is built, named
// once, and deployed by that same name. No push, no pull, no second copy.
func TestRegistryOffChangesNothing(t *testing.T) {
	var w Watcher
	if got := w.buildTags("api:abc1234"); len(got) != 1 || got[0] != "api:abc1234" {
		t.Fatalf("buildTags = %v, want just the local tag", got)
	}
	ref, err := w.pushBuiltImage(context.Background(), "api:abc1234")
	if err != nil {
		t.Fatalf("push with no registry: %v", err)
	}
	if ref != "api:abc1234" {
		t.Fatalf("deploy ref = %q, want the local tag unchanged", ref)
	}
}

// With a registry, one build applies both names — so there is never a moment
// where the local and remote names could mean different images.
func TestRegistryModeBuildsBothNamesAtOnce(t *testing.T) {
	w := Watcher{registry: registryConfig{Prefix: "registry.example.com/qooq"}}
	got := w.buildTags("acme--prod-api:abc1234")
	if len(got) != 2 {
		t.Fatalf("buildTags = %v, want two", got)
	}
	if got[0] != "acme--prod-api:abc1234" {
		t.Errorf("the local name must come first and be unchanged, got %q", got[0])
	}
	if got[1] != "registry.example.com/qooq/acme--prod-api:abc1234" {
		t.Errorf("remote name = %q", got[1])
	}
}

// The remote ref inherits the namespace the local tag already carries, so two
// users cannot collide in the registry any more than they can locally.
func TestRemoteRefsKeepTheirNamespace(t *testing.T) {
	w := Watcher{registry: registryConfig{Prefix: "reg.example.com"}}
	a := w.registry.remoteRef(imageRepo("alice--prod", "api"))
	b := w.registry.remoteRef(imageRepo("bob--prod", "api"))
	if a == b {
		t.Fatalf("two users share the remote repo %q", a)
	}
}

// A trailing slash on the prefix must not produce a double slash, which some
// registries accept and others reject — a difference that would only show up
// against whichever registry a user happens to run.
func TestRegistryPrefixTrailingSlashIsTolerated(t *testing.T) {
	with := registryConfig{Prefix: "reg.example.com/qooq/"}
	without := registryConfig{Prefix: "reg.example.com/qooq"}
	if with.remoteRef("api:abc") != without.remoteRef("api:abc") {
		t.Fatalf("%q != %q", with.remoteRef("api:abc"), without.remoteRef("api:abc"))
	}
}

// A user's component lands on a network of its own even with no manifest
// (an image deploy): the engine's default bridge is shared by every such
// container. A single-user install keeps the engine default.
func TestASharedNodeComponentWithoutAManifestGetsItsOwnNetwork(t *testing.T) {
	w := &Watcher{}
	for _, tc := range []struct{ ns, want string }{
		{"acme--prod", "qooq-acme--prod-web"},
		{"", ""},
		{"staging", ""},
	} {
		spec, _ := json.Marshal(map[string]any{"namespace": tc.ns})
		req, err := w.buildDeployRequest(&Target{ComponentName: "web", DeploySpec: string(spec)}, "nginx:alpine", deploySpecFields{})
		if err != nil {
			t.Fatal(err)
		}
		if req.Network != tc.want {
			t.Errorf("namespace %q: network = %q, want %q", tc.ns, req.Network, tc.want)
		}
	}
}

// The watcher passes qooq.yaml's restart policy through and leaves an
// omitted one empty: the default ("always") belongs to qooqd alone, for
// every install, and "no" still means no.
func TestTheRestartPolicyIsPassedThrough(t *testing.T) {
	w := &Watcher{}
	for _, tc := range []struct{ ns, manifest, want string }{
		{"acme--prod", "", ""},
		{"acme--prod", "no", "no"},
		{"acme--prod", "on-failure", "on-failure"},
		{"", "", ""},
	} {
		spec, _ := json.Marshal(map[string]any{"namespace": tc.ns})
		req, err := w.buildDeployRequest(&Target{ComponentName: "web", DeploySpec: string(spec)}, "nginx:alpine",
			deploySpecFields{RestartPolicy: tc.manifest})
		if err != nil {
			t.Fatal(err)
		}
		if req.RestartPolicy != tc.want {
			t.Errorf("namespace %q, manifest %q: restart = %q, want %q", tc.ns, tc.manifest, req.RestartPolicy, tc.want)
		}
	}
}
