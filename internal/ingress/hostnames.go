// Package ingress runs a reverse proxy in front of qooq's components so an
// app is reachable at a hostname instead of a host port.
//
// It deliberately does not live in internal/runtime: routing by hostname is
// not an engine primitive, and runtime.Runtime is the engine-neutral boundary.
// The proxy is an ordinary qooq component, so it inherits restart policy,
// logs and reconciliation rather than reimplementing them.
package ingress

import (
	"net"
	"strings"

	"github.com/qooqapp/qooq/pkg/client"
)

// LocalSuffix is appended to a component's name for its always-available
// local hostname. Browsers resolve anything under .localhost to the loopback
// address without any DNS or hosts-file setup (RFC 6761), which is what makes
// this work on a fresh machine with nothing configured.
const LocalSuffix = ".localhost"

// wildcardDNSSuffix turns an IP address into a hostname: api.192.168.1.50
// .nip.io resolves to 192.168.1.50 from any device, so a phone on the same
// network can open the app by name without touching its hosts file.
//
// It depends on a third party's DNS being up, which is why it is only ever an
// *additional* hostname: the .localhost form and any real domain keep working
// if nip.io does not.
const wildcardDNSSuffix = ".nip.io"

// Hostnames lists every name the ingress should route to a component.
//
// namespace scopes the name (two stacks may each own an "api"), and is
// rendered as a dotted label rather than reusing the container name's
// "<ns>_<name>": an underscore is not valid in a hostname, so browsers and
// DNS would reject it.
//
// advertiseHost contributes the wildcard-DNS form only when it is an IPv4
// literal — for a hostname (a real machine name or a domain) that form would
// be meaningless, and for IPv6 nip.io's dashed encoding is a footgun not worth
// the surface. domain, when set, is the operator's own hostname and is listed
// first: it is the one they will actually publish.
func Hostnames(namespace, name, domain, advertiseHost string) []string {
	// The default namespace is left out of the hostname. Every component was
	// reachable at <name>.localhost before namespaces were stored, and putting
	// "default" into those names would change every URL already in use to say
	// something the reader learns nothing from.
	label := name
	if namespace != "" && namespace != client.DefaultNamespace {
		label = name + "." + namespace
	}

	var out []string
	if domain != "" {
		out = append(out, domain)
	}
	out = append(out, label+LocalSuffix)
	if ip := net.ParseIP(advertiseHost); ip != nil && ip.To4() != nil {
		out = append(out, label+"."+advertiseHost+wildcardDNSSuffix)
	}
	return out
}

// IsPublicDomain reports whether a hostname is one a public certificate
// authority could ever issue for.
//
// This gates automatic HTTPS. Asking Let's Encrypt for "api.localhost" or for
// a nip.io name pointing at a private address cannot succeed, and the failed
// challenges count against a per-account rate limit that is shared with the
// names that *would* have worked. Refusing to try is what keeps one
// misconfigured component from taking TLS down for every other one.
func IsPublicDomain(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	// A bare IP is never certifiable by a public CA in practice.
	if net.ParseIP(host) != nil {
		return false
	}
	if strings.HasSuffix(host, LocalSuffix) || host == "localhost" {
		return false
	}
	if strings.HasSuffix(host, wildcardDNSSuffix) {
		return false
	}
	// Reserved for testing and documentation (RFC 2606/6761); a public CA
	// will not issue for these either.
	for _, suffix := range []string{".local", ".test", ".example", ".invalid", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	// Anything left must at least be a dotted name to be a public domain.
	return strings.Contains(host, ".")
}
