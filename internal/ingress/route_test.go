package ingress

import (
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

func testManager(cfg Config) *Manager {
	return &Manager{
		cfg: cfg,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func runningComponent(name string, ports []runtime.PortMapping, ing *core.IngressSpec) *core.Component {
	return &core.Component{
		Spec: core.ComponentSpec{
			Name:    name,
			Ports:   ports,
			Ingress: ing,
		},
		Status: core.ComponentStatus{Phase: runtime.PhaseRunning},
	}
}

func TestRouteForPortSelection(t *testing.T) {
	m := testManager(Config{AdvertiseHost: func() string { return "192.168.1.50" }})
	ports := []runtime.PortMapping{{ContainerPort: 8080}, {ContainerPort: 9090}}

	// Default: the first declared port, since manifest order is deterministic.
	r, ok := m.routeFor(runningComponent("api", ports, nil))
	if !ok || !strings.HasSuffix(r.upstream, ":8080") {
		t.Errorf("default upstream = %q, want the first port", r.upstream)
	}

	// An app that exposes several ports has to be able to say which one is
	// the public one.
	r, ok = m.routeFor(runningComponent("api", ports, &core.IngressSpec{Port: 9090}))
	if !ok || !strings.HasSuffix(r.upstream, ":9090") {
		t.Errorf("upstream with ingress.port = %q, want the selected port", r.upstream)
	}
}

func TestRouteForHostsAndTLS(t *testing.T) {
	m := testManager(Config{AdvertiseHost: func() string { return "192.168.1.50" }})
	comp := runningComponent("api", []runtime.PortMapping{{ContainerPort: 8080}}, &core.IngressSpec{
		Hosts: []string{"api.example.com"},
	})
	comp.Spec.Domain = "legacy.example.com"

	r, ok := m.routeFor(comp)
	if !ok {
		t.Fatal("expected a route")
	}
	// domain and ingress.hosts are additive: domain is the older one-host
	// shorthand, and dropping either would silently stop routing something.
	for _, want := range []string{"legacy.example.com", "api.localhost", "api.192.168.1.50.nip.io", "api.example.com"} {
		found := false
		for _, h := range r.hostnames {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("hostnames %v missing %q", r.hostnames, want)
		}
	}
}

func TestRouteForTLSCannotBeTurnedOnWithoutAListener(t *testing.T) {
	// The daemon binds 443 only when started with -ingress-tls. A component
	// asking for TLS anyway must not be advertised over https, because
	// nothing is listening.
	yes := true
	m := testManager(Config{TLS: false})
	r, _ := m.routeFor(runningComponent("api", []runtime.PortMapping{{ContainerPort: 8080}}, &core.IngressSpec{TLS: &yes}))
	if r.tls {
		t.Error("component tls:true must not take effect when the daemon has no 443 listener")
	}

	// The other direction is fine: opting out needs no listener.
	no := false
	m = testManager(Config{TLS: true})
	r, _ = m.routeFor(runningComponent("api", []runtime.PortMapping{{ContainerPort: 8080}}, &core.IngressSpec{TLS: &no}))
	if r.tls {
		t.Error("component tls:false must be honoured")
	}
}

func TestRouteOrderPutsSpecificPathsFirst(t *testing.T) {
	// Caddy takes the first matching route. A whole-host route ahead of a
	// /api route on the same host would swallow it, and the /api component
	// would look perfectly healthy while never receiving a request.
	keys := []string{
		routeSortKey("", "", "web"),
		routeSortKey("/api", "", "api"),
		routeSortKey("/api/v2", "", "apiv2"),
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	if sorted[0] != keys[2] {
		t.Errorf("longest path should sort first, got %q", sorted[0])
	}
	if sorted[2] != keys[0] {
		t.Errorf("the catch-all route should sort last, got %q", sorted[2])
	}
}

func TestDesiredConfigPathAndStrip(t *testing.T) {
	m := testManager(Config{HTTPPort: 80})
	cfg := m.desiredConfig([]route{
		{hostnames: []string{"a.localhost"}, upstream: "a:80", path: "/api", stripPath: true},
	})

	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	// A prefix matcher has to cover both the bare prefix and everything under
	// it; Caddy's path matcher is otherwise exact.
	for _, want := range []string{`"/api"`, `"/api/*"`, `"strip_path_prefix":"/api"`} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %s:\n%s", want, got)
		}
	}
}

func TestDesiredConfigIssuerRules(t *testing.T) {
	routes := []route{{
		hostnames: []string{"api.example.com", "api.localhost"},
		upstream:  "api:80",
		tls:       true,
	}}

	// ACME: only what a public CA could issue for is attempted. The rest is
	// skipped so a name that cannot pass a challenge does not burn rate limit
	// shared with the one that can.
	acme := testManager(Config{HTTPPort: 80, TLS: true, TLSIssuer: IssuerACME})
	body, _ := json.Marshal(acme.desiredConfig(routes))
	if !strings.Contains(string(body), `"skip":["api.localhost"]`) {
		t.Errorf("acme should skip api.localhost:\n%s", body)
	}
	if strings.Contains(string(body), `"module":"internal"`) {
		t.Error("acme must not configure the internal issuer")
	}

	// Internal: nothing is unissuable, and .localhost is the whole reason
	// someone would pick this mode.
	internal := testManager(Config{HTTPPort: 80, TLS: true, TLSIssuer: IssuerInternal})
	body, _ = json.Marshal(internal.desiredConfig(routes))
	got := string(body)
	if !strings.Contains(got, `"module":"internal"`) {
		t.Errorf("internal issuer not configured:\n%s", got)
	}
	if !strings.Contains(got, "api.localhost") || strings.Contains(got, `"skip":["api.localhost"]`) {
		t.Errorf("internal issuer should certify api.localhost, not skip it:\n%s", got)
	}
	// http has to keep working alongside https locally.
	if !strings.Contains(got, `"disable_redirects":true`) {
		t.Errorf("internal issuer should not force redirects:\n%s", got)
	}
}

func TestDesiredConfigDedupesSharedHostnames(t *testing.T) {
	// Two components sharing a hostname is the whole point of path routing.
	// Caddy refuses a config that names the same host in two TLS automation
	// policies, and it refuses the *whole* config — so one shared hostname
	// took down routing for every unrelated component too. Found by running
	// it, not by reading it.
	m := testManager(Config{HTTPPort: 80, TLS: true, TLSIssuer: IssuerInternal})
	body, _ := json.Marshal(m.desiredConfig([]route{
		{hostnames: []string{"app.example.com"}, upstream: "back:80", path: "/api", tls: true},
		{hostnames: []string{"app.example.com"}, upstream: "front:80", tls: true},
	}))

	got := string(body)
	if n := strings.Count(got, `"subjects":`); n != 1 {
		t.Fatalf("want a single automation policy, got %d:\n%s", n, got)
	}
	if n := strings.Count(got, `"app.example.com","app.example.com"`); n != 0 {
		t.Errorf("hostname listed twice in one policy:\n%s", got)
	}

	// A hostname wanted by one route and not another must be certified, not
	// both certified and skipped — the two lists have to stay disjoint.
	m2 := testManager(Config{HTTPPort: 80, TLS: true, TLSIssuer: IssuerInternal})
	body2, _ := json.Marshal(m2.desiredConfig([]route{
		{hostnames: []string{"shared.example.com"}, upstream: "a:80", tls: true},
		{hostnames: []string{"shared.example.com"}, upstream: "b:80", tls: false},
	}))
	if strings.Contains(string(body2), `"skip":["shared.example.com"]`) {
		t.Errorf("a hostname certified by one route must not also be skipped:\n%s", body2)
	}
}

func TestDesiredConfigNoTLSKeepsPort443Unbound(t *testing.T) {
	m := testManager(Config{HTTPPort: 80})
	body, _ := json.Marshal(m.desiredConfig([]route{
		{hostnames: []string{"a.localhost"}, upstream: "a:80"},
	}))
	if strings.Contains(string(body), ":443") {
		t.Errorf("no route asked for tls, so 443 should not be listened on:\n%s", body)
	}
}

// A service qooq starts for an app (Ingress.Off) gets no route: a database
// has no business behind a public hostname.
func TestRouteForSkipsAComponentWithIngressOff(t *testing.T) {
	m := testManager(Config{AdvertiseHost: func() string { return "192.168.1.50" }})
	c := runningComponent("api-db", []runtime.PortMapping{{ContainerPort: 5432}}, &core.IngressSpec{Off: true})
	if _, ok := m.routeFor(c); ok {
		t.Error("a component with ingress off was routed")
	}
}

// The proxy is the one component meant to be reached from outside: its public
// ports name 0.0.0.0, so qooqd's -publish-ip 127.0.0.1 (which fills in only
// what a spec leaves empty) cannot bind them to loopback (E1).
func TestProxyPortsAreAlwaysPublic(t *testing.T) {
	for _, tls := range []bool{false, true} {
		for _, p := range proxyPorts(Config{HTTPPort: 80, TLS: tls}) {
			if p.HostIP != "0.0.0.0" {
				t.Errorf("tls=%v: port %d binds %q, want 0.0.0.0", tls, p.HostPort, p.HostIP)
			}
		}
	}
	if n := len(proxyPorts(Config{HTTPPort: 80, TLS: true})); n != 2 {
		t.Errorf("with tls: %d ports, want 80 and 443", n)
	}
}

// BindIP narrows the proxy to one host address — a laptop keeping its apps to
// itself — and must apply to every port it publishes, HTTPS included.
func TestProxyPortsHonourBindIP(t *testing.T) {
	ports := proxyPorts(Config{HTTPPort: 8080, TLS: true, BindIP: "127.0.0.1"})
	if len(ports) != 2 {
		t.Fatalf("%d ports, want 2", len(ports))
	}
	for _, p := range ports {
		if p.HostIP != "127.0.0.1" {
			t.Errorf("port %d binds %q, want 127.0.0.1", p.HostPort, p.HostIP)
		}
	}
}
