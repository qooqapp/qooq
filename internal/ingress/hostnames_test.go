package ingress

import (
	"strings"
	"testing"
)

func TestHostnames(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		component     string
		domain        string
		advertiseHost string
		want          []string
	}{
		{
			name:          "plain component on an IPv4 host",
			component:     "api",
			advertiseHost: "192.168.1.50",
			want:          []string{"api.localhost", "api.192.168.1.50.nip.io"},
		},
		{
			// The operator's own domain leads: it is the one they publish.
			name:          "declared domain comes first",
			component:     "api",
			domain:        "api.example.com",
			advertiseHost: "192.168.1.50",
			want:          []string{"api.example.com", "api.localhost", "api.192.168.1.50.nip.io"},
		},
		{
			// A namespace becomes a dotted label. Reusing the container name's
			// "<ns>_<name>" would produce an underscore, which is not valid in
			// a hostname.
			name:          "namespace becomes a dotted label",
			namespace:     "acme",
			component:     "api",
			advertiseHost: "10.0.0.4",
			want:          []string{"api.acme.localhost", "api.acme.10.0.0.4.nip.io"},
		},
		{
			// nip.io encodes an address; with a hostname there is nothing to
			// encode, so that form is simply not offered.
			name:          "non-IP advertise host yields no wildcard-DNS name",
			component:     "api",
			advertiseHost: "build-server",
			want:          []string{"api.localhost"},
		},
		{
			name:          "no advertise host at all",
			component:     "api",
			advertiseHost: "",
			want:          []string{"api.localhost"},
		},
		{
			// IPv6 is skipped: nip.io's dashed encoding is a footgun and the
			// endpoints this feeds get pasted into browsers.
			name:          "IPv6 advertise host yields no wildcard-DNS name",
			component:     "api",
			advertiseHost: "fe80::1",
			want:          []string{"api.localhost"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Hostnames(tc.namespace, tc.component, tc.domain, tc.advertiseHost)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Hostnames() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsPublicDomain(t *testing.T) {
	// Getting this wrong in the permissive direction is the expensive one: a
	// name that cannot pass an ACME challenge still gets tried, and the
	// failures count against a rate limit shared with the names that would
	// have worked.
	public := []string{
		"api.example.com",
		"qooq.app",
		"a.b.c.example.org",
		"API.EXAMPLE.COM",
		"api.example.com.", // trailing dot is still the same name
	}
	notPublic := []string{
		"api.localhost",
		"localhost",
		"api.192.168.1.50.nip.io",
		"192.168.1.50",
		"::1",
		"api.local",
		"api.test",
		"api.internal",
		"nodots",
		"",
	}

	for _, h := range public {
		if !IsPublicDomain(h) {
			t.Errorf("IsPublicDomain(%q) = false, want true", h)
		}
	}
	for _, h := range notPublic {
		if IsPublicDomain(h) {
			t.Errorf("IsPublicDomain(%q) = true, want false", h)
		}
	}
}
