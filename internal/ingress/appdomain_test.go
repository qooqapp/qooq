package ingress

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

func TestComponentHostnames(t *testing.T) {
	const app = "apps.example.net"
	long := strings.Repeat("a", 64)
	tests := []struct {
		name      string
		appDomain string
		ns        string
		domain    string
		hosts     []string
		want      []string
		problem   bool
	}{
		{name: "no app domain: shared-node user keeps every name", ns: "acme--prod", domain: "evil.example.com", hosts: []string{"x.example.com"},
			want: []string{"evil.example.com", "web.acme--prod.localhost", "web.acme--prod.10.0.0.5.nip.io", "x.example.com"}},
		{name: "app domain, not a shared-node user: unchanged", appDomain: app, ns: "shop", domain: "shop.example.com",
			want: []string{"shop.example.com", "web.shop.localhost", "web.shop.10.0.0.5.nip.io"}},
		{name: "app domain, default namespace: unchanged", appDomain: app, ns: "",
			want: []string{"web.localhost", "web.10.0.0.5.nip.io"}},
		{name: "shared-node user on its assigned name only", appDomain: app, ns: "acme--prod", domain: "blog-k3x9q.apps.example.net", hosts: []string{"x.example.com"},
			want: []string{"blog-k3x9q.apps.example.net"}},
		{name: "shared-node user domain elsewhere", appDomain: app, ns: "acme--prod", domain: "evil.example.com", problem: true},
		{name: "shared-node user domain two levels down", appDomain: app, ns: "acme--prod", domain: "a.b.apps.example.net", problem: true},
		{name: "shared-node user domain is the app domain itself", appDomain: app, ns: "acme--prod", domain: "apps.example.net", problem: true},
		{name: "shared-node user domain on a lookalike", appDomain: app, ns: "acme--prod", domain: "blog.notapps.example.net", problem: true},
		{name: "shared-node user label too long", appDomain: app, ns: "acme--prod", domain: long + ".apps.example.net", problem: true},
		{name: "shared-node user label not lowercase", appDomain: app, ns: "acme--prod", domain: "Blog.apps.example.net", problem: true},
		{name: "shared-node user label starts with a hyphen", appDomain: app, ns: "acme--prod", domain: "-blog.apps.example.net", problem: true},
		{name: "shared-node user without a domain", appDomain: app, ns: "acme--prod", problem: true},
		{name: "shared-node user hosts alone get nothing", appDomain: app, ns: "acme--prod", hosts: []string{"blog-k3x9q.apps.example.net"}, problem: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := core.ComponentSpec{Namespace: tc.ns, Name: "web", Domain: tc.domain}
			if tc.hosts != nil {
				spec.Ingress = &core.IngressSpec{Hosts: tc.hosts}
			}
			got, problem := ComponentHostnames(spec, tc.appDomain, "10.0.0.5")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hosts = %v, want %v", got, tc.want)
			}
			if (problem != "") != tc.problem {
				t.Errorf("problem = %q, want problem=%v", problem, tc.problem)
			}
			if tc.problem && tc.domain != "" && !strings.Contains(problem, tc.domain) {
				t.Errorf("problem %q should name the refused value %q", problem, tc.domain)
			}
		})
	}
}

func claimant(id, ns, name, domain string, created time.Time, ing *core.IngressSpec) *core.Component {
	c := runningComponent(name, []runtime.PortMapping{{ContainerPort: 80}}, ing)
	c.ID, c.CreatedAt = id, created
	c.Spec.Namespace, c.Spec.Domain = ns, domain
	return c
}

func TestAssignGivesAContestedNameToTheOldest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		comps   []*core.Component
		app     string
		winner  string
		loser   string
		host    string
		problem bool
	}{
		{
			name: "older wins although it sorts later by name",
			comps: []*core.Component{
				claimant("B", "zzz", "zed", "shared.example.com", t0.Add(time.Hour), nil),
				claimant("A", "zzz", "yak", "shared.example.com", t0, nil),
			},
			winner: "A", loser: "B", host: "shared.example.com", problem: true,
		},
		{
			name: "same creation time: lower id wins",
			comps: []*core.Component{
				claimant("02", "", "aaa", "shared.example.com", t0, nil),
				claimant("01", "", "bbb", "shared.example.com", t0, nil),
			},
			winner: "01", loser: "02", host: "shared.example.com", problem: true,
		},
		{
			name: "app domain names collide the same way",
			app:  "apps.example.net",
			comps: []*core.Component{
				claimant("new", "bob--prod", "web", "x-12345.apps.example.net", t0.Add(time.Minute), nil),
				claimant("old", "acme--prod", "web", "x-12345.apps.example.net", t0, nil),
			},
			winner: "old", loser: "new", host: "x-12345.apps.example.net", problem: true,
		},
		{
			name: "a stopped older component keeps its name",
			comps: func() []*core.Component {
				old := claimant("old", "", "a", "shared.example.com", t0, nil)
				old.Status.Phase = runtime.PhaseExited
				return []*core.Component{old, claimant("new", "", "b", "shared.example.com", t0.Add(time.Hour), nil)}
			}(),
			winner: "old", loser: "new", host: "shared.example.com", problem: true,
		},
		{
			name: "a removed older component gives it up",
			comps: func() []*core.Component {
				old := claimant("old", "", "a", "shared.example.com", t0, nil)
				old.Status.Phase = runtime.PhaseRemoved
				return []*core.Component{old, claimant("new", "", "b", "shared.example.com", t0.Add(time.Hour), nil)}
			}(),
			winner: "new", loser: "old", host: "shared.example.com",
		},
		{
			name: "an older component with the ingress off does not hold it",
			comps: []*core.Component{
				claimant("old", "", "a", "shared.example.com", t0, &core.IngressSpec{Off: true}),
				claimant("new", "", "b", "shared.example.com", t0.Add(time.Hour), nil),
			},
			winner: "new", loser: "old", host: "shared.example.com",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Assign(tc.comps, tc.app, "")
			if !contains(a.Hosts(tc.winner), tc.host) {
				t.Errorf("%s should hold %s, has %v", tc.winner, tc.host, a.Hosts(tc.winner))
			}
			if contains(a.Hosts(tc.loser), tc.host) {
				t.Errorf("%s should not hold %s, has %v", tc.loser, tc.host, a.Hosts(tc.loser))
			}
			if got := len(a.Problems) > 0; got != tc.problem {
				t.Errorf("problems = %v, want reported=%v", a.Problems, tc.problem)
			}
		})
	}
}

// Path routing shares a hostname on purpose; only the same host and path is
// a collision.
func TestAssignKeepsPathRoutingOnASharedHost(t *testing.T) {
	t0 := time.Now()
	a := Assign([]*core.Component{
		claimant("front", "", "front", "app.example.com", t0, nil),
		claimant("back", "", "back", "app.example.com", t0.Add(time.Second), &core.IngressSpec{Path: "/api"}),
	}, "", "")
	if !contains(a.Hosts("front"), "app.example.com") || !contains(a.Hosts("back"), "app.example.com") {
		t.Errorf("both should route app.example.com: front %v, back %v", a.Hosts("front"), a.Hosts("back"))
	}
	if len(a.Problems) != 0 {
		t.Errorf("no collision expected: %v", a.Problems)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// The sync builds its routes from one Assign over every component, so a
// contested name ends up in exactly one route, the oldest's — and the loser
// is logged once, not every tick.
func TestRoutesResolveCollisionsAcrossComponents(t *testing.T) {
	var logs bytes.Buffer
	m := &Manager{cfg: Config{AppDomain: "apps.example.net"}, log: slog.New(slog.NewTextHandler(&logs, nil))}
	t0 := time.Now()
	comps := []*core.Component{
		claimant("2", "bob--prod", "aaa", "web-abc12.apps.example.net", t0.Add(time.Hour), nil),
		claimant("1", "acme--prod", "zzz", "web-abc12.apps.example.net", t0, nil),
		claimant("3", "acme--prod", "evil", "evil.example.com", t0, nil),
	}
	routes, routed := m.routes(comps)
	if len(routes) != 1 || len(routed) != 1 || routed[0].ID != "1" {
		t.Fatalf("want one route, for the oldest; got %d routes for %v", len(routes), routed)
	}
	if !reflect.DeepEqual(routes[0].hostnames, []string{"web-abc12.apps.example.net"}) {
		t.Errorf("hostnames = %v", routes[0].hostnames)
	}
	first := logs.String()
	for _, want := range []string{"bob--prod/aaa", "acme--prod/evil", "evil.example.com"} {
		if !strings.Contains(first, want) {
			t.Errorf("warning should name %q:\n%s", want, first)
		}
	}
	if n := strings.Count(first, "level=WARN"); n != 2 {
		t.Errorf("want 2 warnings, got %d:\n%s", n, first)
	}
	m.routes(comps)
	if logs.Len() != len(first) {
		t.Errorf("the same problems were logged again on the next tick:\n%s", logs.String()[len(first):])
	}
}

func appDomainConfig(t *testing.T, cfg Config, routes []route) (map[string]any, string) {
	t.Helper()
	m := testManager(cfg)
	c := m.desiredConfig(routes)
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	return generic, string(body)
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func TestDesiredConfigAppDomain(t *testing.T) {
	appRoute := route{hostnames: []string{"web-abc12.apps.example.net"}, upstream: "w:80"}
	localRoute := route{hostnames: []string{"ops.localhost", "ops.example.com"}, upstream: "o:80", tls: true}
	base := Config{HTTPPort: 80, HTTPSPort: 443, AppDomain: "apps.example.net", CertFile: "/c", KeyFile: "/k"}

	tests := []struct {
		name  string
		cfg   func(Config) Config
		check func(t *testing.T, c map[string]any, body string)
	}{
		{
			name: "fixed certificate: loaded, 443 bound, never handed to an issuer",
			cfg:  func(c Config) Config { c.TLS, c.TLSIssuer = true, IssuerACME; return c },
			check: func(t *testing.T, c map[string]any, body string) {
				srv := dig(c, "apps", "http", "servers", "qooq")
				if !contains(toStrings(dig(srv, "listen")), ":443") {
					t.Errorf("443 not bound: %v", dig(srv, "listen"))
				}
				skipCerts := toStrings(dig(srv, "automatic_https", "skip_certificates"))
				for _, h := range []string{"web-abc12.apps.example.net", "*.apps.example.net"} {
					if !contains(skipCerts, h) {
						t.Errorf("%s not in skip_certificates %v", h, skipCerts)
					}
				}
				if contains(toStrings(dig(srv, "automatic_https", "skip")), "web-abc12.apps.example.net") {
					t.Error("an app-domain name must be served on https, not skipped")
				}
				// ops.example.com is a public name with -ingress-tls and acme, so it
				// is the only one Caddy may manage itself.
				if contains(skipCerts, "ops.example.com") {
					t.Error("a non-app-domain name was taken out of certificate management")
				}
				files, _ := dig(c, "apps", "tls", "certificates", "load_files").([]any)
				if len(files) != 1 || dig(files[0], "certificate") != configMountDir+"/"+certFileName || dig(files[0], "key") != configMountDir+"/"+keyFileName {
					t.Errorf("load_files = %v", files)
				}
			},
		},
		{
			name: "internal issuer never certifies an app-domain name",
			cfg:  func(c Config) Config { c.TLS, c.TLSIssuer = true, IssuerInternal; return c },
			check: func(t *testing.T, c map[string]any, body string) {
				policies, _ := dig(c, "apps", "tls", "automation", "policies").([]any)
				for _, p := range policies {
					if contains(toStrings(dig(p, "subjects")), "web-abc12.apps.example.net") {
						t.Errorf("app-domain name in an issuer policy: %s", body)
					}
				}
				if dig(c, "apps", "tls", "certificates") == nil {
					t.Error("load_files lost when the internal issuer is also configured")
				}
			},
		},
		{
			name: "no fixed certificate: app-domain names stay http, still never issued",
			cfg:  func(c Config) Config { c.TLS, c.TLSIssuer, c.CertFile, c.KeyFile = true, IssuerACME, "", ""; return c },
			check: func(t *testing.T, c map[string]any, body string) {
				skip := toStrings(dig(c, "apps", "http", "servers", "qooq", "automatic_https", "skip"))
				if !contains(skip, "web-abc12.apps.example.net") || !contains(skip, "*.apps.example.net") {
					t.Errorf("app-domain names should be skipped without a certificate: %v", skip)
				}
				if strings.Contains(body, "load_files") {
					t.Error("no certificate was given")
				}
			},
		},
		{
			name: "https port other than 443",
			cfg:  func(c Config) Config { c.HTTPSPort = 18443; c.HTTPPort = 18080; return c },
			check: func(t *testing.T, c map[string]any, body string) {
				if !contains(toStrings(dig(c, "apps", "http", "servers", "qooq", "listen")), ":18443") {
					t.Errorf("listen = %v", dig(c, "apps", "http", "servers", "qooq", "listen"))
				}
				if dig(c, "apps", "http", "https_port") != float64(18443) {
					t.Errorf("https_port = %v", dig(c, "apps", "http", "https_port"))
				}
				// Without it Caddy serves TLS on the http listener too.
				if dig(c, "apps", "http", "http_port") != float64(18080) {
					t.Errorf("http_port = %v", dig(c, "apps", "http", "http_port"))
				}
			},
		},
		{
			name: "unknown names get a 404 page, after every real route",
			cfg:  func(c Config) Config { return c },
			check: func(t *testing.T, c map[string]any, body string) {
				routes, _ := dig(c, "apps", "http", "servers", "qooq", "routes").([]any)
				last := routes[len(routes)-1]
				match, _ := dig(last, "match").([]any)
				if !reflect.DeepEqual(toStrings(dig(match[0], "host")), []string{"*.apps.example.net"}) {
					t.Fatalf("last route should be the wildcard: %v", last)
				}
				handle, _ := dig(last, "handle").([]any)
				h := handle[0]
				if dig(h, "status_code") != float64(404) || !strings.Contains(dig(h, "body").(string), "This service does not exist or was removed") ||
					!strings.Contains(dig(h, "body").(string), "這個服務不存在或已移除") {
					t.Errorf("404 handler = %v", h)
				}
				if strings.Contains(body, `"Location"`) {
					t.Error("no redirect was configured")
				}
			},
		},
		{
			name: "the bare app domain redirects when asked",
			cfg:  func(c Config) Config { c.AppDomainRedirect = "https://example.org"; return c },
			check: func(t *testing.T, c map[string]any, body string) {
				routes, _ := dig(c, "apps", "http", "servers", "qooq", "routes").([]any)
				var found any
				for _, r := range routes {
					match, _ := dig(r, "match").([]any)
					if reflect.DeepEqual(toStrings(dig(match[0], "host")), []string{"apps.example.net"}) {
						found = r
					}
				}
				handle, _ := dig(found, "handle").([]any)
				if len(handle) != 1 || dig(handle[0], "status_code") != float64(301) ||
					!reflect.DeepEqual(toStrings(dig(handle[0], "headers", "Location")), []string{"https://example.org"}) {
					t.Errorf("apex route = %v", found)
				}
				if !contains(toStrings(dig(c, "apps", "http", "servers", "qooq", "automatic_https", "skip_certificates")), "apps.example.net") {
					t.Error("the bare domain must not be sent to an issuer either")
				}
			},
		},
		{
			name: "cloudflare: trusted ranges and the client header",
			cfg:  func(c Config) Config { c.Cloudflare = true; return c },
			check: func(t *testing.T, c map[string]any, body string) {
				srv := dig(c, "apps", "http", "servers", "qooq")
				if dig(srv, "trusted_proxies", "source") != "static" {
					t.Errorf("trusted_proxies = %v", dig(srv, "trusted_proxies"))
				}
				ranges := toStrings(dig(srv, "trusted_proxies", "ranges"))
				if !contains(ranges, "173.245.48.0/20") || !contains(ranges, "2400:cb00::/32") || len(ranges) != len(cloudflareRanges) {
					t.Errorf("ranges = %v", ranges)
				}
				if !reflect.DeepEqual(toStrings(dig(srv, "client_ip_headers")), []string{"CF-Connecting-IP"}) {
					t.Errorf("client_ip_headers = %v", dig(srv, "client_ip_headers"))
				}
			},
		},
		{
			name: "no cloudflare: nothing trusted",
			cfg:  func(c Config) Config { return c },
			check: func(t *testing.T, c map[string]any, body string) {
				if strings.Contains(body, "trusted_proxies") || strings.Contains(body, "CF-Connecting-IP") {
					t.Errorf("cloudflare settings without -ingress-cloudflare: %s", body)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, body := appDomainConfig(t, tc.cfg(base), []route{appRoute, localRoute})
			tc.check(t, c, body)
		})
	}
}

func toStrings(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestProxyPortsWithAFixedCertificate(t *testing.T) {
	ports := proxyPorts(Config{HTTPPort: 18080, HTTPSPort: 18443, CertFile: "/c", KeyFile: "/k"})
	want := []runtime.PortMapping{
		{ContainerPort: 18080, HostPort: 18080, HostIP: "0.0.0.0"},
		{ContainerPort: 18443, HostPort: 18443, HostIP: "0.0.0.0"},
	}
	if !reflect.DeepEqual(ports, want) {
		t.Errorf("ports = %+v, want %+v", ports, want)
	}
}

// writeTestCert writes a self-signed certificate and key, returning paths.
func writeTestCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "*.qooq.localhost"},
		DNSNames: []string{"*.qooq.localhost"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o644); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestInstallCertCopiesIntoTheMountedDirectory(t *testing.T) {
	src, state := t.TempDir(), t.TempDir()
	certPath, keyPath := writeTestCert(t, src)
	// A key left world-readable by an earlier version must be tightened.
	if err := os.WriteFile(filepath.Join(state, keyFileName), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installCert(Config{CertFile: certPath, KeyFile: keyPath}, state); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		from, name string
		mode       os.FileMode
	}{{certPath, certFileName, 0o644}, {keyPath, keyFileName, 0o600}} {
		got, err := os.ReadFile(filepath.Join(state, f.name))
		want, _ := os.ReadFile(f.from)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s not copied: %v", f.name, err)
		}
		st, _ := os.Stat(filepath.Join(state, f.name))
		if st.Mode().Perm() != f.mode {
			t.Errorf("%s mode = %v, want %v", f.name, st.Mode().Perm(), f.mode)
		}
	}
	if err := installCert(Config{CertFile: filepath.Join(src, "missing"), KeyFile: keyPath}, state); err == nil {
		t.Error("a missing certificate must fail the deploy")
	}
	if err := installCert(Config{}, t.TempDir()); err != nil {
		t.Errorf("no certificate configured should be a no-op: %v", err)
	}
}

func TestValidateAppDomainSettings(t *testing.T) {
	for _, d := range []string{"apps.example.net", "qooq.localhost", "a-b.example.co.uk"} {
		if err := ValidateAppDomain(d); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
	for _, d := range []string{"", "localhost", "Apps.example.net", "apps.example.net.", ".apps.example.net", "*.apps.example.net", "apps..example.net"} {
		if err := ValidateAppDomain(d); err == nil {
			t.Errorf("%q should be refused", d)
		}
	}
	for _, u := range []string{"https://example.org", "http://x.example/path"} {
		if err := ValidateRedirect(u); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for _, u := range []string{"example.org", "/home", "javascript:alert(1)", "https://"} {
		if err := ValidateRedirect(u); err == nil {
			t.Errorf("%q should be refused", u)
		}
	}

	dir := t.TempDir()
	certPath, keyPath := writeTestCert(t, dir)
	if err := ValidateCertPair(certPath, keyPath); err != nil {
		t.Errorf("valid pair: %v", err)
	}
	if err := ValidateCertPair("", ""); err != nil {
		t.Errorf("no certificate is allowed: %v", err)
	}
	otherCert, _ := writeTestCert(t, t.TempDir())
	for name, pair := range map[string][2]string{
		"cert only":    {certPath, ""},
		"key only":     {"", keyPath},
		"unreadable":   {filepath.Join(dir, "nope"), keyPath},
		"mismatched":   {otherCert, keyPath},
		"key for cert": {keyPath, keyPath},
	} {
		if err := ValidateCertPair(pair[0], pair[1]); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}
