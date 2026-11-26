package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/ingress"
	"github.com/qooqapp/qooq/pkg/client"
)

// DetectAdvertiseHost picks the IPv4 address other machines would use to reach
// this host, for composing component endpoints.
//
// It is a heuristic, and on a machine with a VPN, several NICs or an unusual
// routing setup it can pick the wrong one — which is why qooqd logs its choice
// at boot and -advertise-host overrides it. Returning "" is a normal outcome
// (a host with no non-loopback address); callers fall back to localhost, which
// is what they did before this existed.
//
// IPv6 is deliberately out of scope for v1: the endpoints this feeds are
// pasted into browsers and curl commands, where a bare v6 literal needs
// brackets and usually is not what the operator wanted anyway.
func DetectAdvertiseHost() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	type candidate struct {
		ip    string
		rank  int // lower is better
		index int
	}
	var candidates []candidate

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			// Link-local (169.254.0.0/16) means DHCP failed; it is never an
			// address anyone can usefully reach this host on.
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			candidates = append(candidates, candidate{
				ip:    ip.String(),
				rank:  interfaceRank(iface.Name),
				index: iface.Index,
			})
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	// Within a rank, the lowest interface index is the machine's primary NIC
	// on both Linux (eth0/enp*) and macOS (en0).
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return candidates[i].index < candidates[j].index
	})
	return candidates[0].ip
}

// interfaceRank deprioritizes interfaces that have an address but are not how
// the outside world reaches this host: container/VM bridges (whose addresses
// only exist inside this machine) and VPN tunnels (which route to the VPN, not
// to the LAN the operator is testing from).
func interfaceRank(name string) int {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "kube"} {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			return 2
		}
	}
	for _, prefix := range []string{"utun", "tun", "tap", "ppp", "wg"} {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			return 1
		}
	}
	return 0
}

// endpointsFor turns a component's published host ports into ready-to-open
// URLs. Ports with no host mapping are skipped: they are not reachable from
// outside the container network, so listing them would be a lie.
//
// host is qooqd's advertise host; empty falls back to 127.0.0.1 so a daemon
// that could not determine its address still produces something usable on the
// machine it runs on.
//
// The scheme is always http: qooqd terminates nothing and knows nothing about
// the app's TLS. A component's domain is deliberately NOT used here — it is
// metadata until there is an ingress actually routing it, and an endpoint that
// does not answer is worse than no endpoint at all.
func endpointsFor(host string, ports []client.PortSpec) []string {
	if host == "" {
		host = "127.0.0.1"
	}
	var out []string
	for _, p := range ports {
		if p.HostPort != 0 {
			out = append(out, fmt.Sprintf("http://%s:%d", host, p.HostPort))
		}
	}
	return out
}

// ingressEndpoints are the URLs an ingress routes to a component.
//
// They come first in the reported endpoints because they are the addresses
// meant to be used and shared; a published host port still works and is still
// listed, but it is the lower-level way in. tls only affects the scheme of
// hostnames a public CA can issue for — the rest stay http, matching what the
// proxy is actually configured to serve.
//
// The hostnames come from ingress.Assign over all components — the same call
// the proxy builds its routes from — so a name the proxy refuses (a user's
// own domain under -ingress-app-domain, or one an older component already
// holds) is not reported either.
func ingressEndpoints(c *core.Component, advertiseHost string, info ingress.Config, all []*core.Component) []string {
	info = info.WithDefaults()
	present := false
	for _, o := range all {
		if o.ID == c.ID {
			present = true
			break
		}
	}
	if !present {
		all = append(append([]*core.Component(nil), all...), c)
	}
	hosts := ingress.Assign(all, info.AppDomain, advertiseHost).Hosts(c.ID)

	tls := info.TLS
	// A component can opt out of TLS, but it cannot opt in when the daemon
	// bound no 443 — mirroring what the ingress itself decides.
	if c.Spec.Ingress != nil && c.Spec.Ingress.TLS != nil && !(*c.Spec.Ingress.TLS && !info.TLS) {
		tls = *c.Spec.Ingress.TLS
	}
	https := func(h string) string {
		if info.HTTPSPort == 443 {
			return "https://" + h
		}
		return fmt.Sprintf("https://%s:%d", h, info.HTTPSPort)
	}

	// A proxy bound to loopback answers only this machine: the
	// <name>.<ip>.nip.io name, which exists for other devices, leads to a
	// refused connection, so it is not reported (same rule as a loopback host
	// port in endpoints).
	loopbackOnly := false
	if ip := net.ParseIP(info.BindIP); ip != nil && ip.IsLoopback() {
		loopbackOnly = true
	}

	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if loopbackOnly && h != "localhost" && !strings.HasSuffix(h, ".localhost") {
			continue
		}
		// Under the app domain a visitor always arrives over https: the
		// platform's certificate here, or the TLS-terminating proxy in front.
		if ingress.UnderAppDomain(h, info.AppDomain) {
			out = append(out, https(h))
			continue
		}
		// Which scheme a hostname is actually served on depends on who signs:
		// a public CA cannot issue for .localhost, but Caddy's internal CA
		// issues for anything. Reporting https for a name served over http
		// sends the reader to a connection that is refused.
		if tls && (info.TLSIssuer == ingress.IssuerInternal || ingress.IsPublicDomain(h)) {
			out = append(out, https(h))
			continue
		}
		if info.HTTPPort == 80 {
			out = append(out, "http://"+h)
			continue
		}
		out = append(out, fmt.Sprintf("http://%s:%d", h, info.HTTPPort))
	}
	return out
}

// AdvertiseHost holds the address this daemon reports, so it can change while
// the daemon runs.
//
// It used to be resolved once at startup, which is wrong for the machine qooq
// is mostly used on: a laptop that joins a different network keeps handing out
// the address it had at boot. Everything downstream inherits that — the
// endpoints in `component get`, the nip.io hostnames, and ${component.x.
// endpoint} baked into another app's environment at deploy time — and the
// symptom is the worst kind: the container is running, its health probe is
// green, and only a browser sees that nothing answers.
//
// A value the operator gave explicitly is never overwritten. They know
// something the daemon cannot see (a NAT, a tunnel, a name), and quietly
// replacing it with a detected address would be qooq deciding it knows better.
type AdvertiseHost struct {
	v atomic.Value // string
}

// NewAdvertiseHost holds host, which may be empty.
func NewAdvertiseHost(host string) *AdvertiseHost {
	a := &AdvertiseHost{}
	a.v.Store(host)
	return a
}

// Get is safe to call from any goroutine.
func (a *AdvertiseHost) Get() string {
	s, _ := a.v.Load().(string)
	return s
}

// Set replaces the address.
func (a *AdvertiseHost) Set(host string) { a.v.Store(host) }

// Refresh re-detects the address every interval and updates it when it
// changes, until ctx is done. A detection that comes back empty is ignored
// rather than published: a moment with no usable interface (waking up, mid
// reconnect) says nothing about where this machine can be reached, and
// replacing a working address with none would break every endpoint until the
// next tick.
func (a *AdvertiseHost) Refresh(ctx context.Context, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			found := DetectAdvertiseHost()
			if found == "" || found == a.Get() {
				continue
			}
			// Worth a line at info: it explains why a URL handed out ten
			// minutes ago has stopped working, which otherwise looks like a
			// fault in the app.
			log.Info("advertise host changed", "from", a.Get(), "to", found)
			a.Set(found)
		}
	}
}

// staleAddress reports the pair (deployed-with, now) when a component carries
// an address that has since changed, and empty strings otherwise.
//
// The test is not "the advertise host moved" — that would flag every component
// on the machine, including the many that never mention it, and a flag that is
// usually irrelevant is one people learn to skip. It is "this component's own
// configuration names the old address", which is precisely the set that stopped
// working.
func staleAddress(c *core.Component, now string) (deployedWith, current string) {
	was := c.Status.AdvertiseHost
	if was == "" || now == "" || was == now {
		return "", ""
	}
	if !mentionsHost(c, was) {
		return "", ""
	}
	return was, now
}

// mentionsHost looks where a resolved address can have been written: the
// environment (${component.x.endpoint} and anything hand-written) and the
// command line.
func mentionsHost(c *core.Component, host string) bool {
	for _, v := range c.Spec.Env {
		if strings.Contains(v, host) {
			return true
		}
	}
	for _, arg := range c.Spec.Command {
		if strings.Contains(arg, host) {
			return true
		}
	}
	return false
}
