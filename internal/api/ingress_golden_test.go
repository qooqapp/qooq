package api

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/ingress"
	"github.com/qooqapp/qooq/internal/runtime"
)

// TestSelfHostedEndpointsAreUnchanged pins the ingress endpoints a daemon
// without -ingress-app-domain reports. The golden file was written by the
// code before the app-domain work; regenerate with QOOQ_UPDATE_GOLDEN=1 only
// on purpose.
func TestSelfHostedEndpointsAreUnchanged(t *testing.T) {
	yes, no := true, false
	comp := func(ns, name, domain string, ing *core.IngressSpec) *core.Component {
		return &core.Component{
			ID: ns + "/" + name,
			Spec: core.ComponentSpec{
				Namespace: ns, Name: name, Domain: domain, Ingress: ing,
				Ports: []runtime.PortMapping{{ContainerPort: 8080}},
			},
			Status: core.ComponentStatus{Phase: runtime.PhaseRunning},
		}
	}
	comps := []*core.Component{
		comp("", "web", "", nil),
		comp("default", "api", "api.example.com", nil),
		comp("shop", "front", "", &core.IngressSpec{Hosts: []string{"shop.example.com"}, TLS: &yes}),
		comp("acme--prod", "blog", "blog-k3x9q.apps.example.net", &core.IngressSpec{Hosts: []string{"evil.example.com"}}),
		comp("acme--prod", "quiet", "", &core.IngressSpec{TLS: &no}),
	}
	infos := []ingress.Config{
		{HTTPPort: 80},
		{HTTPPort: 18080, TLS: true, TLSIssuer: ingress.IssuerACME},
		{HTTPPort: 80, TLS: true, TLSIssuer: ingress.IssuerInternal},
	}
	var out bytes.Buffer
	for i, info := range infos {
		for _, c := range comps {
			fmt.Fprintf(&out, "%d %s %v\n", i, c.ID, ingressEndpoints(c, "192.168.1.50", info, comps))
		}
	}

	path := filepath.Join("testdata", "selfhosted-endpoints.golden")
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
		t.Errorf("self-hosted endpoints changed; diff testdata/selfhosted-endpoints.golden against:\n%s", out.String())
	}
}
