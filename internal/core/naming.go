package core

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/qooqapp/qooq/pkg/client"
)

// namespaceSeparator joins a namespace to a component name to form the
// engine-level container name. "_" is used rather than "-" so the join is
// unambiguous: namespaces and namespaced component names are restricted to
// DNS-label characters (which exclude "_"), so "a_b-c" can only ever have come
// from namespace "a" and name "b-c".
//
// Nothing ever parses the joined form back apart — the namespace is a stored
// field, and orphan adoption reads the whole spec out of the qooq.spec label.
// The separator only has to keep container names unique and readable.
const namespaceSeparator = "_"

// Every component has a namespace; specs reaching this file have already had
// client.NormalizeNamespace applied, so Namespace is never empty. The default
// one is spelled client.DefaultNamespace and is a stored value like any other.
//
// ErrInvalidName is returned when a component's namespace or name
// is not a DNS label. The restriction exists because the component name
// becomes a DNS alias on its network: that is how a sibling addresses it, so
// it has to be resolvable.
var ErrInvalidName = errors.New("namespace and name must be lowercase DNS labels (a-z, 0-9, -), 1-63 chars, not starting or ending with -")

// dnsLabel is the character set a name must satisfy to work as a DNS alias.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validateNaming checks a spec's namespace/name pair.
//
// The charset rule applies to every component, including those in the default
// namespace. It used to exempt them, back when "no namespace" meant "predates
// namespaces and was never held to any rule beyond what docker accepts" — but
// the reason for the rule was never the namespace. A component name becomes a
// DNS alias on its network whatever namespace it is in, so a name that is not
// a DNS label is already unreachable by the siblings meant to call it.
func validateNaming(spec ComponentSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("%w (name is empty)", ErrInvalidName)
	}
	if !dnsLabel.MatchString(spec.Namespace) {
		return fmt.Errorf("%w (namespace %q)", ErrInvalidName, spec.Namespace)
	}
	if !dnsLabel.MatchString(spec.Name) {
		return fmt.Errorf("%w (name %q)", ErrInvalidName, spec.Name)
	}
	return nil
}

// ContainerName is the engine-level name: globally unique across the daemon,
// which is what docker requires.
//
// The default namespace does not qualify the name, for two reasons. It does not
// need to — a name is unique within its namespace, and no other namespace can
// produce a bare name, since a qualified one always contains the separator that
// a DNS label cannot. And qualifying it would rename every container already
// running: the rename itself is only a redeploy, but the ingress alias attached
// to the old name is not, because reconnecting an endpoint that already exists
// is a no-op the engine reports as a conflict, leaving routes pointing at a
// name nothing answers to.
func ContainerName(spec ComponentSpec) string {
	if spec.Namespace == client.DefaultNamespace {
		return spec.Name
	}
	return spec.Namespace + namespaceSeparator + spec.Name
}

// networkAliases are the names this container answers to on its network.
//
// The short name comes first and is the one that matters: a namespace has its
// own network, so siblings can address each other by the bare component name.
// That is what lets the same repo deploy into any namespace without editing a
// single URL — the alternative, addressing by the qualified name, would bake
// the namespace into every app's configuration.
//
// The qualified name is added as a second alias purely so that a human
// exec'ing into an unfamiliar network can reach a container by the name they
// see in `component ls`.
func networkAliases(spec ComponentSpec) []string {
	short := spec.Name
	qualified := ContainerName(spec)
	if qualified == short {
		return []string{short}
	}
	return []string{short, qualified}
}

// sameIdentity reports whether two specs claim the same (namespace, name).
// Uniqueness is scoped to the namespace: two stacks may each have a component
// called "api" without colliding, since they live on separate networks and
// their container names differ.
func sameIdentity(a, b ComponentSpec) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name
}

// displayName is how a component is identified to a human: always "ns/name",
// borrowing kubectl's convention (which reads better than the container's
// "ns_name"). Always qualified, even in the default namespace, because a bare
// name stops identifying anything the moment a second namespace takes it.
func displayName(spec ComponentSpec) string {
	return client.NormalizeNamespace(spec.Namespace) + "/" + spec.Name
}
