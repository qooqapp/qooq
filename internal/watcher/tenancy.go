package watcher

import (
	"errors"
	"strings"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/tenancy"
)

// Namespace isolation for the things a namespace did not already reach.
//
// Container names (core.ContainerName), ingress hostnames
// (internal/ingress.Hostnames) and mount subtrees (mounts.go) are already scoped
// by namespace. Networks and groups were not, and that was a hole in two
// different ways:
//
//   - A network name of "qooq-<component>" is shared by every namespace with a
//     component of that name, so two stacks each holding an "api" put them on
//     one network. A manifest naming a network explicitly ("network: shared")
//     could join any other namespace's network outright.
//   - A group named after the component collides the same way, so
//     `qooq group stop api` stops every namespace's api at once. That is a bug
//     today, before any shared use: two stacks with a project called "api"
//     already share a group.
//
// The rule follows the one ContainerName and Hostnames already settled on:
// qualify with the namespace, except for the default namespace, which stays
// bare. That keeps every existing single-namespace deployment on exactly the
// names it has now — renaming a running component's network or group is not
// free, and nothing about "default" tells a reader anything.

// namespaceQualified prefixes a shared resource name with its namespace.
//
// Applied only where the name ORIGINATES from something a user controls: a
// manifest's network: key, and the group derived from a component name.
//
// Deliberately NOT applied to a name that arrived in an operator's deploy_spec.
// Two reasons. The operator layer is qooq itself — for a stack, the stack sets
// the network; on a shared node, an external manager does — so there is nothing to fence
// off there. And doing it anyway is actively wrong: a stack's network is already
// "qooq-<namespace>", which does not start with "<namespace>-", so a blanket
// pass would turn it into "acme-prod-qooq-acme-prod".
//
// Idempotent even so, because the same manifest value can be re-derived on a
// later sweep.
func namespaceQualified(namespace, name string) string {
	ns := client.NormalizeNamespace(namespace)
	if ns == client.DefaultNamespace || name == "" {
		return name
	}
	prefix := ns + "-"
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}

// defaultAppNetwork is the per-app network a manifest gets when it does not name
// one: everything this app owns can talk, and nothing else joins by accident.
// Sharing is opt-in via the manifest's network: key.
//
// The namespace is inside the name rather than prefixed onto the whole thing, so
// the result reads as a qooq-owned network ("qooq-acme--prod-api") instead of
// burying the qooq- marker in the middle.
func defaultAppNetwork(namespace, component string) string {
	ns := client.NormalizeNamespace(namespace)
	if ns == client.DefaultNamespace {
		return "qooq-" + component
	}
	return "qooq-" + ns + "-" + component
}

// isTenantNamespace reports whether a namespace is a external-manager account's
// ("<account>--<namespace>"). The watcher does not know accounts; it only
// knows the spelling, which pkg/tenancy defines for both sides.
func isTenantNamespace(namespace string) bool {
	return tenancy.IsTenant(namespace)
}

// groupName is the name of the group holding an app and its services.
//
// Qualified for the same reason as the network, and visible to the operator:
// `qooq group ls` shows it, and two namespaces' groups have to be tellable
// apart before `qooq group stop` means anything.
func groupName(namespace, component string) string {
	return namespaceQualified(namespace, component)
}

// stackNetwork is the single network a stack's projects share.
//
// A stack's name is its namespace, so the namespace alone makes this unique —
// there is no component name to add, and adding one would give each project its
// own network, which is the opposite of what a stack is for.
func stackNetwork(namespace string) string {
	return "qooq-" + namespace
}

// imageRepo is the docker repository name a target's images are tagged under,
// and the prefix pruning uses to decide which images are its own.
//
// Namespaced for a reason that is not merely tidiness. Images live in one flat
// namespace on a shared daemon, and pruneOldImages deletes every tag beginning
// with "<component>:" apart from the current and previous ones. With bare
// component names, two namespaces each holding an "api" share that prefix — so
// one user's deploy deletes the other user's image, and the victim finds out
// when a restart or a reconcile tries to recreate a container from an image that
// is no longer there.
//
// The double hyphen a qualified namespace carries is legal here: docker's
// reference grammar allows runs of hyphens between alphanumerics (verified
// against a real daemon, not inferred from the grammar).
func imageRepo(namespace, component string) string {
	return namespaceQualified(namespace, component)
}

// artifactDirName is imageRepo's equivalent for the process runtime, which
// stores built artifacts on the filesystem instead of as images.
//
// Same collision, same consequence: pruneOldArtifacts keeps the two newest sha
// directories under <artifacts>/<name>, so two namespaces sharing a component
// name delete each other's builds.
func artifactDirName(namespace, component string) string {
	return namespaceQualified(namespace, component)
}

// checkTenantManifestAddress refuses a user's manifest that picks its own
// address: `domain:` or `ingress.hosts:`.
//
// On a shared platform the address of a service is assigned (it arrives as the
// operator's deploy_spec.domain, which this does not touch). A manifest that
// names one could claim somebody else's hostname, collide with another and
// make the proxy refuse the whole configuration, or have the platform ask a
// certificate authority for a name nobody owns. It is refused rather than
// ignored: an ignored `domain:` reads as accepted, and the author goes on to
// debug DNS for a name that was never routed.
//
// Runs before any build, on whichever manifest the target resolved — the
// repo's own, the generated stand-in, or one an operator PUT — and the failure
// is the repository's, so it counts toward giving up like any other.
func checkTenantManifestAddress(t *Target, mf *manifest) error {
	if mf == nil || !isTenantNamespace(namespaceOf(t)) {
		return nil
	}
	if strings.TrimSpace(mf.Domain) != "" {
		return errTenantManifestAddress
	}
	if mf.Ingress != nil {
		for _, h := range mf.Ingress.Hosts {
			if strings.TrimSpace(h) != "" {
				return errTenantManifestAddress
			}
		}
	}
	return nil
}

var errTenantManifestAddress = errors.New("domain and ingress.hosts cannot be set in qooq.yaml here: " +
	"the address of a service is assigned by the platform (custom domains are not supported yet)")
