package api

import (
	"strings"
	"sync"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"

	"github.com/qooqapp/qooq/internal/core"
)

func TestEndpointsFor(t *testing.T) {
	tests := []struct {
		name  string
		host  string
		ports []client.PortSpec
		want  []string
	}{
		{
			name:  "published port uses the advertise host",
			host:  "192.168.1.50",
			ports: []client.PortSpec{{ContainerPort: 8080, HostPort: 19000}},
			want:  []string{"http://192.168.1.50:19000"},
		},
		{
			// The whole point of the feature: an unpublished port is not
			// reachable from outside the container network, so reporting a URL
			// for it would send the operator to an address that never answers.
			name:  "unpublished port yields no endpoint",
			host:  "192.168.1.50",
			ports: []client.PortSpec{{ContainerPort: 8080}},
			want:  nil,
		},
		{
			name: "mixed ports keep only the published ones",
			host: "10.0.0.4",
			ports: []client.PortSpec{
				{ContainerPort: 8080, HostPort: 19000},
				{ContainerPort: 9090},
				{ContainerPort: 5432, HostPort: 15432},
			},
			want: []string{"http://10.0.0.4:19000", "http://10.0.0.4:15432"},
		},
		{
			// A daemon that could not determine its address still produces
			// something usable on the machine it runs on.
			name:  "empty host falls back to loopback",
			host:  "",
			ports: []client.PortSpec{{ContainerPort: 80, HostPort: 8080}},
			want:  []string{"http://127.0.0.1:8080"},
		},
		{
			name:  "no ports at all",
			host:  "192.168.1.50",
			ports: nil,
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := endpointsFor(tc.host, tc.ports)
			if len(got) != len(tc.want) {
				t.Fatalf("endpointsFor() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("endpointsFor()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestInterfaceRank(t *testing.T) {
	// Real NICs must outrank tunnels, which must outrank container bridges:
	// a docker bridge address is reachable only from inside this machine, and
	// a VPN address routes to the VPN rather than the LAN the operator is
	// testing from.
	real := []string{"en0", "eth0", "enp0s3", "wlan0"}
	tunnels := []string{"utun0", "tun0", "tap0", "wg0", "ppp0"}
	bridges := []string{"docker0", "br-1a2b3c", "veth1234", "virbr0", "cni0"}

	for _, name := range real {
		if got := interfaceRank(name); got != 0 {
			t.Errorf("interfaceRank(%q) = %d, want 0", name, got)
		}
	}
	for _, name := range tunnels {
		if got := interfaceRank(name); got != 1 {
			t.Errorf("interfaceRank(%q) = %d, want 1", name, got)
		}
	}
	for _, name := range bridges {
		if got := interfaceRank(name); got != 2 {
			t.Errorf("interfaceRank(%q) = %d, want 2", name, got)
		}
	}
}

// TestDetectAdvertiseHost cannot assert a specific address — it depends on the
// machine running the test. It asserts the shape of whatever comes back, since
// returning something unusable (a loopback or link-local address) would be
// worse than returning nothing.
func TestDetectAdvertiseHost(t *testing.T) {
	host := DetectAdvertiseHost()
	if host == "" {
		t.Skip("no non-loopback IPv4 on this machine")
	}
	if strings.HasPrefix(host, "127.") {
		t.Errorf("DetectAdvertiseHost() = %q, must not be loopback", host)
	}
	if strings.HasPrefix(host, "169.254.") {
		t.Errorf("DetectAdvertiseHost() = %q, must not be link-local", host)
	}
	if strings.Contains(host, ":") {
		t.Errorf("DetectAdvertiseHost() = %q, want an IPv4 address", host)
	}
}

// A laptop that joins another network keeps handing out the address it had at
// boot, and every endpoint built from it stops answering — with the container
// still running and its probe still green. So the held value has to be
// replaceable, and safely, since the API reads it while the refresher writes.
func TestAdvertiseHostIsReplaceable(t *testing.T) {
	a := NewAdvertiseHost("192.168.1.51")
	if got := a.Get(); got != "192.168.1.51" {
		t.Fatalf("Get() = %q", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.Get() }()
	}
	a.Set("192.168.1.50")
	wg.Wait()

	if got := a.Get(); got != "192.168.1.50" {
		t.Errorf("Get() = %q after Set, want the new address", got)
	}
}

// An empty holder must not panic: a machine with no usable interface reports
// no advertise host at all, and endpoints fall back to 127.0.0.1.
func TestAdvertiseHostEmptyIsUsable(t *testing.T) {
	if got := NewAdvertiseHost("").Get(); got != "" {
		t.Errorf("Get() = %q, want empty", got)
	}
}

func componentWithEnv(deployedWith string, env map[string]string) *core.Component {
	return &core.Component{
		Spec:   core.ComponentSpec{Name: "web", Namespace: "default", Env: env},
		Status: core.ComponentStatus{AdvertiseHost: deployedWith},
	}
}

// The case this exists for: a reference resolved at deploy time still names an
// address the machine has since lost. Nothing else says so — the container
// runs, the probe passes, and only a browser finds out.
func TestStaleAddressFlagsAComponentThatCarriesTheOldOne(t *testing.T) {
	c := componentWithEnv("192.168.1.51", map[string]string{
		"API_BASE": "http://192.168.1.51:19000/apis/v1",
	})
	was, now := staleAddress(c, "192.168.1.50")
	if was != "192.168.1.51" || now != "192.168.1.50" {
		t.Errorf("staleAddress() = (%q, %q), want the pair", was, now)
	}
}

// The false positive that would sink the feature: most components never
// mention the advertise host, and flagging all of them after any network
// change teaches people to ignore the mark — which then also hides the ones
// that matter.
func TestStaleAddressIgnoresComponentsThatNeverNamedIt(t *testing.T) {
	c := componentWithEnv("192.168.1.51", map[string]string{
		"DB_HOST": "postgres",
		"MODE":    "production",
	})
	if was, _ := staleAddress(c, "192.168.1.50"); was != "" {
		t.Errorf("flagged a component whose config does not mention the address: %q", was)
	}
}

func TestStaleAddressQuietWhenNothingChanged(t *testing.T) {
	env := map[string]string{"API_BASE": "http://192.168.1.50:19000"}
	if was, _ := staleAddress(componentWithEnv("192.168.1.50", env), "192.168.1.50"); was != "" {
		t.Errorf("flagged an address that has not moved: %q", was)
	}
	// A component deployed before the address was recorded carries none, and
	// there is nothing to compare it against.
	if was, _ := staleAddress(componentWithEnv("", env), "192.168.1.50"); was != "" {
		t.Errorf("flagged a component with no recorded address: %q", was)
	}
	// A daemon that cannot detect an address knows nothing about staleness.
	if was, _ := staleAddress(componentWithEnv("192.168.1.51", env), ""); was != "" {
		t.Errorf("flagged while the current address is unknown: %q", was)
	}
}

// A command line can carry a resolved address just as an environment variable
// can.
func TestStaleAddressLooksAtTheCommandToo(t *testing.T) {
	c := componentWithEnv("192.168.1.51", nil)
	c.Spec.Command = []string{"serve", "--public-url=http://192.168.1.51:8080"}
	if was, _ := staleAddress(c, "192.168.1.50"); was == "" {
		t.Error("an address named in the command was not noticed")
	}
}
