package ingress

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/pkg/tenancy"
)

// cloudflareRanges are the addresses Cloudflare connects to origins from,
// trusted to name the client in CF-Connecting-IP (-ingress-cloudflare).
// Caddy's standard build has no module that keeps this list current, so it is
// written out here. Taken on 2026-11-23 from https://www.cloudflare.com/ips-v4
// and https://www.cloudflare.com/ips-v6; compare against those when updating.
var cloudflareRanges = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
	"2400:cb00::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2405:b500::/32",
	"2405:8100::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

// notFoundPage answers a name under the app domain that routes nowhere. The
// wildcard DNS record makes every such name resolve, and Caddy's default for
// an unmatched request is an empty 200 — which reads as "the app is broken"
// rather than "this address is wrong".
const notFoundPage = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>404</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#333">
<h1>404</h1>
<p>這個服務不存在或已移除。</p>
<p>This service does not exist or was removed.</p>
</body></html>
`

// dedupeSorted sorts names and drops repeats.
func dedupeSorted(names []string) []string {
	sort.Strings(names)
	out := names[:0]
	for i, n := range names {
		if i == 0 || n != names[i-1] {
			out = append(out, n)
		}
	}
	return out
}

// IsTenantNamespace reports whether a namespace belongs to a external-manager
// account ("<account>--<namespace>"). The rule lives in pkg/tenancy so the
// daemon, the watcher and the external manager cannot disagree about what counts.
func IsTenantNamespace(namespace string) bool {
	return tenancy.IsTenant(namespace)
}

// UnderAppDomain reports whether host is exactly one DNS label directly under
// appDomain: "blog-k3x9q.apps.example.net" is, "a.b.apps.example.net" and "apps.example.net"
// itself are not. One level only, because a wildcard certificate covers one
// level and nothing deeper.
func UnderAppDomain(host, appDomain string) bool {
	if appDomain == "" {
		return false
	}
	label, ok := strings.CutSuffix(host, "."+appDomain)
	return ok && validLabel(label)
}

// validLabel is a single lowercase DNS label: 1–63 of [a-z0-9-], neither
// starting nor ending with a hyphen.
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ValidateAppDomain checks an -ingress-app-domain value: two or more
// lowercase DNS labels and nothing else.
func ValidateAppDomain(d string) error {
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return fmt.Errorf("app domain %q needs at least two labels (like apps.example.net)", d)
	}
	for _, l := range labels {
		if !validLabel(l) {
			return fmt.Errorf("app domain %q: %q is not a lowercase DNS label", d, l)
		}
	}
	return nil
}

// ValidateRedirect checks an -ingress-app-domain-redirect value: an absolute
// http(s) URL, since it goes out verbatim in a Location header.
func ValidateRedirect(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("redirect %q is not an absolute http(s) URL", v)
	}
	return nil
}

// ValidateCertPair checks that a certificate and key file can be read and
// belong together. Called at startup so a node that was told to serve a fixed
// certificate refuses to start rather than run without it.
func ValidateCertPair(certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("a certificate needs both -ingress-cert-file and -ingress-key-file, got only one")
	}
	if certFile == "" {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return fmt.Errorf("load ingress certificate: %w", err)
	}
	return nil
}

// ComponentHostnames is every hostname a component asks to be routed on,
// after the app-domain rule. Both the proxy's routes and the API's endpoints
// come from here (through Assign), so the two cannot disagree.
//
// Without an app domain, or for a namespace that is not a user's, it is
// Hostnames plus ingress.hosts, as it always was. With one, a user gets
// exactly one name — its Domain, and only when that is "<label>.<appDomain>"
// — and nothing else: not .localhost, not nip.io, not ingress.hosts. Those
// values arrive from a manifest the user writes, and a limit the sender can
// change is not a limit, so the daemon holds the line even when the watcher
// let something through. problem says why a user got no name.
func ComponentHostnames(spec core.ComponentSpec, appDomain, advertiseHost string) (hosts []string, problem string) {
	if appDomain == "" || !IsTenantNamespace(spec.Namespace) {
		hosts = Hostnames(spec.Namespace, spec.Name, spec.Domain, advertiseHost)
		if spec.Ingress != nil {
			hosts = append(hosts, spec.Ingress.Hosts...)
		}
		return hosts, ""
	}
	if !UnderAppDomain(spec.Domain, appDomain) {
		return nil, fmt.Sprintf("domain %q is not a single label under %s; such a namespace is only routed on the name the platform assigned", spec.Domain, appDomain)
	}
	return []string{spec.Domain}, ""
}

// Problem is a component the hostname rules refused a name to.
type Problem struct {
	Component string
	Message   string
}

// Assignment is the hostnames each component is actually routed on.
type Assignment struct {
	hosts    map[string][]string
	Problems []Problem
}

// Hosts returns the hostnames assigned to the component with this id.
func (a Assignment) Hosts(id string) []string { return a.hosts[id] }

// claimsHostnames reports whether c takes part in routing at all: the proxy
// does not, nor does a removed component, one with the ingress turned off, or
// one with no port to forward to.
func claimsHostnames(c *core.Component) bool {
	return c.Spec.Name != ComponentName &&
		c.Status.Phase != runtime.PhaseRemoved &&
		!(c.Spec.Ingress != nil && c.Spec.Ingress.Off) &&
		len(c.Spec.Ports) > 0
}

// Assign resolves the hostnames of every component together, because one
// component's names depend on the others': when two claim the same hostname
// and path, only the one created first keeps it (ties broken by id so the
// answer never wobbles), and the other is reported.
//
// Caddy accepts a duplicate host in two routes (checked against caddy:2.8)
// and serves whichever route comes first — which here was decided by
// path/namespace/name order, so a newcomer could take over a name that was
// already routed just by sorting earlier. Creation order keeps a name with
// whoever had it. Phase is deliberately not considered: a component that
// restarts must not hand its name to someone else and then take it back.
func Assign(comps []*core.Component, appDomain, advertiseHost string) Assignment {
	ordered := make([]*core.Component, 0, len(comps))
	for _, c := range comps {
		if claimsHostnames(c) {
			ordered = append(ordered, c)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})

	out := Assignment{hosts: make(map[string][]string, len(ordered))}
	// owner maps "host path" to the component holding it. A component naming
	// the same host twice (domain equal to a derived name) keeps both, as it
	// always did; only another component is turned away.
	owner := map[string]*core.Component{}
	for _, c := range ordered {
		ref := c.Spec.Namespace + "/" + c.Spec.Name
		hosts, problem := ComponentHostnames(c.Spec, appDomain, advertiseHost)
		if problem != "" {
			out.Problems = append(out.Problems, Problem{Component: ref, Message: problem})
		}
		path := ""
		if c.Spec.Ingress != nil {
			path = c.Spec.Ingress.Path
		}
		kept := make([]string, 0, len(hosts))
		for _, h := range hosts {
			key := h + " " + path
			if first, taken := owner[key]; taken && first != c {
				out.Problems = append(out.Problems, Problem{
					Component: ref,
					Message: fmt.Sprintf("hostname %q (path %q) is already routed to %s/%s, which was created first",
						h, path, first.Spec.Namespace, first.Spec.Name),
				})
				continue
			}
			owner[key] = c
			kept = append(kept, h)
		}
		out.hosts[c.ID] = kept
	}
	return out
}
