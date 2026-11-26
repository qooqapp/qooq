package watcher

import (
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// Build isolation — the last of the three shared-node blockers.
//
// The assumption everything else in this package was written under is that
// creating a target means trusting the repository behind it: it is the
// operator's own code, and running it is the point. On a shared node the repository
// belongs to another user, and that assumption becomes "any registrant may run
// arbitrary code on the host".
//
// Two paths make that literal, and both were open:
//
//  1. build.command runs on the host with the watcher's own environment. A
//     qooq.yaml of `build: {command: ["sh","-c","env"]}` read
//     QOOQ_WATCHER_SECRET_KEY — the passphrase that decrypts EVERY user's
//     git credentials and secrets — along with every QOOQ_SECRET_*. That
//     defeats the whole of M3: the interpolation layer stopped handing the
//     environment out, and the build layer handed out the same thing wholesale.
//  2. mounts.policy defaults to "open", so a manifest can bind any host path
//     into its container — including the watcher's own sqlite.
//
// The Dockerfile path is not in this list: BuildOptions carries no environment
// and no build args, and docker sandboxes the build itself.

// qooqEnvPrefix marks the environment variables that belong to qooq rather than
// to the machine.
const qooqEnvPrefix = "QOOQ_"

// buildCommandEnv is the environment a build.command runs with.
//
// Everything is passed through EXCEPT qooq's own variables, always, in every
// mode. No legitimate build needs the passphrase that decrypts other people's
// credentials, so there is nothing to weigh: this is not a trade-off between
// security and capability, it is removing something that was never useful.
//
// The rest of the environment is still inherited, because a build genuinely may
// need it (a toolchain's cache directory, a proxy setting, the user's own
// registry token) and stripping it to a fixed allowlist would break builds for
// every toolchain nobody thought of. That inheritance is exactly why
// build.command is refused outright for untrusted repositories — see
// checkBuildCommandAllowed. Filtering is defence in depth for the trusted case,
// not what makes the untrusted case safe.
func buildCommandEnv() []string {
	all := os.Environ()
	out := make([]string, 0, len(all))
	for _, kv := range all {
		if strings.HasPrefix(kv, qooqEnvPrefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// errUntrustedBuildCommand is returned instead of running a repository's own
// command on the host.
//
// Refused rather than sandboxed, deliberately. build.command exists for the
// process runtime — the case where the operator does not want docker — so
// "run it in a container" would require the very thing its users are avoiding,
// and a partial sandbox would be worse than a clear no: it would read as safety
// while a determined manifest walked around it.
//
// Nothing is lost for the shared-node case that was not already missing:
// process-runtime artifacts have no cross-machine delivery story either, so a shared-node
// deployment was never going to serve them. Untrusted repositories build through a Dockerfile
// or a template, which docker sandboxes.
func errUntrustedBuildCommand() error {
	return fmt.Errorf(
		"build.command is not available on this watcher: it runs this repository's own "+
			"command directly on the host, which cannot be made safe for a repository the "+
			"operator does not control. Build with a Dockerfile or build.template instead "+
			"(set %s=false only if every repository this watcher builds is trusted)",
		"QOOQ_WATCHER_UNTRUSTED_REPOS")
}

// checkBuildCommandAllowed refuses build.command when repositories are not
// trusted.
func (w *Watcher) checkBuildCommandAllowed() error {
	if w.untrustedRepos {
		return errUntrustedBuildCommand()
	}
	return nil
}

// effectiveMountsPolicy narrows the configured policy when repositories are not
// trusted.
//
// "open" lets a manifest bind any host path into its container, which for an
// untrusted repository is a second way to read the watcher's database and a
// first way to write anywhere the watcher can. Narrowed rather than rejected at
// startup so an operator turning on untrusted_repos does not also have to
// discover and change a second setting — a two-step secure configuration is one
// step away from being half-done.
func (w *Watcher) effectiveMountsPolicy(configured mountsPolicy) mountsPolicy {
	if !w.untrustedRepos {
		return configured
	}
	if configured.raw == mountsPolicyOpen {
		return mountsPolicy{raw: mountsPolicyRepoOnly}
	}
	return configured
}

// checkRepoURLAllowed refuses a repository on the watcher's own disk when
// repositories are not trusted.
//
// A file:// URL or a bare path is how the offline product clones a local bare
// repo, and there it is the operator's own. Given by a stranger it is a way to
// clone, build and read back whatever git repository the watcher's user can
// open — another user's checkout, qooq's own source. Decided by go-git's own
// endpoint parser, the one the clone uses, so "is this local?" has the same
// answer here as at clone time (scp-style host:path is remote, a path is not).
//
// Defence in depth: the external manager already admits only https URLs on public
// hosts. Network destinations are not judged here.
func (w *Watcher) checkRepoURLAllowed(raw string) error {
	if !w.untrustedRepos {
		return nil
	}
	ep, err := transport.NewEndpoint(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("repo_url: %w", err)
	}
	if ep.Protocol == "file" {
		return fmt.Errorf("repo_url %q is on this machine's disk, which this watcher does not clone from "+
			"(set %s=false only if every repository this watcher builds is trusted)",
			raw, "QOOQ_WATCHER_UNTRUSTED_REPOS")
	}
	return nil
}
