package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/ingress"
	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/pkg/client"
)

func TestIngressEndpointsUnderAnAppDomain(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	comp := func(id, ns, name, domain string, created time.Time, ing *core.IngressSpec) *core.Component {
		return &core.Component{
			ID: id, CreatedAt: created,
			Spec: core.ComponentSpec{
				Namespace: ns, Name: name, Domain: domain, Ingress: ing,
				Ports: []runtime.PortMapping{{ContainerPort: 80}},
			},
			Status: core.ComponentStatus{Phase: runtime.PhaseRunning},
		}
	}
	no := false
	sharedUser := comp("t", "acme--prod", "blog", "blog-k3x9q.apps.example.net", t0, &core.IngressSpec{Hosts: []string{"x.example.com"}, TLS: &no})
	evil := comp("e", "acme--prod", "evil", "evil.example.com", t0, nil)
	late := comp("l", "bob--prod", "blog", "blog-k3x9q.apps.example.net", t0.Add(time.Hour), nil)
	ops := comp("o", "", "ops", "", t0, nil)
	all := []*core.Component{sharedUser, evil, late, ops}

	tests := []struct {
		name string
		c    *core.Component
		info ingress.Config
		want []string
	}{
		{"shared-node user: its assigned name, https, even with tls off and no listener", sharedUser,
			ingress.Config{HTTPPort: 80, AppDomain: "apps.example.net"}, []string{"https://blog-k3x9q.apps.example.net"}},
		{"https port other than 443 is in the URL", sharedUser,
			ingress.Config{HTTPPort: 18080, HTTPSPort: 18443, AppDomain: "apps.example.net"}, []string{"https://blog-k3x9q.apps.example.net:18443"}},
		{"shared-node user asking for someone else's domain: nothing", evil,
			ingress.Config{HTTPPort: 80, AppDomain: "apps.example.net"}, []string{}},
		{"the later of two claimants: nothing", late,
			ingress.Config{HTTPPort: 80, AppDomain: "apps.example.net"}, []string{}},
		{"not a shared-node user: unchanged", ops,
			ingress.Config{HTTPPort: 80, AppDomain: "apps.example.net"}, []string{"http://ops.localhost", "http://ops.10.0.0.5.nip.io"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ingressEndpoints(tc.c, "10.0.0.5", tc.info, all)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("endpoints = %v, want %v", got, tc.want)
			}
		})
	}
}

// Through the handlers: a single GET has to look the other components up to
// know a name is taken, and a list must reach the same answer.
func TestEndpointsAgreeBetweenGetAndList(t *testing.T) {
	svc := newTestService(t, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewRouter(svc, log, "", "127.0.0.1:7777", nil, NewAdvertiseHost("10.0.0.5"),
		&ingress.Config{HTTPPort: 18080, HTTPSPort: 18443, AppDomain: "qooq.localhost"})

	deploy := func(ns, name, domain string) string {
		t.Helper()
		rec := serve(h, jsonReq(t, http.MethodPost, "/v1/components/", client.DeployRequest{
			Namespace: ns, Name: name, Image: "nginx:alpine", Domain: domain,
			Ports: []client.PortSpec{{ContainerPort: 80}},
		}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("deploy %s: %d %s", name, rec.Code, rec.Body.String())
		}
		// CreatedAt has to differ for the older one to win on time, not id.
		time.Sleep(5 * time.Millisecond)
		return decodeComponent(t, rec).ID
	}
	web := deploy("acme--prod", "web", "web-abc12.qooq.localhost")
	evil := deploy("acme--prod", "evil", "evil.example.com")
	late := deploy("bob--prod", "web", "web-abc12.qooq.localhost")
	want := map[string][]string{
		web:  {"https://web-abc12.qooq.localhost:18443"},
		evil: {},
		late: {},
	}

	for id, w := range want {
		got := decodeComponent(t, serve(h, jsonReq(t, http.MethodGet, "/v1/components/"+id, nil))).Endpoints
		if !reflect.DeepEqual(nonNil(got), w) {
			t.Errorf("GET %s endpoints = %v, want %v", id, got, w)
		}
	}
	rec := serve(h, jsonReq(t, http.MethodGet, "/v1/components/", nil))
	var list []client.ComponentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if !reflect.DeepEqual(nonNil(c.Endpoints), want[c.ID]) {
			t.Errorf("list %s endpoints = %v, want %v", c.ID, c.Endpoints, want[c.ID])
		}
	}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// A proxy bound to loopback (`qooq config set access local`) answers only this
// machine, so the nip.io name meant for other devices is not reported: it
// would be an address that refuses the connection.
func TestIngressEndpointsWhenTheProxyIsLoopbackOnly(t *testing.T) {
	c := &core.Component{
		ID: "a",
		Spec: core.ComponentSpec{Name: "web",
			Ports: []runtime.PortMapping{{ContainerPort: 80}}},
		Status: core.ComponentStatus{Phase: runtime.PhaseRunning},
	}
	all := []*core.Component{c}
	lan := ingressEndpoints(c, "192.168.1.20", ingress.Config{HTTPPort: 8080}, all)
	if len(lan) != 2 || lan[0] != "http://web.localhost:8080" || lan[1] != "http://web.192.168.1.20.nip.io:8080" {
		t.Fatalf("lan: %v, want the localhost and the nip.io name", lan)
	}
	local := ingressEndpoints(c, "192.168.1.20", ingress.Config{HTTPPort: 8080, BindIP: "127.0.0.1"}, all)
	if len(local) != 1 || local[0] != "http://web.localhost:8080" {
		t.Fatalf("loopback-only: %v, want only http://web.localhost:8080", local)
	}
}
