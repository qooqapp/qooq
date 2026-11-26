package main

import (
	"log/slog"
	"net"
	"strings"
)

// warnExposedWithoutAuth says something at startup when qooqd is bound past
// loopback with no bearer token.
//
// Nothing is actually wide open in that state: with auth disabled the router
// mounts a Host-header guard that only accepts the localhost forms of -listen,
// so a request arriving as http://192.168.0.5:7777/ is refused. But silence
// here is wrong in both directions. An operator who binds 0.0.0.0 expecting it
// to work gets 403 from every remote client with nothing explaining why; and
// the obvious way to "fix" that — adding the address to -allowed-hosts —
// removes the last thing standing in front of an API that builds and runs
// arbitrary images. Both cases deserve a line in the log at the moment the
// decision is made, rather than an hour later in someone else's debugging.
func warnExposedWithoutAuth(log *slog.Logger, listenAddr, token string, extraHosts []string) {
	if token != "" || isLoopbackListen(listenAddr) {
		return
	}
	if len(extraHosts) > 0 {
		log.Warn("listening past loopback with no token and a Host allowlist: these hosts reach an API that can build and run arbitrary images, with no authentication",
			"addr", listenAddr, "allowed_hosts", strings.Join(extraHosts, ","),
			"fix", "set -token or QOOQ_TOKEN")
		return
	}
	log.Warn("listening past loopback with no token: remote requests will be refused by the Host-header guard",
		"addr", listenAddr,
		"fix", "set -token or QOOQ_TOKEN to serve them (adding -allowed-hosts instead leaves the API unauthenticated)")
}

// isLoopbackListen reports whether a listen address can only be reached from
// this machine. A missing or empty host (":7777") means every interface, which
// is the case this exists to catch.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Unparseable: let the listener report the real problem rather than
		// guessing about reachability here.
		return true
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// A hostname that is not "localhost" is resolved by the listener, not here;
	// treat it as reachable so the warning errs toward being shown.
	return false
}
