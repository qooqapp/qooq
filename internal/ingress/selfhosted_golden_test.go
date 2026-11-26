package ingress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

// TestSelfHostedConfigIsUnchanged pins the Caddy config a daemon without
// -ingress-app-domain (and the other shared-node flags) renders, byte for byte.
// The golden file was written by the code as it was before the app-domain
// work, so a self-hosted install can only change here on purpose:
// regenerate with QOOQ_UPDATE_GOLDEN=1 and say why in the commit.
func TestSelfHostedConfigIsUnchanged(t *testing.T) {
	yes, no := true, false
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	comp := func(id, ns, name, domain string, ing *core.IngressSpec) *core.Component {
		c := runningComponent(name, []runtime.PortMapping{{ContainerPort: 8080}}, ing)
		c.ID = id
		c.CreatedAt = created
		c.Spec.Namespace = ns
		c.Spec.Domain = domain
		return c
	}
	comps := []*core.Component{
		comp("01", "", "web", "", nil),
		comp("02", "default", "api", "api.example.com", &core.IngressSpec{Path: "/api", StripPath: true}),
		comp("03", "shop", "front", "", &core.IngressSpec{Hosts: []string{"shop.example.com"}, TLS: &yes}),
		comp("04", "acme--prod", "blog", "blog-k3x9q.apps.example.net", &core.IngressSpec{Hosts: []string{"evil.example.com"}}),
		comp("05", "acme--prod", "quiet", "", &core.IngressSpec{TLS: &no}),
		comp("06", "", "db", "", &core.IngressSpec{Off: true}),
	}
	adv := func() string { return "192.168.1.50" }
	configs := []Config{
		{HTTPPort: 80, AdvertiseHost: adv},
		{HTTPPort: 18080, TLS: true, TLSIssuer: IssuerACME, AdvertiseHost: adv},
		{HTTPPort: 80, TLS: true, TLSIssuer: IssuerInternal, AdvertiseHost: adv},
	}

	var out bytes.Buffer
	for i, cfg := range configs {
		m := testManager(cfg)
		var routes []route
		for _, c := range comps {
			if r, ok := m.routeFor(c); ok {
				routes = append(routes, r)
			}
		}
		sort.Slice(routes, func(a, b int) bool { return routes[a].sortKey < routes[b].sortKey })
		body, err := json.MarshalIndent(m.desiredConfig(routes), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		ports, _ := json.Marshal(proxyPorts(cfg))
		fmt.Fprintf(&out, "== config %d\n%s\nports %s\n", i, body, ports)
	}

	path := filepath.Join("testdata", "selfhosted.golden")
	if os.Getenv("QOOQ_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, out.Bytes()) {
		t.Errorf("self-hosted ingress config changed; diff testdata/selfhosted.golden against:\n%s", out.String())
	}
}
